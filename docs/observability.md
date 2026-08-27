# Observability

## Prometheus Metrics

The Async Processor exposes Prometheus metrics under the `llm_d_async` subsystem on the metrics port (default `9090`). All counters and histograms carry `queue_id`, `queue_name`, and `pool_name` labels so you can filter and aggregate per queue.

**Request lifecycle:**

| Metric | Type | Description |
|--------|------|-------------|
| `llm_d_async_async_request_total` | Counter | New async requests (first attempt only) |
| `llm_d_async_async_successful_requests_total` | Counter | Requests that received a successful inference response |
| `llm_d_async_async_tokens_total` | Counter | Tokens processed by successfully-dispatched requests, by `direction`: `input` (prompt_tokens) and `output` (completion_tokens). Parsed best-effort from the OpenAI `usage` object in 2xx response bodies; no-op when usage is absent or the body is not parseable (e.g. streaming responses). Non-OpenAI gateways undercount by design. |
| `llm_d_async_async_failed_requests_total` | Counter | Requests that failed with a fatal or non-retryable error |
| `llm_d_async_async_shedded_requests_total` | Counter | Requests shedded due to rate limiting (429 / capacity) |
| `llm_d_async_async_exceeded_deadline_requests_total` | Counter | Requests that exceeded their deadline before completion |
| `llm_d_async_async_request_retries_total` | Counter | Retry attempts |

**Latency and deadlines:**

| Metric | Type | Description |
|--------|------|-------------|
| `llm_d_async_async_message_latency_time_millis` | Histogram | End-to-end message latency in milliseconds (publish to successful processing). Only registered when the transport supports message latency (GCP Pub/Sub only). |
| `llm_d_async_async_inference_latency_time_millis` | Histogram | Time in milliseconds spent calling `llm-d-router` (or other inference gateway), measured around each request attempt. Isolates "model time" from "queue time". Always registered. |
| `llm_d_async_async_queue_residence_time_millis` | Histogram | Time in milliseconds a message spent buffered in-process, from broker ingestion until a worker pulled it for processing. Measures the async delay introduced by the system (queue time). Always registered. |
| `llm_d_async_async_deadline_proximity_millis` | Histogram | Snapshot histogram of time in milliseconds remaining until each queued item's deadline, rebuilt once per backlog poll from exact cumulative `ZCOUNT` counts per bucket (`le="0"` holds items past their deadline but still queued; higher buckets are cumulative, so every item counts in each bucket it expires in). Redis sorted-set only; Cloud Pub/Sub cannot expose per-item deadlines. Because each poll replaces the snapshot, the series is not monotonic — `rate()` is meaningless; use `histogram_quantile` per scrape. The `_sum` is estimated from bucket midpoints. |

**Capacity and backlog:**

| Metric | Type | Description |
|--------|------|-------------|
| `llm_d_async_async_queue_depth` | Gauge | Requests received from the broker and buffered in-process awaiting an available worker |
| `llm_d_async_async_inflight_requests` | Gauge | Requests currently being processed by workers (dispatched to inference, awaiting a response) |
| `llm_d_async_async_broker_backlog` | Gauge | Undelivered/pending messages held by the broker queue (polled every `metrics-backlog-poll-interval`; `redis-sortedset` and `gcp-pubsub` only) |
| `llm_d_async_async_pool_worker_limit` | Gauge | Configured worker concurrency limit for a pool (carries only the `pool_name` label). Compare against `llm_d_async_async_inflight_requests` to compute worker utilization. |

**Gates:**

| Metric | Type | Description |
|--------|------|-------------|
| `llm_d_async_async_dispatch_budget` | Gauge | Current dispatch budget [0.0–1.0] returned by the queue's gate; the fraction of system capacity available for new requests (0.0 = gate fully closed). Useful for diagnosing why throughput is throttled. |
| `llm_d_async_async_gate_decisions_total` | Counter | Count of gate decisions that prevented dispatch, by `reason`: `gate_closed` (no dispatch budget), `quota_exhausted` (per-attribute quota overflow), `dropped` (gate permanently rejected the request), `error` (gate evaluation failed). `quota_exhausted`, `dropped` and `error` count individual messages refused after being dequeued. `gate_closed` counts those plus every dequeue round in which the budget shrank the batch to zero — the way budget-based gates (`prometheus-budget`/`-saturation`/`-query`) shed work *before* a message is dequeued — so its rate reflects throttled dispatch opportunities, not messages. All four `reason` series are created at 0 when a queue or gated worker pool starts, so a query returns 0 rather than an empty vector. |
| `llm_d_async_async_gate_metric_value` | Gauge | Raw metric value a metric-based gate (`prometheus-saturation`/`-budget`/`-query`) last read — the number compared against the threshold below. For the saturation gate this is `1 - saturation`. |
| `llm_d_async_async_gate_metric_threshold` | Gauge | Threshold the value above is compared against. The gate closes when `value <= threshold`, which is what drives `async_dispatch_budget` to 0. |
| `llm_d_async_async_gate_metric_source_available` | Gauge | Whether the gate's last evaluation got a reading from any metric source (1) or fell back to `fallback` (0) |

**Labels:**

| Label | Description |
|-------|-------------|
| `queue_id` | Queue identifier. For `redis-sortedset`, from the queue config `id` field (defaults to the queue name); other transports use the queue name / subscriber ID. |
| `queue_name` | Logical queue name (Redis sorted set name, channel name, or Pub/Sub subscriber ID) |
| `pool_name` | Worker pool the queue routes to (`async_pool_worker_limit` carries only this label) |
| `reason` | Gate-decision reason (only on `async_gate_decisions_total`): `gate_closed`, `quota_exhausted`, `dropped`, `error` |
| `inference_pool` | InferencePool a gate queries (only on the `async_gate_metric_*` gauges), from the gate's `pool` param. Empty when the gate does not name one. |
| `direction` | Token direction (only on `async_tokens_total`): `input` or `output` |

`pool_name` always names the **async worker pool** that owns the series, never the
InferencePool a gate happens to query — that is what `inference_pool` is for. Every
per-queue series therefore carries the same `queue_id`/`queue_name`/`pool_name`
triple and joins on it, including the gate gauges. A **pool-level** gate (one
configured on a worker pool rather than a queue) has no single queue, so its gauges
and `async_gate_decisions_total` counter carry an empty `queue_id` and `queue_name`
and are keyed by `pool_name` alone.

**Example PromQL queries:**

```promql
# Per-queue success ratio over the last 5 minutes
rate(llm_d_async_async_successful_requests_total[5m]) / rate(llm_d_async_async_request_total[5m])

# Which queues are getting rate-limited?
rate(llm_d_async_async_shedded_requests_total[5m])

# Retry ratio by queue
rate(llm_d_async_async_request_retries_total[5m]) / rate(llm_d_async_async_request_total[5m])

# p95 llm-d-router / inference gateway latency by queue (model time, excluding queue time)
histogram_quantile(0.95, sum by (queue_name, le) (rate(llm_d_async_async_inference_latency_time_millis_bucket[5m])))

# p95 queue residence time by queue (async delay, excluding model time)
histogram_quantile(0.95, sum by (queue_name, le) (rate(llm_d_async_async_queue_residence_time_millis_bucket[5m])))

# Worker utilization per pool
sum by (pool_name) (llm_d_async_async_inflight_requests) / llm_d_async_async_pool_worker_limit

# Why is a queue's gate closed? The gauges join on the queue triple, so you can
# put the budget, the value it came from, and the threshold on one panel.
llm_d_async_async_dispatch_budget
llm_d_async_async_gate_metric_value
llm_d_async_async_gate_metric_threshold

# How much headroom does each queue's gate have?
llm_d_async_async_gate_metric_value - on(queue_id, queue_name, pool_name) llm_d_async_async_gate_metric_threshold

# Throttling rate against the pool it is throttling
sum by (pool_name) (rate(llm_d_async_async_gate_decisions_total{reason="gate_closed"}[5m]))
```

## OpenTelemetry Tracing

The Async Processor supports distributed tracing via [OpenTelemetry](https://opentelemetry.io/). When enabled, it exports traces to an OTLP-compatible collector (e.g., Jaeger, Grafana Tempo, OpenTelemetry Collector).

**Spans emitted:**

| Span Name | Description |
|-----------|-------------|
| `process-request` | Per-request span covering validation, dispatch, and result routing |
| `http-request` | Child span for the outgoing HTTP call to `llm-d-router` (via `otelhttp`) |
| `re-enqueue` | Linked span created when a request is re-enqueued during graceful shutdown |

**Span attributes:**

| Attribute | Description |
|-----------|-------------|
| `request.id` | Request identifier |
| `queue.id` | Queue identifier (matches Prometheus `queue_id` label) |
| `queue.name` | Queue name (matches Prometheus `queue_name` label) |
| `retry.count` | Current retry attempt (0 for first attempt) |
| `error.category` | Error classification on failure (`RATE_LIMIT`, `SERVER_ERROR`, `UNKNOWN`, etc.) |

**Trace context propagation:**

Producers can inject W3C Trace Context (`traceparent`/`tracestate`) and Baggage into the request's `metadata` field. The processor extracts it and creates child spans under the producer's trace, enabling end-to-end distributed tracing across the queue boundary.

```json
{
    "id": "req-123",
    "deadline": 1764045130,
    "payload": {"model": "my-model", "prompt": "hello"},
    "metadata": {
        "traceparent": "00-a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6-1234567890abcdef-01"
    }
}
```

The processor also injects trace context into outgoing inference requests via W3C headers, so `llm-d-router` can continue the trace.

**Configuration:**

Tracing is controlled via standard OpenTelemetry environment variables. Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable; leave it empty to disable (no-op).

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC collector endpoint (e.g., `http://jaeger:4317`). Empty disables tracing. | _(disabled)_ |
| `OTEL_EXPORTER_OTLP_INSECURE` | Use plaintext gRPC connection | SDK default (secure); Helm chart sets `true` |
| `OTEL_SERVICE_NAME` | Service name for traces | `llm-d-async` |
| `OTEL_TRACES_SAMPLER` | Sampling strategy (`always_on`, `parentbased_traceidratio`, etc.) | SDK default (`parentbased_always_on`); Helm chart sets `parentbased_traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Sampling ratio (0.0–1.0) | Helm chart sets `1.0` |

The binary itself reads `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_SERVICE_NAME`; the remaining variables are handled by the OpenTelemetry SDK, and the defaults shown as "Helm chart sets" apply when deploying with the provided chart.

**Redis command tracing:**

- `enable_tracing` (transport config): Enable per-command Redis tracing spans via `redisotel`. Produces high span volume — use only for debugging. Default: `false`. Set it in the Redis `--transport-config` (the older `--redis-tracing` CLI flag is a deprecated alias).

**Helm chart:**

```yaml
ap:
  otel:
    endpoint: "http://jaeger:4317"  # leave empty to disable
    insecure: true
    sampler: "parentbased_traceidratio"
    samplerArg: "1.0"
    redisTracing: false
```
