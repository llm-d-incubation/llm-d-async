package frontend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-async/api"
	"github.com/llm-d/llm-d-async/producer"
	"github.com/redis/go-redis/v9"
)

// submitter is the subset of producer.Producer the server needs.
type submitter interface {
	SubmitRequest(ctx context.Context, req api.Request) error
	CancelRequests(ctx context.Context, requestIDs []string) error
}

// waitPollInterval is the poll cadence when the multiplexed wake-up is
// unavailable (Redis without keyspace notifications, or wakeupMode: poll).
const waitPollInterval = 200 * time.Millisecond

// waitBackupPollInterval is the slow safety poll under the notify wake-up:
// keyspace notifications are fire and forget, so a lost notification is
// recovered on the next backup tick rather than never.
const waitBackupPollInterval = 2 * time.Second

// Server is the OpenAI-compatible frontend HTTP server.
type Server struct {
	cfg     *Config
	rdb     *redis.Client
	sub     submitter
	proxy   *httputil.ReverseProxy
	quota   *quotaClassifier
	logger  *slog.Logger
	metrics *serverMetrics
	waiter  *resultWaiter // nil = poll mode
}

// NewServer builds a Server from config, connecting to Redis and preparing
// the gateway proxy.
func NewServer(cfg *Config, logger *slog.Logger) (*Server, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redisURL: %w", err)
	}
	rdb := redis.NewClient(opts)

	prod, err := producer.NewRedisSortedSetProducer(producer.RedisSortedSetConfig{
		RequestQueueName: cfg.DefaultQueue,
		// Placeholder: every message sets its own per-request result key.
		ResultQueueName: resultKeyPrefix + "unrouted",
	}, producer.WithRedisClient(rdb))
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	target, err := url.Parse(cfg.IGWBaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse igwBaseURL: %w", err)
	}

	s := &Server{
		cfg:     cfg,
		rdb:     rdb,
		sub:     prod,
		quota:   &quotaClassifier{rdb: rdb, cfg: cfg.Quota},
		logger:  logger,
		metrics: newServerMetrics(),
	}

	switch cfg.WakeupMode {
	case "poll":
	case "notify":
		s.waiter = newResultWaiter(context.Background(), rdb, opts.DB, logger)
	default: // auto
		detectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		enabled := notificationsEnabled(detectCtx, rdb)
		cancel()
		if enabled {
			s.waiter = newResultWaiter(context.Background(), rdb, opts.DB, logger)
			logger.Info("wait mode using keyspace-notification wake-up")
		} else {
			logger.Warn("Redis keyspace notifications not enabled (notify-keyspace-events needs K and l); wait mode falls back to polling")
		}
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		// Flush immediately so SSE streaming passes through.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				writeOpenAIError(w, http.StatusGatewayTimeout, "timeout_error", api.ErrCodeDeadlineExceeded, "request deadline exceeded")
				return
			}
			writeOpenAIError(w, http.StatusBadGateway, "api_error", api.ErrCodeInferenceError, fmt.Sprintf("gateway error: %v", err))
		},
	}
	return s, nil
}

// Handler returns the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleFetch)
	mux.HandleFunc("DELETE /v1/requests/{id}", s.handleDelete)
	mux.HandleFunc("POST /v1/", s.handleInference)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", s.metrics.handler())
	return mux
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	seen := map[string]bool{}
	models := []model{}
	for _, rt := range s.cfg.Routes {
		if rt.Model == "" || seen[rt.Model] {
			continue
		}
		seen[rt.Model] = true
		models = append(models, model{ID: rt.Model, Object: "model", OwnedBy: "llm-d-async"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func (s *Server) tenantOf(r *http.Request) string {
	if t := r.Header.Get(s.cfg.TenantHeader); t != "" {
		return t
	}
	return defaultTenant
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, res, err := lookupResult(r.Context(), s.rdb, s.tenantOf(r), id)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "LOOKUP_FAILED", err.Error())
		return
	}
	switch state {
	case stateReady:
		s.metrics.fetchTotal.WithLabelValues("ready").Inc()
		w.Header().Set(s.cfg.RequestIDHeader, id)
		writeResult(w, res)
	case statePending:
		s.metrics.fetchTotal.WithLabelValues("pending").Inc()
		writePending(w, id)
	default:
		s.metrics.fetchTotal.WithLabelValues("gone").Inc()
		writeOpenAIError(w, http.StatusGone, "invalid_request_error", "UNKNOWN_REQUEST",
			"request id is unknown, expired, or already deleted")
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.rdb.Del(r.Context(), resultKey(s.tenantOf(r), r.PathValue("id"))).Err(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writePending(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "pending"})
}

// inferenceRequest carries the per-request state extracted by validation.
type inferenceRequest struct {
	body    []byte
	payload map[string]any
	model   string
	stream  bool
	tenant  string
	mode    Mode
	id      string
	timeout time.Duration
}

func (s *Server) parseInference(r *http.Request) (*inferenceRequest, int, string) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge, "request body too large or unreadable"
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, http.StatusBadRequest, "request body is not valid JSON"
	}
	model, _ := payload["model"].(string)
	if model == "" {
		return nil, http.StatusBadRequest, "model is required"
	}
	stream, _ := payload["stream"].(bool)

	mode := s.cfg.DefaultMode
	switch strings.ToLower(r.Header.Get(s.cfg.ModeHeader)) {
	case "":
	case string(ModeDirect):
		mode = ModeDirect
	case string(ModeEnqueue):
		mode = ModeEnqueue
	case string(ModeWait):
		mode = ModeWait
	default:
		return nil, http.StatusBadRequest, fmt.Sprintf("unknown %s value", s.cfg.ModeHeader)
	}
	if stream && mode != ModeDirect {
		return nil, http.StatusBadRequest, "stream is only supported in direct mode"
	}

	tenant := r.Header.Get(s.cfg.TenantHeader)
	if tenant == "" {
		tenant = defaultTenant
	}

	id := r.Header.Get(s.cfg.RequestIDHeader)
	if id == "" {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, http.StatusInternalServerError, "failed to generate request id"
		}
		id = hex.EncodeToString(buf[:])
	}

	bounds := s.cfg.timeoutBounds(mode)
	timeoutSecs := bounds.DefaultSeconds
	if v := r.Header.Get(s.cfg.TimeoutHeader); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			timeoutSecs = parsed
		}
	}
	if timeoutSecs > bounds.MaxSeconds {
		timeoutSecs = bounds.MaxSeconds
	}

	return &inferenceRequest{
		body:    body,
		payload: payload,
		model:   model,
		stream:  stream,
		tenant:  tenant,
		mode:    mode,
		id:      id,
		timeout: time.Duration(timeoutSecs) * time.Second,
	}, 0, ""
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}

	req, status, msg := s.parseInference(r)
	if status != 0 {
		s.metrics.observeRequest("invalid", status, start)
		writeOpenAIError(rec, status, "invalid_request_error", api.ErrCodeInvalidRequest, msg)
		return
	}
	mode := string(req.mode)
	s.metrics.inflight.WithLabelValues(mode).Inc()
	defer func() {
		s.metrics.inflight.WithLabelValues(mode).Dec()
		s.metrics.observeRequest(mode, rec.status, start)
	}()
	rec.Header().Set(s.cfg.RequestIDHeader, req.id)

	if req.mode == ModeDirect {
		s.serveDirect(rec, r, req)
		return
	}
	s.serveQueued(rec, r, req)
}

// serveDirect labels the request (quota classification, objective and
// fairness headers) and proxies it to the gateway with a context deadline.
// The quota slot is held until the response is fully written, mirroring the
// redis-quota gate's release-on-completion semantics.
func (s *Server) serveDirect(w http.ResponseWriter, r *http.Request, req *inferenceRequest) {
	classification, release, err := s.quota.classify(r.Context(), req.tenant)
	if err != nil {
		s.logger.Warn("quota classification failed open", "tenant", req.tenant, "error", err)
		s.metrics.quotaTotal.WithLabelValues("error").Inc()
	} else {
		s.metrics.quotaTotal.WithLabelValues(string(classification)).Inc()
	}
	defer release()

	_, tier := s.cfg.route(req.model, req.tenant)

	ctx, cancel := context.WithTimeout(r.Context(), req.timeout)
	defer cancel()

	out := r.Clone(ctx)
	out.Body = io.NopCloser(bytes.NewReader(req.body))
	out.ContentLength = int64(len(req.body))
	// Identity headers are frontend-owned: strip client-supplied values
	// unconditionally so priority cannot be self-assigned, even for tiers
	// without a configured objective mapping.
	out.Header.Del(s.cfg.ObjectiveHeader)
	out.Header.Del(s.cfg.FairnessHeader)
	if pair, ok := s.cfg.Objectives[tier]; ok {
		objective := pair.Reserved
		if classification == ClassificationOverflow {
			objective = pair.Overflow
		}
		if objective != "" {
			out.Header.Set(s.cfg.ObjectiveHeader, objective)
		}
	}
	out.Header.Set(s.cfg.FairnessHeader, req.tenant)
	s.proxy.ServeHTTP(w, out)
}

// serveQueued enqueues onto the broker with a per-request result key. Mode
// enqueue responds 202 immediately. Mode wait polls the result key up to the
// wait cap, then falls back to the 202 response.
func (s *Server) serveQueued(w http.ResponseWriter, r *http.Request, req *inferenceRequest) {
	queue, _ := s.cfg.route(req.model, req.tenant)
	now := time.Now()

	metadata := map[string]string{s.cfg.Quota.Attribute: req.tenant}
	if tp := r.Header.Get("traceparent"); tp != "" {
		metadata["traceparent"] = tp
	}
	var fwd map[string]string
	for _, h := range s.cfg.ForwardHeaders {
		if v := r.Header.Get(h); v != "" {
			if fwd == nil {
				fwd = map[string]string{}
			}
			fwd[h] = v
		}
	}

	msg := &api.RedisRequest{
		RequestMessage: api.RequestMessage{
			ID:       req.id,
			Created:  now.Unix(),
			Deadline: now.Add(req.timeout).Unix(),
			Payload:  req.payload,
			Metadata: metadata,
			Headers:  fwd,
			Endpoint: r.URL.Path,
		},
		RequestQueueName: queue,
		ResultQueueName:  resultKey(req.tenant, req.id),
	}
	if err := s.sub.SubmitRequest(r.Context(), msg); err != nil {
		s.metrics.enqueueFailures.Inc()
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error", "ENQUEUE_FAILED",
			fmt.Sprintf("failed to enqueue request: %v", err))
		return
	}

	if req.mode == ModeEnqueue {
		writePending(w, req.id)
		return
	}
	s.waitForResult(w, r, req)
}

func (s *Server) waitForResult(w http.ResponseWriter, r *http.Request, req *inferenceRequest) {
	waitCap := time.Duration(s.cfg.WaitCapSeconds) * time.Second
	if req.timeout < waitCap {
		waitCap = req.timeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), waitCap)
	defer cancel()

	// Multiplexed wake-up: subscribe before checking so a result landing
	// between check and park still notifies. Falls back to polling when the
	// waiter is unavailable, or per registration error.
	var wake <-chan struct{}
	pollEvery := waitPollInterval
	if s.waiter != nil {
		if ch, cleanup, err := s.waiter.register(ctx, resultKey(req.tenant, req.id)); err == nil {
			defer cleanup()
			wake = ch
			pollEvery = waitBackupPollInterval
		} else {
			s.logger.Warn("wake-up registration failed, polling instead", "error", err)
		}
	}

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		state, res, err := lookupResult(ctx, s.rdb, req.tenant, req.id)
		if err == nil && state == stateReady {
			s.metrics.waitOutcomes.WithLabelValues("result").Inc()
			writeResult(w, res)
			return
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				// Client disconnected: nobody will fetch this result, so
				// cancel pre-dispatch (best effort, per the producer's
				// cancellation contract). A client retry with the same id
				// re-submits cleanly: SubmitRequest clears stale markers.
				s.metrics.waitOutcomes.WithLabelValues("cancelled").Inc()
				cancelCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancelFn()
				if err := s.sub.CancelRequests(cancelCtx, []string{req.id}); err != nil {
					s.logger.Warn("failed to cancel abandoned request", "id", req.id, "error", err)
				}
				return
			}
			// Wait cap reached with the client still connected: fall back to
			// the enqueue response. The request stays queued and fetchable.
			s.metrics.waitOutcomes.WithLabelValues("timeout").Inc()
			writePending(w, req.id)
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}
