# OpenAI-Compatible Frontend

An optional HTTP frontend that lets stock OpenAI clients use llm-d-async without speaking the broker protocol. It is a plain producer and result reader: it enqueues onto the same Redis queues the async processor consumes and reads results from per-request keys. The AP itself is unchanged. Design details are in [docs/openai-frontend-design.md](../docs/openai-frontend-design.md).

## Serving modes

Every `POST /v1/*` request is served in one of three modes, selected by the `X-AP-Mode` header (or `defaultMode` when the header is absent):

| Mode | Behavior |
| :-- | :-- |
| `direct` | Labels the request (objective, fairness id) and reverse proxies it straight to the inference gateway. Supports streaming. Counts against the same Redis quota counters the AP's gates use. |
| `enqueue` | Enqueues onto the broker and returns `202` with a request id. Fetch the result later with `GET /v1/requests/{id}`. |
| `wait` | Enqueue plus a held connection that returns the result when it lands. If the wait cap expires first, the client gets the `202` response and falls back to fetching. |

## Endpoints

| Route | Purpose |
| :-- | :-- |
| `POST /v1/completions`, `POST /v1/chat/completions` (any `/v1/*` POST) | Submit a request in the selected mode |
| `GET /v1/requests/{id}` | Fetch a queued result: `200` ready, `202` pending, `410` unknown or expired |
| `DELETE /v1/requests/{id}` | Reclaim a result immediately |
| `GET /v1/models` | Models derived from the configured routes |
| `GET /healthz`, `GET /readyz` | Probes |
| `GET /metrics` | Prometheus metrics (`llm_d_async_frontend_*`) |

Request headers: `X-AP-Mode` (serving mode), `X-Team` (tenant), `X-Request-Id` (client-supplied id, the recovery handle for queued modes), `X-Request-Timeout-Seconds` (deadline, capped per mode). Header names are configurable. Tenant and request id must not contain `:`.

## Setup

The frontend is a single binary configured by one YAML file:

```sh
go run ./cmd --config config.yaml
```

Minimal config:

```yaml
redisURL: "redis://redis:6379"      # the Redis that holds the AP's queues
igwBaseURL: "http://gateway:80"     # inference gateway base URL for direct mode
defaultMode: direct                 # direct | enqueue | wait, per-request header wins
routes:
  - model: "my-model"
    queue: "team-a-queue"
    tier: "interactive"
```

All keys and their defaults are documented in [config.go](config.go). Three AP-side settings matter for the queued modes:

1. The queues the frontend targets must not set `result_queue_name` in the AP queue config, so the per-request result key routing applies.
2. Set `result_ttl_seconds` on those queues so unfetched results expire.
3. For fairness arbitration at the gateway, set `fairness_attribute` in the merge policy parameters to the frontend's quota attribute (default `team`), so the AP stamps queued dispatches with the same tenant identity the frontend keys quota on and stamps on its direct path.

Wait mode wakes on Redis keyspace notifications when `notify-keyspace-events` includes `K` and `l`, and falls back to polling otherwise (`wakeupMode` controls this).

### Kubernetes

The chart deploys the frontend when `frontend.enabled` is true, rendering `frontend.config` verbatim into a ConfigMap. See the `frontend` block in [charts/llm-d-async/values.yaml](../charts/llm-d-async/values.yaml). The image builds from [Dockerfile.frontend](../Dockerfile.frontend):

```sh
docker build -f Dockerfile.frontend -t <registry>/llm-d-async-frontend:dev .
```

## Result lifecycle (queued modes)

Results land in tenant-scoped keys (`results:req:<tenant>:<id>`) with the queue's `result_ttl_seconds`. Fetch is a non-destructive, idempotent read. A delivered fetch shrinks the key's TTL to a short grace window covering lost-response retries, a delivered wait deletes the key outright, and `DELETE` reclaims immediately. The full TTL bounds the case where nobody ever fetches.
