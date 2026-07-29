# Design: OpenAI-compatible frontend for llm-d-async

## Motivation

Users are adopting llm-d-async for request-level queueing, and some deployments route all of their traffic through it. Today the only way in is hand-building the internal JSON envelope and speaking the broker protocol directly, and the only way out is draining a shared result list. That blocks the actual usage: standard OpenAI clients submitting requests and getting responses back.

The goal is an OpenAI API compatible frontend that acts as a complete swap-in for the llm-d-router client: point an OpenAI SDK's base_url at it and it behaves like a normal inference endpoint. Every request gets the AP system's tenant, quota, and priority labeling. Callers choose per request whether they also get the broker's queueing and retries (enqueue and wait modes) or a labeled direct proxy to the gateway (the default as shipped).

Why this solution and not alternatives: embedding HTTP intake in the AP breaks its queues-only contract and couples frontend scaling to consumer scaling. Client SDK work does not help users of stock OpenAI clients in arbitrary languages. Pointing clients at the gateway directly gives up durable queueing, tenant quota accounting, deadlines, and retries. A thin frontend that is just another producer and result consumer preserves the AP architecture untouched.

## Scope

1. Frontend accepts `POST /v1/chat/completions`, `/v1/completions`, generic `/v1/*` passthrough, and `GET /v1/models` (served from config).
2. Four ways requests are served, all with identical labeling (tenant, tier, classification, objectives, fairness id) so rank and share are mode independent:
   - Mode 1, raw AP: publish to the broker directly as today, no frontend involved. Existing users unchanged.
   - Mode 2, OpenAI enqueue: POST to the frontend, which enqueues onto Redis with a per-request result key and immediately returns 202 with the request id. Results are fetched later via `GET /v1/requests/{id}` (200 with the upstream body, pending response if not ready, 410 after TTL expiry). Any replica can serve the fetch since results are addressed by request, not replica.
   - Mode 3, OpenAI wait: mode 2 plus a held connection that wakes when the result lands, up to a server-capped wait. On timeout it returns the mode 2 style 202 response and the client falls back to fetching. Wake-up is driven by Redis keyspace notifications multiplexed over one pub/sub connection per replica, so the only replica subscribed to a request's channel is the one holding its connection (mechanics under Architecture). Deployments without keyspace notifications fall back to polling.
   - Mode 4, OpenAI direct: the frontend labels the request (quota classification from the shared Redis counters, objective and fairness headers) and proxies straight to the gateway, skipping AP queues and workers. Streaming passes through natively. Requests select a mode with the `X-AP-Mode` header, and requests without one get the configured `defaultMode`: direct when unset, so a stock SDK behaves like the router, or wait or enqueue for deployments that want all traffic through the broker. An unrecognized `defaultMode` value fails startup rather than silently falling back.
   - Lane parity across modes: reserved vs overflow is a counter concept, not a queue concept. The frontend runs the same quota classification against the same Redis counters at admission (increment on admit, decrement on completion), so all modes draw from one quota account per tenant. Tier comes from tenant/endpoint config using the same tier vocabulary. There is no mode-specific tier: mode choice must be priority-neutral, or the direct path becomes self-service priority escalation. Overflow on the direct path carries its lower-priority objective, so under load the gateway may shed it as 429 rather than park it, matching what a connection-holding caller can tolerate.
3. Backend: Redis sorted-set only for the first release (no Pub/Sub work). It has deadline-ordered dispatch, the per-message result queue routing that per-request result keys rely on, and existing producer SDK support.
4. Streaming (`stream: true`) on the queued modes is rejected with a clear 400 in v1. Streaming callers use the direct mode.
5. Tracking parity contract for the direct path: both direct and non-direct paths update and read Redis quota counters independently. Any new signal the AP derives from its request path (for example HTTP response codes feeding a saturation detector) must be mirrored on the direct path, so the AP's view of downstream health includes the traffic that bypassed its workers. Currently that parity is achieved by both sides reading and writing shared keys, but future request artifacts may require the frontend to communicate directly with AP proper.

## Architecture

New Go module `frontend/` with binary `cmd/frontend/`, deployed as its own Deployment via the chart (`frontend.enabled=true`), horizontally scalable.

Request path per call:

1. Validate minimally: well-formed JSON, `model` present, size cap, `stream` allowed only in direct mode. Payload stays opaque beyond that.
2. Resolve the tenant (configurable header, for example `X-Team`), run quota classification against the shared Redis counters, and select the mode from the `X-AP-Mode` header, falling back to the configured `defaultMode` (direct out of the box).
3. Direct mode: stamp the objective and fairness headers and proxy to the gateway with a context deadline. No broker involvement.
4. Enqueue and wait modes: build `api.RequestMessage` (id from client header or generated, `created` now, `deadline` = now + min(client timeout, configured max), `payload` = verbatim body, `endpoint` = request path, `metadata` carrying tenant and traceparent, allowlisted headers), set the per-request result key and status marker, route to a queue by (model, tenant) from a config map, and enqueue via the producer interface (ZADD, score = deadline). Enqueue mode returns 202 with the id immediately. Wait mode blocks on the result key up to the capped wait.

Result path (modes 2 and 3):

1. Every enqueued message sets `result_queue_name` to a unique per-request key scoped by tenant (`results:req:<tenant>:<id>`), so fetching or deleting a result requires presenting the tenant it was enqueued under. The sorted-set result writer already routes by that field, so no AP changes to the dispatch path. Result keys carry a TTL (see Core AP changes).
2. Mode 2 fetch: `GET /v1/requests/{id}` is an idempotent, non-destructive read, repeatable until TTL expiry, with an optional DELETE endpoint for eager cleanup. Mode 3 wait: the POST handler subscribes to the key's arrival notification on the replica's shared pub/sub connection, checks the key once to close the arrival race, and parks until notification, backup poll tick, or the capped timeout (which returns the 202 response). If the frontend fails to write a response to the client, the result stays under its key for the remaining TTL and a re-fetch finds it.
3. Error mapping is uniform across both modes: `status_code > 0` mirrors the upstream status and body, `GATE_DROPPED` maps to 429 (with Retry-After when present), `DEADLINE_EXCEEDED` to 504, `INVALID_REQUEST` to 400, `GATE_ERROR` and `INFERENCE_ERROR` to 502, all wrapped in the OpenAI error JSON envelope.
4. No frontend state survives a request: results are addressed by request id, so any replica serves any fetch, and a replica death costs only the connections it was holding. Unfetched results expire via TTL. The frontend deploys as a plain Deployment behind a k8s Service with round robin, no sticky sessions or coordination, all replicas sharing the one Redis that already holds the queues and quota counters.
5. The producer's existing active token (`request-active:<id>`, present while a request is in flight) lets fetches distinguish pending (token present, result absent) from expired or unknown (both absent, 410). No separate status marker is needed.
6. Request ids are optional in all modes and server-generated when absent. A client-supplied id (per-request header) is the supported recovery mechanism for mode 3: retry with the same id dedupes at enqueue against the active token, or fetches the existing result, instead of risking duplicate execution. Without one, mode 3 connection loss degrades to ordinary HTTP retry-may-reexecute semantics. The frontend can additionally emit a 103 Early Hints interim response carrying the generated id (wait mode only). Clients are required to tolerate unexpected 1xx responses, but intermediaries forward them inconsistently, so this is best effort, config gated, and default off.

The wait mode cap is independent of the message deadline (timeout returns the 202 fallback, not an error). A request whose deadline expires while queued produces a DEADLINE_EXCEEDED result in its key, which fetches map to 504.

Deadlines on the direct path: the deadline stops being a scheduling input (no broker to order) and becomes a pure timeout, enforced by the frontend exactly the way workers enforce it on the queued path. The proxied request carries a context deadline of min(client timeout header, per-tier configured max). Expiry or client disconnect cancels the connection. No worker-style retries on the direct path: 429s and 503s relay to the caller, whose SDK retry logic is the right owner.

## Gateway labeling integration

How the frontend's tenant, tier, and classification reach the gateway. This is labeling only: what the gateway does with the labels (admission, scheduling, shedding) is the gateway's concern and out of scope for this design.

1. Priority reaches the EPP only through `x-llm-d-inference-objective` (alias `x-gateway-inference-objective`) resolving to an InferenceObjective CR that carries the priority number. The numeric `x-gateway-priority` header has no consumer upstream. Creating the CRs is a documented deployment prerequisite: define one per traffic class. The frontend stamps the objective name per request from its (tier, classification) map. On the queued path, the merge policy stamps the objective name per lane at dispatch, so reserved and overflow messages carry their own objectives (see Core AP changes).
2. The frontend stamps `x-llm-d-inference-fairness-id` with the tenant key, giving each tenant its own flow identity at the gateway. The merge policy stamps the same header at AP dispatch, so queued and direct traffic present one flow identity per tenant.

## Implications of all traffic through the frontend

- Latency overhead on the queued modes is user-visible: broker poll, dispatch, and result delivery all add up, so interactive queues need `pollIntervalMs` and result batch settings tuned well below the batch-oriented defaults, targeting low hundreds of ms over a direct gateway call. Direct mode adds only the quota round trip and proxy hop.
- Interactive traffic on the queued modes must ride the interactive tier (lane 0) with quota gates in classifying mode, so overload surfaces as 429 to callers instead of unbounded queueing.
- Durability tradeoff is acceptable for held connections: a hard AP crash can lose in-memory staged messages (up to the tier-priority staging buffer of 1000 per pool), but the caller observes a timeout or error and retries, which is normal HTTP semantics. Batch-style deferred submitters remain the ones who care, and they keep broker durability.
- Frontend replicas hold one open connection per in-flight request.

## Core AP changes

The design needs 3 changes in the AP, none touching its queues-only contract:

1. TTL on result destinations (per-queue config, applied on push), so unfetched per-request result keys expire. Belts without TTL configured are unchanged.
2. Merge policy: stamp `x-llm-d-inference-fairness-id` from metadata, and stamp the InferenceObjective name per lane in place of the numeric `x-gateway-priority` header (see Gateway labeling integration).
3. Worker: classify an in-flight send aborted by the request deadline as DEADLINE_EXCEEDED instead of INFERENCE_ERROR. This case occurs whenever a deadline expires while the request is queued or executing downstream, and the frontend needs it to map to 504 rather than 502.

Everything else exists: endpoint override, header passthrough, metadata tenant keys, verbatim status/body in results, 429 Retry-After handling.

## Risks and open questions

1. Streaming available via direct mode. The remaining gap is only streaming on the queued modes, where results transit the broker as single messages. Options if ever demanded: chunked result messages or a broker-bypass result channel. Probably low priority, as async and streaming are more or less opposite goals.
2. Mode 3 wake-up depends on Redis keyspace notifications (a Redis config requirement, off by default) or a small AP result-writer change to publish alongside the push. Deployments with neither fall back to polling. Redis is also now a synchronous dependency of the direct path's quota check. NOTE: on Redis failure the frontend fails open (quota briefly unenforced) rather than failing live traffic.

## Regarding gRPC Support

gRPC is not supported in v1: the frontend speaks HTTP/JSON only, matching the OpenAI protocol it exists to be compatible with. We plan to address this in a future release. What that entails:

- There is no standard OpenAI gRPC protocol, so gRPC support means defining our own service. The natural shape is one RPC per serving semantic rather than a mode header: `Complete` (direct, unary), `CompleteStream` (direct, server streaming), `Enqueue` returning the request id, `Fetch` by id, and `Wait`.
- The request payload stays opaque JSON bytes inside the proto messages, alongside typed envelope fields (tenant, request id, timeout). Fully typed protos mirroring the OpenAI chat schema are explicitly not planned: that schema is large and moving, has no canonical proto upstream, and typing it would break the frontend's payload-opacity invariant and create a permanent maintenance burden.
- gRPC terminates at the frontend regardless: everything downstream (gateway, model servers) speaks HTTP/JSON, so the frontend transcodes. Enqueue, Fetch, and Wait reuse the existing handlers nearly unchanged. `CompleteStream` needs one new piece, a pump that parses the upstream SSE stream and emits proto chunks.
- Implementation-wise, Connect-RPC (connect-go) is the likely vehicle: one handler definition serves gRPC, gRPC-web, and plain JSON/HTTP on the same port, so the gRPC surface stays a thin veneer over the existing code.

Until then, non-HTTP programmatic access remains available through mode 1 (the producer SDK against the broker directly).

## Future work

- Pub/Sub backend (the frontend touches the broker only through the producer interface, so this is backend work, not frontend work).
- Producer context-aware blocking pop with per-call timeout against a named result key, plus non-destructive read, replacing the frontend's direct Redis reads.
- Streaming on the queued modes if ever demanded (see Risks) and the gRPC surface (see Regarding gRPC Support).
