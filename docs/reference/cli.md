# Command Line Parameters

**Core:**

| Flag | Default | Description |
|------|---------|-------------|
| `concurrency` | `64` | Number of concurrent workers (per pool if unspecified). The processor is I/O-bound (each worker holds one in-flight request for its full duration), so in-flight concurrency caps throughput — see [Queues, Topics, and Worker Pools](../../README.md#queues-topics-and-worker-pools). |
| `transport` | `redis-pubsub` | The transport (message queue) implementation. One of `redis-pubsub`, `redis-sortedset`, `gcp-pubsub`. Gating is configured per queue/topic via `gate_type` in the transport config (this replaces the former `gcp-pubsub-gated` implementation). |
| `transport-config` | — | Inline JSON transport configuration. See [Transport Configuration](../../docs/transports.md#transport-configuration). Mutually exclusive with `transport-config-file`; exactly one of the two is required. |
| `transport-config-file` | — | Path to a JSON file with the transport configuration. Mutually exclusive with `transport-config`. |
| `pool-config-file` | — | Path to the JSON worker pool definitions. If omitted, a single `"default"` worker pool is created with concurrency determined by the global `concurrency` flag. See [Worker Pools Configuration](../../docs/worker-pools.md#worker-pools-configuration). |
| `request-merge-policy-config-file` | — | Path to the JSON request merge policy specification (`type` and `parameters`). If not specified, defaults to the `random-robin` policy. The older `--request-merge-policy-config` name is a deprecated alias. |
| `transform-config-file` | — | Path to the JSON request body transform configuration. Empty disables transforms. See [Request Body Transform Reference](../../docs/requests-and-results.md#request-body-transform-reference). |

**Gating and Prometheus:**

| Flag | Default | Description |
|------|---------|-------------|
| `prometheus-url` | — | Prometheus server URL for metric-based gates (e.g., http://localhost:9090). Required when using metric-based gates (`prometheus-saturation`, `prometheus-budget`, `prometheus-query`). For Google Managed Prometheus (GMP), point this to a local proxy or GMP frontend that handles authentication — direct GMP URLs are not supported as the Async Processor does not perform GMP authentication. |
| `prometheus-cache-ttl` | `5s` | TTL for cached Prometheus metric sources (e.g. `1m`, `0s` to disable). Increasing this reduces Prometheus load but also reduces the responsiveness of dispatch gates to metric changes. |

**Timeouts and draining:**

| Flag | Default | Description |
|------|---------|-------------|
| `request-timeout` | `5m` | Timeout for individual inference requests. |
| `drain-timeout` | `2m` | Maximum time to wait for in-flight requests to complete after SIGTERM. |

**Ports and endpoints:**

| Flag | Default | Description |
|------|---------|-------------|
| `metrics-port` | `9090` | Port serving Prometheus metrics. |
| `metrics-endpoint-auth` | `true` | Enables authentication and authorization of the metrics endpoint. |
| `health-port` | `8081` | The health probe port. |
| `metrics-backlog-poll-interval` | `15s` | Interval to poll the broker for queue backlog metrics (`0` disables). Only applies to transports that support it (`redis-sortedset`, `gcp-pubsub`). |

**TLS (outbound, towards the inference gateway):**

| Flag | Default | Description |
|------|---------|-------------|
| `tls-ca-cert` | — | Path to CA certificate file (PEM) for verifying the inference gateway. |
| `tls-cert` / `tls-key` | — | Paths to client certificate/key files (PEM) for mTLS. Must be provided together. |
| `tls-insecure-skip-verify` | `false` | Skip TLS certificate verification (dev/test only). |

**Logging:**

| Flag | Default | Description |
|------|---------|-------------|
| `v` | `2` | Log level verbosity. |
| `zap-*` | — | Standard controller-runtime zap flags (`zap-devel`, `zap-encoder`, `zap-log-level`, `zap-stacktrace-level`, `zap-time-encoding`). |

> **Deprecated:** The per-backend flags — `--message-queue-impl`, `--redis.url`, `--redis.*`, `--redis.ss.*`, `--pubsub.*`, `--redis-tracing`, and `--request-merge-policy-config` — still work for backwards compatibility but are deprecated. When used, the processor logs a warning and translates them into the transport config. `--transport`/`--transport-config` take precedence when both are set. The legacy flags are documented per backend under [Implementations](../../docs/transports.md#implementations).
