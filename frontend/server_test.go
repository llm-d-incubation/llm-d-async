package frontend

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	assert.Equal(t, resultKey("team-a", id), envelope.Internal.ResultQueueName)
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

func storeResult(t *testing.T, env *testEnv, tenant, id string, res api.ResultMessage) {
	t.Helper()
	data, err := json.Marshal(res)
	require.NoError(t, err)
	require.NoError(t, env.rdb.LPush(t.Context(), resultKey(tenant, id), string(data)).Err())
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
	storeResult(t, env, defaultTenant, "fetch-1", api.ResultMessage{ID: "fetch-1", StatusCode: 200, Payload: `{"done":true}`})
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
		storeResult(t, env, defaultTenant, id, api.ResultMessage{ID: id, ErrorCode: tc.code, ErrorMessage: "boom"})
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
		env.rdb.LPush(t.Context(), resultKey(defaultTenant, "wait-1"), string(data))
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

func TestCrossTenantFetchDenied(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	// team-a enqueues and its result lands.
	rec := post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Team": "team-a", "X-Request-Id": "xt-1"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	storeResult(t, env, "team-a", "xt-1", api.ResultMessage{ID: "xt-1", StatusCode: 200, Payload: `{"secret":true}`})

	// team-b (or no tenant) cannot read it.
	req := httptest.NewRequest(http.MethodGet, "/v1/requests/xt-1", nil)
	req.Header.Set("X-Team", "team-b")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	assert.NotEqual(t, http.StatusOK, rec2.Code)
	assert.NotContains(t, rec2.Body.String(), "secret")

	// The owner can.
	req = httptest.NewRequest(http.MethodGet, "/v1/requests/xt-1", nil)
	req.Header.Set("X-Team", "team-a")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req)
	assert.Equal(t, http.StatusOK, rec3.Code)
	assert.Contains(t, rec3.Body.String(), "secret")
}

func TestWaitModeNotifyWakeup(t *testing.T) {
	env := newTestEnv(t, func(c *Config) { c.WaitCapSeconds = 10; c.WakeupMode = "notify" })
	require.NotNil(t, env.srv.waiter, "notify mode should create the waiter")
	h := env.srv.Handler()

	go func() {
		time.Sleep(250 * time.Millisecond)
		data, _ := json.Marshal(api.ResultMessage{ID: "ntf-1", StatusCode: 200, Payload: `{"notified":true}`})
		key := resultKey(defaultTenant, "ntf-1")
		env.rdb.LPush(context.Background(), key, string(data))
		// miniredis does not emit keyspace notifications; simulate the
		// server's notification publish.
		env.rdb.Publish(context.Background(), "__keyspace@0__:"+key, "lpush")
	}()

	start := time.Now()
	rec := post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "wait", "X-Request-Id": "ntf-1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"notified":true}`, rec.Body.String())
	// Woken by the notification, well before the 2s backup poll tick.
	assert.Less(t, time.Since(start), 1500*time.Millisecond)
}

func TestPerModeTimeoutDefaults(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	// Enqueue mode defaults to hour-scale deadlines.
	rec := post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Request-Id": "tmo-enq"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	members, err := env.rdb.ZRange(t.Context(), "team-default-queue", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, members, 1)
	var envlp struct {
		Data api.RequestMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(members[0]), &envlp))
	remaining := envlp.Data.Deadline - time.Now().Unix()
	assert.Greater(t, remaining, int64(3000), "enqueue deadline should default to hour scale")

	// Enqueue max cap is per mode too: a client asking beyond it is clamped.
	rec = post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue", "X-Request-Id": "tmo-enq2", "X-Request-Timeout-Seconds": "999999"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	members, err = env.rdb.ZRange(t.Context(), "team-default-queue", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, members, 2)
	require.NoError(t, json.Unmarshal([]byte(members[1]), &envlp))
	assert.LessOrEqual(t, envlp.Data.Deadline-time.Now().Unix(), int64(86400))
}

func TestMetricsEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	h := env.srv.Handler()

	post(h, "/v1/completions", `{"model":"test-model","prompt":"hi"}`,
		map[string]string{"X-AP-Mode": "enqueue"})
	post(h, "/v1/chat/completions", `{"model":"test-model","messages":[]}`,
		map[string]string{"X-Team": "team-a"})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `llm_d_async_frontend_requests_total{code="202",mode="enqueue"} 1`)
	assert.Contains(t, body, `llm_d_async_frontend_requests_total{code="200",mode="direct"} 1`)
	assert.Contains(t, body, `llm_d_async_frontend_quota_classifications_total{classification="reserved"} 1`)
	assert.Contains(t, body, "llm_d_async_frontend_request_duration_seconds")
}

func TestModelsEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "test-model")
}

func TestCancelledErrorMapsTo499(t *testing.T) {
	env := newTestEnv(t, nil)
	storeResult(t, env, defaultTenant, "cx-1", api.ResultMessage{ID: "cx-1", ErrorCode: api.ErrCodeCancelled, ErrorMessage: "cancelled"})
	req := httptest.NewRequest(http.MethodGet, "/v1/requests/cx-1", nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, 499, rec.Code)
}

func TestWaitModeDisconnectCancels(t *testing.T) {
	env := newTestEnv(t, func(c *Config) { c.WaitCapSeconds = 30 })
	rec := &recordingSubmitter{inner: env.srv.sub}
	env.srv.sub = rec

	// A request context that dies shortly after enqueue simulates a client
	// disconnect while waiting.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		strings.NewReader(`{"model":"test-model","prompt":"hi"}`)).WithContext(ctx)
	req.Header.Set("X-AP-Mode", "wait")
	req.Header.Set("X-Request-Id", "gone-1")
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()

	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)

	deadline := time.After(3 * time.Second)
	for {
		if rec.cancelled("gone-1") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected CancelRequests for abandoned wait request")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type recordingSubmitter struct {
	inner submitter
	mu    sync.Mutex
	ids   []string
}

func (r *recordingSubmitter) SubmitRequest(ctx context.Context, req api.Request) error {
	return r.inner.SubmitRequest(ctx, req)
}

func (r *recordingSubmitter) CancelRequests(ctx context.Context, ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ids...)
	return r.inner.CancelRequests(ctx, ids)
}

func (r *recordingSubmitter) cancelled(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.ids {
		if v == id {
			return true
		}
	}
	return false
}
