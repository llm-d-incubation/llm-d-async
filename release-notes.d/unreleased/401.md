---
pr: 401
url: https://github.com/llm-d/llm-d-async/pull/401
author: Abhinav-kodes
date: 2026-08-16
---
Token throughput counter: `llm_d_async_async_tokens_total` reports input
and output tokens processed by successfully-dispatched requests, parsed
best-effort from the OpenAI `usage` object in 2xx response bodies. No-op
when usage is absent or the body is not parseable (e.g. streaming
responses); non-OpenAI gateways undercount by design.
