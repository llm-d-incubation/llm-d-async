package frontend

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// serverMetrics holds the frontend's Prometheus collectors. A per-server
// registry (rather than the global one) keeps tests with multiple servers
// from colliding on registration.
type serverMetrics struct {
	registry        *prometheus.Registry
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inflight        *prometheus.GaugeVec
	quotaTotal      *prometheus.CounterVec
	enqueueFailures prometheus.Counter
	fetchTotal      *prometheus.CounterVec
	waitOutcomes    *prometheus.CounterVec
}

func newServerMetrics() *serverMetrics {
	m := &serverMetrics{
		registry: prometheus.NewRegistry(),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llm_d_async_frontend", Name: "requests_total",
			Help: "Inference requests by serving mode and HTTP status code.",
		}, []string{"mode", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "llm_d_async_frontend", Name: "request_duration_seconds",
			Help:    "Inference request duration by serving mode, as observed at the frontend.",
			Buckets: []float64{.05, .1, .2, .4, .8, 1.5, 3, 6, 12, 30, 60},
		}, []string{"mode"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "llm_d_async_frontend", Name: "inflight_requests",
			Help: "Inference requests currently being served, by mode.",
		}, []string{"mode"}),
		quotaTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llm_d_async_frontend", Name: "quota_classifications_total",
			Help: "Quota classifications by outcome (reserved, overflow, error).",
		}, []string{"classification"}),
		enqueueFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "llm_d_async_frontend", Name: "enqueue_failures_total",
			Help: "Requests that failed to enqueue onto the broker.",
		}),
		fetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llm_d_async_frontend", Name: "fetch_total",
			Help: "Result fetches by outcome (ready, pending, gone, error).",
		}, []string{"outcome"}),
		waitOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llm_d_async_frontend", Name: "wait_outcomes_total",
			Help: "Wait mode outcomes (result delivered on the connection, or timeout fallback to 202).",
		}, []string{"outcome"}),
	}
	m.registry.MustRegister(m.requestsTotal, m.requestDuration, m.inflight,
		m.quotaTotal, m.enqueueFailures, m.fetchTotal, m.waitOutcomes)
	return m
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// statusRecorder captures the response status for metrics while remaining
// transparent to the reverse proxy's streaming path: Unwrap exposes the
// underlying writer to http.ResponseController (used by ReverseProxy for
// flushing), and Flush is forwarded directly.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// observeRequest records one served inference request.
func (m *serverMetrics) observeRequest(mode string, status int, start time.Time) {
	if status == 0 {
		status = http.StatusOK
	}
	m.requestsTotal.WithLabelValues(mode, strconv.Itoa(status)).Inc()
	m.requestDuration.WithLabelValues(mode).Observe(time.Since(start).Seconds())
}
