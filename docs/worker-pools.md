# Worker Pools and Merge Policies

## Worker Pools Configuration

When using multiple queues or topics, the worker capacities and pool-level gates for named pools can be configured via a dedicated worker pools file (`--pool-config-file`).

**JSON Schema:**
```json
[
  {
    "id": "qwen-pool",
    "workers": 4,
    "gate_type": "local-max-concurrency",
    "gate_params": {
      "limit": "2"
    }
  }
]
```

**Fields:**
- `id` (required): Unique pool identifier referenced by queue/topic configurations.
- `workers` (required): Number of concurrent workers dedicated to this pool. Must be positive.
- `gate_type` (optional): The type of dispatch gate to apply to the pool (e.g. `local-max-concurrency`, `prometheus-saturation`).
- `gate_params` (optional): Key-value parameters configuring the gate.


## Request Merge Policy Reference

The merge policy is configured using the `--request-merge-policy-config-file` CLI flag (the older `--request-merge-policy-config` name is a deprecated alias). It points to a JSON configuration file specifying the policy `type` and optional custom `parameters`:

```json
{
  "type": "tier-priority",
  "parameters": {
    "priority_header": "x-gateway-priority",
    "lane_objectives": {
      "reserved-interactive": "premium-latency",
      "overflow-batch": "best-effort"
    }
  }
}
```

See [Request Merge Policies](../README.md#request-merge-policies) for how per-pool merging works.

1. **`random-robin`**: Randomly picks messages from all queues configured for a given pool. This is the default policy.
   - **Parameters**:
     - `fairness_header` (optional, string): The HTTP header name used to pass the tenant's fairness identity to the gateway's flow control. Set to `""` to disable stamping. A name that is not a legal HTTP header name is rejected at startup. Default is `"x-llm-d-inference-fairness-id"`.
     - `fairness_attribute` (optional, string): The message metadata attribute holding the tenant identity (the same attribute the `redis-quota` gate keys on). The stamped value replaces any caller-supplied header of the same name under any letter case, so the identity the gateway arbitrates on is the one quota is accounted against. The header is only stamped when the attribute is present, non-empty, at most 256 bytes, and a legal HTTP header value; otherwise the request dispatches with the header untouched. Default is `"userid"`.
   - **Note**: Stamping is on by default and sends the attribute's value to the gateway, where it may be recorded in access logs. Prefer an opaque tenant ID over personally identifying values such as email addresses, or set `fairness_header` to `""` to disable stamping.
2. **`tier-priority`**: Buckets requests into 6 strict priority lanes using routing tags (`(classification, tier)`) — see [Tiers and Priority Lanes](../README.md#tiers-and-priority-lanes) for the lane order and defaults. Within each bucket, it round-robins across different client channels and stamps the chosen priority header with the numeric lane index (0 = highest priority).
   - **Note**: The `tier-priority` merge policy assumes that all messages within a single queue share the same priority. Message classification relies on the FIFO order of an individual queue, and a message's classification does not change after it is pulled off the queue.
   - **Parameters**:
     - `priority_header` (optional, string): The HTTP header name used to pass the priority value downstream to the inference scheduler. A name that is not a legal HTTP header name is rejected at startup. Default is `"x-gateway-priority"`.
     - `tier_label` (optional, string): The label name on `InternalRequest.Labels` used to look up the request's priority tier. Default is `"tier"`.
     - `objective_header` (optional, string): The HTTP header name used to stamp the lane's InferenceObjective name. A name that is not a legal HTTP header name is rejected at startup. Default is `"x-llm-d-inference-objective"` (`api.ObjectiveHeader`).
     - `lane_objectives` (optional, object): Maps lane keys (`"reserved-interactive"`, `"reserved-async"`, `"reserved-batch"`, `"overflow-interactive"`, `"overflow-async"`, `"overflow-batch"`) to InferenceObjective names. A request whose lane has an entry gets that objective stamped as `objective_header`, which overrides the queue-level `inference_objective`. Lanes without an entry fall back to the queue objective.
     - `fairness_header` / `fairness_attribute` (optional, string): Same as for `random-robin`.
