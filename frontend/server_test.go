package frontend

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/llm-d/llm-d-async/api"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	srv     *Server
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	backend *httptest.Server
	seen    chan *http.Request
}

func newTestEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	mr := miniredis.RunT(t)

	seen := make(chan *http.Request, 8)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"chat.completion","backend":true}`))
	}))
	t.Cleanup(backend.Close)

	cfg := &Config{
		RedisURL:   "redis://" + mr.Addr(),
		IGWBaseURL: backend.URL,
		Routes: []Route{
			{Model: "test-model", Queue: "team-default-queue", Tier: "interactive"},
		},
		Objectives: map[string]ObjectivePair{
			"interactive": {Reserved: "interactive-reserved", Overflow: "interactive-overflow"},
		},
		Quota: QuotaConfig{Limits: map[string]int{"limited-team": 1}},
	}
	cfg.applyDefaults()
	if mutate != nil {
		mutate(cfg)
	}

	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	require.NoError(t, err)
	return &testEnv{srv: srv, mr: mr, rdb: srv.rdb, backend: backend, seen: seen}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func post(h http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEnqueueMode(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	rec := post(h, "/v1/chat/completions",
		`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Team": "team-a"})

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id := resp["id"]
	require.NotEmpty(t, id)
	assert.Equal(t, "pending", resp["status"])
	assert.Equal(t, id, rec.Header().Get("X-Request-Id"))

	// The envelope landed on the routed queue with per-request result key,
	// endpoint, and tenant metadata.
	members, err := env.rdb.ZRange(t.Context(), "team-default-queue", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, members, 1)
	var envelope struct {
		Internal struct {
			ResultQueueName string `json:"result_queue_name"`
		} `json:"internal"`
		Data api.RequestMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(members[0]), &envelope))
	assert.Equal(t, resultKey(id), envelope.Internal.ResultQueueName)
	assert.Equal(t, "/v1/chat/completions", envelope.Data.Endpoint)
	assert.Equal(t, "team-a", envelope.Data.Metadata["team"])
	assert.Equal(t, "test-model", envelope.Data.Payload["model"])
	assert.Greater(t, envelope.Data.Deadline, time.Now().Unix())

	// Pending marker: the producer's active-token key exists.
	exists, err := env.rdb.Exists(t.Context(), api.RequestActiveTokenKey(id)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists)
}

func TestClientSuppliedID(t *testing.T) {
	env := newTestEnv(t, nil)
	rec := post(env.srv.Handler(), "/v1/completions",
		`{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Request-Id": "my-id-1"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), "my-id-1")
}

func storeResult(t *testing.T, env *testEnv, id string, res api.ResultMessage) {
	t.Helper()
	data, err := json.Marshal(res)
	require.NoError(t, err)
	require.NoError(t, env.rdb.LPush(t.Context(), resultKey(id), string(data)).Err())
	// Result flush also cleans up the active-token key.
	env.rdb.Del(t.Context(), api.RequestActiveTokenKey(id))
}

func TestFetchLifecycle(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	// Unknown id.
	req := httptest.NewRequest(http.MethodGet, "/v1/requests/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusGone, rec.Code)

	// Enqueue, then fetch while pending.
	rec = post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Request-Id": "fetch-1"})
	require.Equal(t, http.StatusAccepted, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/requests/fetch-1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), "pending")

	// Result arrives: fetch mirrors upstream status and body, idempotently.
	storeResult(t, env, "fetch-1", api.ResultMessage{ID: "fetch-1", StatusCode: 200, Payload: `{"done":true}`})
	for range 2 {
		req = httptest.NewRequest(http.MethodGet, "/v1/requests/fetch-1", nil)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"done":true}`, rec.Body.String())
	}

	// Eager cleanup, then gone.
	req = httptest.NewRequest(http.MethodDelete, "/v1/requests/fetch-1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/requests/fetch-1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestFetchErrorMapping(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()
	cases := []struct {
		code   string
		status int
	}{
		{api.ErrCodeDeadlineExceeded, http.StatusGatewayTimeout},
		{api.ErrCodeGateDropped, http.StatusTooManyRequests},
		{api.ErrCodeInvalidRequest, http.StatusBadRequest},
		{api.ErrCodeInferenceError, http.StatusBadGateway},
	}
	for i, tc := range cases {
		id := "err-" + tc.code
		storeResult(t, env, id, api.ResultMessage{ID: id, ErrorCode: tc.code, ErrorMessage: "boom"})
		req := httptest.NewRequest(http.MethodGet, "/v1/requests/"+id, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, tc.status, rec.Code, "case %d (%s)", i, tc.code)
		assert.Contains(t, rec.Body.String(), tc.code)
	}
}

func TestDirectModeLabelsAndProxies(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	rec := post(h, "/v1/chat/completions",
		`{"model":"test-model","messages":[]}`,
		map[string]string{"X-Team": "team-a"})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"backend":true`)

	backendReq := <-env.seen
	assert.Equal(t, "/v1/chat/completions", backendReq.URL.Path)
	assert.Equal(t, "interactive-reserved", backendReq.Header.Get("x-llm-d-inference-objective"))
	assert.Equal(t, "team-a", backendReq.Header.Get("x-llm-d-inference-fairness-id"))
}

func TestDirectModeOverflowClassification(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	// Fill limited-team's single reserved slot so the next request overflows.
	require.NoError(t, env.rdb.Set(t.Context(), "quota:team:limited-team", "1", 0).Err())

	rec := post(h, "/v1/chat/completions",
		`{"model":"test-model","messages":[]}`,
		map[string]string{"X-Team": "limited-team"})
	require.Equal(t, http.StatusOK, rec.Code)

	backendReq := <-env.seen
	assert.Equal(t, "interactive-overflow", backendReq.Header.Get("x-llm-d-inference-objective"))
}

func TestWaitModeReturnsResult(t *testing.T) {
	env := newTestEnv(t, func(c *Config) { c.WaitCapSeconds = 5 })
	h := env.srv.Handler()

	go func() {
		time.Sleep(300 * time.Millisecond)
		data, _ := json.Marshal(api.ResultMessage{ID: "wait-1", StatusCode: 200, Payload: `{"waited":true}`})
		env.rdb.LPush(t.Context(), resultKey("wait-1"), string(data))
	}()

	rec := post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "wait", "X-Request-Id": "wait-1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"waited":true}`, rec.Body.String())
}

func TestWaitModeCapFallsBackToPending(t *testing.T) {
	env := newTestEnv(t, func(c *Config) { c.WaitCapSeconds = 1 })
	start := time.Now()
	rec := post(env.srv.Handler(), "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "wait", "X-Request-Id": "wait-2"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), "pending")
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestValidation(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	rec := post(h, "/v1/chat/completions", `{"messages":[]}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "missing model")

	rec = post(h, "/v1/chat/completions", `not json`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "invalid json")

	rec = post(h, "/v1/chat/completions",
		`{"model":"test-model","stream":true}`, map[string]string{"X-AP-Mode": "enqueue"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "stream on queued mode")

	rec = post(h, "/v1/chat/completions",
		`{"model":"test-model"}`, map[string]string{"X-AP-Mode": "bogus"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown mode")
}

func TestModelsEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "test-model")
}
