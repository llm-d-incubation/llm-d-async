# Requests and Results

## Message Formats

### Request Messages

The async processor expects request messages to have the following format:

```json
{
    "id": "unique identifier for result mapping",
    "created": "created timestamp in Unix seconds",
    "deadline": "deadline in Unix seconds",
    "payload": {"regular inference payload"}
}
```

**Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier for result mapping (required) |
| `created` | int64 | Created timestamp in Unix seconds |
| `deadline` | int64 | Deadline in Unix seconds (required, must be positive) |
| `payload` | object | Inference request payload |
| `metadata` | map[string]string | Optional caller-supplied pass-through data (e.g. tracing IDs, user labels) |
| `headers` | map[string]string | Optional HTTP headers forwarded on the outgoing dispatch request |
| `endpoint` | string | Optional per-request dispatch path; overrides the queue-level default when set |

**Example:**

```json
{
    "id": "19933123533434",
    "created": 1764044000,
    "deadline": 1764045130,
    "payload": {"model": "food-review", "prompt": "hi", "max_tokens": 10, "temperature": 0},
    "metadata": {"user": "batch-job-42"}
}
```

Producers handle wrapping these into the internal wire format used for persistence and routing.

### Result Messages

Results are written to the result queue/topic with the following structure:

```json
{
    "id": "id mapped to the request",
    "status_code": 200,
    "payload": "inference result payload"
}
```

**Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | The originating request's `id` |
| `status_code` | int | HTTP status code of the inference response. Present (> 0) whenever an HTTP response was received — including error statuses. |
| `payload` | string | The response body. On non-HTTP failures it carries a JSON `{"error": "<message>"}` object. |
| `error_code` | string | Set for non-HTTP failures (when `status_code` is absent): `DEADLINE_EXCEEDED`, `CANCELLED`, `GATE_DROPPED`, `GATE_ERROR`, `INFERENCE_ERROR`, `INVALID_REQUEST` |
| `error_message` | string | Human-readable description accompanying `error_code` |

### Internal Wire Format

On the broker itself, messages travel in a tagged envelope carrying the request kind and internal routing (producers create this automatically):

```json
{"request_kind": "plain", "data": { "id": "...", "deadline": 1764045130, "payload": {} }}
```

You only need this format when publishing directly to the broker, bypassing a producer (see [Development](../README.md#development)).


## Request Body Transform Reference

Some providers need a different body shape at dispatch time — for example multi-modal endpoints (Whisper transcription, OCR) that expect `multipart/form-data` with a `url` field rather than JSON. Request body-transform plugins handle this without special-casing the worker: they rewrite the outgoing body and `Content-Type` based on per-message `metadata`, and the default JSON path is preserved byte-for-byte when no plugin applies.

Transforms are configured with `--transform-config-file`, pointing at a JSON object that groups plugins by direction:

```json
{
  "requestTransforms": [
    {
      "name": "whisper-multipart",
      "type": "gcs_uri_multipart",
      "parameters": { "providers": ["whisper"] }
    }
  ]
}
```

Each entry has a unique `name`, a registered plugin `type`, and opaque `parameters`. Unknown top-level fields are rejected. When the flag is empty, no transforms are loaded and behavior is unchanged.

With the Helm chart, set `ap.transformConfig` to this same object; the chart renders it to a config file and wires `--transform-config-file` automatically:

```yaml
ap:
  transformConfig:
    requestTransforms:
      - name: "whisper-multipart"
        type: "gcs_uri_multipart"
        parameters:
          providers: ["whisper"]
```

### `gcs_uri_multipart` plugin

Rewrites a JSON body into `multipart/form-data` for endpoints that take a signed object URL. Because producers can't put raw media bytes on the broker, the queued `payload` carries a signed URL (e.g. a GCS V4 signed URL) in a `gcs_uri` field.

- **Activation:** the message's `metadata.provider` must match one of the configured `providers`, and the `payload` must contain a non-empty `gcs_uri`. Otherwise the default JSON path is used unchanged.
- **Transform:** writes the `gcs_uri` value as a `url` form field (a plain field, not a file upload), passes the remaining payload fields through as form fields, and drops `gcs_uri`. A non-empty `file_base64` is rejected as a fatal, non-retryable error (inline media is not supported on this path).
- **Preflight:** parses the signed URL's expiry (V4 `X-Goog-Date` + `X-Goog-Expires`, or V2 `Expires`); if it expires at or before the message deadline, the request fails fatally before dispatch so the broker doesn't retry a request that cannot succeed.
