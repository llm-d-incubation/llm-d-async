# Transports

## Transport Configuration

The transport (message queue) is selected with `--transport` and configured with a single JSON document, supplied either inline via `--transport-config` or from a file via `--transport-config-file` (the two are mutually exclusive; exactly one is required). This is the recommended configuration surface for all backends.

**`redis-pubsub`:**
```json
{
  "url": "redis://user:pass@host:6379/0",
  "retry_queue_name": "retry-sortedset",
  "result_queue_name": "result-queue",
  "enable_tracing": false,
  "queues": [ { "queue_name": "request-queue", "igw_base_url": "http://localhost:30800" } ]
}
```

**`redis-sortedset`:**
```json
{
  "url": "redis://user:pass@host:6379/0",
  "result_queue_name": "result-list",
  "retry_queue_name": "retry-sortedset",
  "poll_interval_ms": 1000,
  "batch_size": 10,
  "enable_tracing": false,
  "queues": [ { "queue_name": "request-sortedset", "igw_base_url": "http://localhost:30800", "gate_type": "redis", "gate_params": { "address": "localhost:6379" } } ]
}
```

**`gcp-pubsub`:**
```json
{
  "project_id": "my-project",
  "result_topic_id": "results",
  "batch_size": 10,
  "topics": [ { "subscriber_id": "requests-sub", "igw_base_url": "http://localhost:30800", "gate_type": "constant", "gate_params": {} } ]
}
```

**Top-level fields:**

| Field | Transports | Default | Description |
|-------|-----------|---------|-------------|
| `url` | redis-* | `REDIS_URL` env | Redis/Valkey URL (e.g. `redis://user:pass@host:port/db`, `rediss://...` for TLS). An explicit `url` takes precedence; `REDIS_URL` fills it in only when empty. Required (one of the two). |
| `retry_queue_name` | redis-* | `retry-sortedset` | Sorted set used for retry scheduling. |
| `result_queue_name` | redis-pubsub | `result-queue` | Channel for results. |
| `result_queue_name` | redis-sortedset | `result-list` | List for results. |
| `poll_interval_ms` | redis-sortedset | `1000` | Poll interval in milliseconds. |
| `batch_size` | redis-sortedset, gcp-pubsub | `10` | Messages per poll (sortedset) / inflight messages (Pub/Sub). |
| `enable_tracing` | redis-* | `false` | Per-command Redis tracing spans via `redisotel`. High span volume — debugging only. |
| `project_id` | gcp-pubsub | — | GCP project ID (required). |
| `result_topic_id` | gcp-pubsub | — | Results topic ID (required). |
| `queues` / `topics` | all | — | Array of queue/topic entries (at least one required). See below. |

## Queue and Topic Entry Fields

Each entry in `queues`/`topics` describes one request source and where its requests dispatch to:

```json
{
   "queue_name": "batch_queue",
   "igw_base_url": "http://localhost:30800",
   "request_path_url": "/v1/completions",
   "inference_objective": "batch-task",
   "worker_pool_id": "qwen-pool",
   "gate_type": "prometheus-saturation",
   "gate_params": { "pool": "inference_pool_1", "threshold": "0.8" },
   "labels": { "tier": "batch", "team": "billing" }
}
```

**Common fields (all transports):**

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `queue_name` (Redis) / `subscriber_id` (Pub/Sub) | yes | — | The Redis channel/sorted-set name, or the GCP Pub/Sub subscriber ID. |
| `igw_base_url` | yes | — | Base URL of `llm-d-router`, an inference gateway, or target model server. |
| `request_path_url` | no | `/v1/completions` | Request path (e.g. `/v1/chat/completions`). |
| `inference_objective` | no | — | InferenceObjective for requests (set as the HTTP header `x-llm-d-inference-objective` if not empty). |
| `worker_pool_id` | no | `default` | The worker pool to route to (defined in the [worker pools configuration](../docs/worker-pools.md#worker-pools-configuration)). |
| `labels` | no | — | Key-value string pairs injected as routing metadata (`Labels`) into the internal request envelope at ingestion/pull time. Used e.g. for the `tier` label. |

**Gate fields (`redis-sortedset` and `gcp-pubsub` only):**

| Field | Required | Description |
|-------|----------|-------------|
| `gate_type` | no | The dispatch gate type for this queue/topic. See [Dispatch Gate Reference](../docs/gates.md#dispatch-gate-reference). |
| `gate_params` | no | Key-value parameters for the gate. |

> **Note:** The ephemeral `redis-pubsub` transport does not support per-queue dispatch gates — `gate_type`/`gate_params` on its queue entries are ignored. Use `redis-sortedset` for per-queue gating.

**Additional fields (`redis-sortedset` only):**

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `id` | no | `queue_name` | Unique queue identifier; becomes the `queue_id` metric label. |
| `result_queue_name` | no | top-level `result_queue_name` | Per-queue result destination override. |
| `result_ttl_seconds` | no | `0` (no expiry) | When > 0, sets an expiry on the result destination each time results are pushed. Used for per-request result keys (frontend enqueue mode) so unfetched results are cleaned up. |


## Backend Compatibility

The Async Processor uses the Redis wire protocol for its message queue implementations (`redis-sortedset`, `redis-pubsub`) and dispatch gates (`redis`, `redis-quota`). Redis-protocol-compatible backends such as [Valkey](https://valkey.io/) can be used with the existing Redis configuration surface.

The `url` field in the transport configuration (see [Transport Configuration](#transport-configuration)), the `REDIS_URL` environment variable, and the deprecated `--redis.*` CLI flags all work unchanged with Valkey — point them at your Valkey endpoint the same way you would with Redis.

> **Note:** The `url`/`redis.*` naming is retained because it refers to the wire protocol, not a specific product.


## Implementations

### Redis Sorted Set (Persisted)

A persisted implementation based on Redis SortedSets. Recommended for production: it offers persistence, priority sorting, and per-queue dispatch gates.

![Async Processor - Redis Sorted Set architecture](/docs/images/redis_sortedset_architecture.png "AP - Redis SortedSet")

#### Legacy Redis Sorted Set command line parameters

> **Deprecated:** Prefer `--transport redis-sortedset` with `--transport-config`/`--transport-config-file` (see [Transport Configuration](#transport-configuration)). The `--redis.ss.*` and `--redis.url` flags below still work but are deprecated aliases translated into the transport config: `--redis.url` → `url`, `--redis.ss.poll-interval-ms` → `poll_interval_ms`, `--redis.ss.batch-size` → `batch_size`, `--redis.ss.result-queue-name` → `result_queue_name`, and the single-queue `--redis.ss.igw-base-url`/`--redis.ss.request-queue-name`/`--redis.ss.request-path-url`/`--redis.ss.inference-objective`/`--redis.ss.gate-type`/`--redis.ss.gate-params` (or `--redis.ss.queues-config`/`--redis.ss.queues-config-file`) → the `queues` array.

- `redis.url`: Redis/Valkey URL (e.g. `redis://user:pass@host:port/db` or `rediss://...` for TLS). Supports Redis-protocol-compatible backends such as Valkey. Can also be set via `REDIS_URL` env var.
- `redis.ss.igw-base-url`: Base URL of the IGW (e.g. https://localhost:30800).<br> Mutually exclusive with `redis.ss.queues-config-file` flag.
- `redis.ss.request-path-url`: Request path url (e.g.: "/v1/completions"). <br> Mutually exclusive with `redis.ss.queues-config-file` flag.
- `redis.ss.inference-objective`: InferenceObjective to use for requests (set as the HTTP header x-gateway-inference-objective if not empty).  <br> Mutually exclusive with `redis.ss.queues-config-file` flag.
- `redis.ss.request-queue-name`: The name of the sorted-set for the requests. Default is <u>request-sortedset</u>.  <br> Mutually exclusive with `redis.ss.queues-config-file` flag.
- `redis.ss.result-queue-name`: The name of the list for the results. Default is <u>result-list</u>.
- `redis.ss.queues-config-file`: The configuration file name when using multiple queues — a JSON array of [queue entries](#queue-and-topic-entry-fields). <br> Mutually exclusive with `redis.ss.igw-base-url`, `redis.ss.request-queue-name`, `redis.ss.request-path-url` and `redis.ss.inference-objective` flags.
- `redis.ss.poll-interval-ms`: Poll interval in milliseconds. Default is <u>1000</u>.
- `redis.ss.batch-size`: Number of messages to process per poll. Default is <u>10</u>.
- `redis.ss.gate-type`: Gate type for single-queue mode (e.g., `redis`, `prometheus-saturation`). Only used when `redis.ss.queues-config-file` is not set.
- `redis.ss.gate-params`: JSON-encoded gate params map for single-queue mode (e.g., `{"address":"localhost:6379"}`). Only used when `redis.ss.queues-config-file` is not set.

### Redis Channels (Ephemeral)

<u>NOTE:</u> Consider using the [Redis Sorted Set](#redis-sorted-set-persisted) implementation for production use,
as it offers persistence and priority sorting.

An example implementation based on Redis channels is provided.

- Redis Channels as the request queues.
- Redis Sorted Set as the retry exponential backoff implementation.
- Redis Channel as the result queue.

This transport does not support per-queue dispatch gates (see [Queue and Topic Entry Fields](#queue-and-topic-entry-fields)).

![Async Processor - Redis architecture](/docs/images/redis_pubsub_architecture.png "AP - Redis")

#### Legacy Redis Channels command line parameters

> **Deprecated:** Prefer `--transport redis-pubsub` with `--transport-config`/`--transport-config-file` (see [Transport Configuration](#transport-configuration)). The `--redis.*` and `--redis.url` flags below still work but are deprecated aliases translated into the transport config: `--redis.url` → `url`, `--redis.retry-queue-name` → `retry_queue_name`, `--redis.result-queue-name` → `result_queue_name`, and the single-queue `--redis.igw-base-url`/`--redis.request-queue-name`/`--redis.request-path-url`/`--redis.inference-objective` (or `--redis.queues-config`/`--redis.queues-config-file`) → the `queues` array.

- `redis.url`: Redis/Valkey URL (e.g. `redis://user:pass@host:port/db` or `rediss://...` for TLS). Supports Redis-protocol-compatible backends such as Valkey. Can also be set via `REDIS_URL` env var.
- `redis.igw-base-url`: Base URL of the IGW (e.g. https://localhost:30800).<br> Mutually exclusive with `redis.queues-config-file` flag.
- `redis.request-path-url`: Request path url (e.g.: "/v1/completions"). <br> Mutually exclusive with `redis.queues-config-file` flag.
- `redis.inference-objective`: InferenceObjective to use for requests (set as the HTTP header x-gateway-inference-objective if not empty).  <br> Mutually exclusive with `redis.queues-config-file` flag.
- `redis.request-queue-name`: The name of the channel for the requests. Default is <u>request-queue</u>.  <br> Mutually exclusive with `redis.queues-config-file` flag.
- `redis.retry-queue-name`: The name of the channel for the retries. Default is <u>retry-sortedset</u>.
- `redis.result-queue-name`: The name of the channel for the results. Default is <u>result-queue</u>.
- `redis.queues-config-file`: The configuration file name when using multiple queues — a JSON array of [queue entries](#queue-and-topic-entry-fields). <br> Mutually exclusive with `redis.igw-base-url`, `redis.request-queue-name`, `redis.request-path-url` and `redis.inference-objective` flags.

### GCP Pub/Sub

The GCP PubSub implementation requires the user to configure the following:

- Requests Topic and a **Subscription** having the following configurations:
    - Exactly once delivery.
    - Retries with exponential backoff.
    - Dead Letter Queue (DLQ).
- Results Topic.

<u>Note:</u> If DLQ is NOT configured for the request topic, retried messages will be counted multiple times in the #_of_requests metric.

![Async Processor - GCP PubSub Architecture](/docs/images/gcp_pubsub_architecture.png "AP - GCP PubSub")

#### Legacy GCP PubSub command line parameters

> **Deprecated:** Prefer `--transport gcp-pubsub` with `--transport-config`/`--transport-config-file` (see [Transport Configuration](#transport-configuration)). The `--pubsub.*` flags below still work but are deprecated aliases translated into the transport config: `--pubsub.project-id` → `project_id`, `--pubsub.result-topic-id` → `result_topic_id`, `--pubsub.batch-size` → `batch_size`, and the single-topic `--pubsub.request-subscriber-id`/`--pubsub.igw-base-url`/`--pubsub.request-path-url`/`--pubsub.inference-objective` (or `--pubsub.topics-config-file`) → the `topics` array. Per-topic gating (formerly the `gcp-pubsub-gated` implementation) is now configured with `gate_type`/`gate_params` in each topic entry.

- `pubsub.project-id`: The name GCP project ID using the PubSub API.
- `pubsub.igw-base-url`: Base URL of the IGW (e.g. https://localhost:30800).<br> Mutually exclusive with `pubsub.topics-config-file` flag.
- `pubsub.request-path-url`: Request path url (e.g.: "/v1/completions"). <br> Mutually exclusive with `pubsub.topics-config-file` flag.
- `pubsub.inference-objective`: InferenceObjective to use for requests (set as the HTTP header x-gateway-inference-objective if not empty). <br> Mutually exclusive with `pubsub.topics-config-file` flag.
- `pubsub.request-subscriber-id`: The subscriber ID for the requests topic.<br> Mutually exclusive with `pubsub.topics-config-file` flag.
- `pubsub.result-topic-id`: The results topic ID.
- `pubsub.batch-size`: Number of inflight messages. Default is <u>10</u>.
- `pubsub.topics-config-file`: The configuration file name when using multiple topics — a JSON array of [topic entries](#queue-and-topic-entry-fields). <br> Mutually exclusive with `pubsub.request-subscriber-id`, `pubsub.request-path-url` and `pubsub.inference-objective` flags.
