package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-d/llm-d-async/api"
	"github.com/redis/go-redis/v9"
)

// resultKey is the per-request result destination. Messages enqueued by the
// frontend set result_queue_name to this key, and the AP's result writer
// delivers there. Reads are non-destructive: the key expires via its TTL.
func resultKey(id string) string {
	return resultKeyPrefix + id
}

// requestState is the outcome of looking up a request id.
type requestState int

const (
	stateReady requestState = iota
	statePending
	stateUnknown
)

// lookupResult reads a request's result without consuming it. Pending is
// detected via the producer-maintained active-token key, which exists from
// submit until the result is flushed.
func lookupResult(ctx context.Context, rdb *redis.Client, id string) (requestState, *api.ResultMessage, error) {
	vals, err := rdb.LRange(ctx, resultKey(id), 0, 0).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return stateUnknown, nil, fmt.Errorf("failed to read result: %w", err)
	}
	if len(vals) > 0 {
		var res api.ResultMessage
		if err := json.Unmarshal([]byte(vals[0]), &res); err != nil {
			return stateUnknown, nil, fmt.Errorf("failed to parse stored result: %w", err)
		}
		return stateReady, &res, nil
	}

	exists, err := rdb.Exists(ctx, api.RequestActiveTokenKey(id)).Result()
	if err != nil {
		return stateUnknown, nil, fmt.Errorf("failed to check request state: %w", err)
	}
	if exists > 0 {
		return statePending, nil, nil
	}
	return stateUnknown, nil, nil
}

// openAIError is the OpenAI error response envelope.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorBody{Message: msg, Type: errType, Code: code}})
}

// writeResult maps a ResultMessage onto the HTTP response. StatusCode > 0
// mirrors the upstream status and body verbatim. Error codes map per the
// design: GATE_DROPPED -> 429, DEADLINE_EXCEEDED -> 504, INVALID_REQUEST ->
// 400, everything else -> 502, wrapped in the OpenAI error envelope.
func writeResult(w http.ResponseWriter, res *api.ResultMessage) {
	if res.StatusCode > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write([]byte(res.Payload))
		return
	}
	switch res.ErrorCode {
	case api.ErrCodeGateDropped:
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", res.ErrorCode, res.ErrorMessage)
	case api.ErrCodeDeadlineExceeded:
		writeOpenAIError(w, http.StatusGatewayTimeout, "timeout_error", res.ErrorCode, res.ErrorMessage)
	case api.ErrCodeInvalidRequest:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", res.ErrorCode, res.ErrorMessage)
	default:
		writeOpenAIError(w, http.StatusBadGateway, "api_error", res.ErrorCode, res.ErrorMessage)
	}
}
