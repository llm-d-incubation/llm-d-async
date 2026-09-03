---
pr: 400
url: https://github.com/llm-d/llm-d-async/pull/400
author: Abhinav-kodes
date: 2026-08-16
---
New queue-deadline observability for the redis-sortedset broker: the
`llm_d_async_async_deadline_proximity_millis` histogram reports time-to-deadline
for queued items as a per-poll snapshot histogram of exact cumulative bucket
counts (`le="0"` for items past their deadline but still queued, up to 24h),
an SLO early warning for batches that would expire simultaneously. Counts are
exact broker queries, not samples, so large queues and prompts carry no extra
cost; the `_sum` is estimated from bucket midpoints. The metric carries the
queue/pool label triple and is redis-sortedset only (Cloud Pub/Sub cannot
expose per-item deadlines).
