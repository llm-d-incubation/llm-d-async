---
pr: 385
url: https://github.com/llm-d/llm-d-async/pull/385
author: jtechapps
date: 2026-07-31
---
The `llm_d_async_async_gate_decisions_total` counter now records decisions (`gate_closed`, `quota_exhausted`, `dropped`, and `error`) for worker pool-level gates in addition to queue-level gates. Because `pool_name` identifies the worker pool on both queue-level and pool-level series, queries aggregating `sum by (pool_name)` across `async_gate_decisions_total` will now include decisions made at the pool gate in addition to the queue gate.
