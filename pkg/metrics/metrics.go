// Package metrics provides metrics registration for the async processor.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// SchedulerSubsystem is the metric prefix of the package.
	SchedulerSubsystem = "llm_d_async"
)

// RegistererGatherer combines both parts of the API of a Prometheus
// registry, both the Registerer and the Gatherer interfaces.
type RegistererGatherer interface {
	prometheus.Registerer
	prometheus.Gatherer
}

var registry RegistererGatherer = prometheus.NewRegistry()

var (
	Retries = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_request_retries_total",
		Help: "Total number of async request retries.",
	})

	AsyncReqs = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_request_total",
		Help: "Total number of async requests.",
	})
	ExceededDeadlineReqs = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_ßexceeded_deadline_requests_total",
		Help: "Total number of async requests that exceeded their deadline.",
	})
	FailedReqs = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_failed_requests_total",
		Help: "Total number of async requests that failed.",
	})
	SuccessfulReqs = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_successful_requests_total",
		Help: "Total number of async requests that succeeded.",
	})
	SheddedRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: SchedulerSubsystem, Name: "async_shedded_requests_total",
		Help: "Total number of async requests that were shedded.",
	})
)

// GetCollectors returns all custom collectors for the async processor.
func GetAsyncProcessorCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		Retries, AsyncReqs, ExceededDeadlineReqs, FailedReqs, SuccessfulReqs, SheddedRequests,
	}
}

var registerMetrics sync.Once

// Register all metrics.
func Register(customCollectors ...prometheus.Collector) {
	registerMetrics.Do(func() {
		for _, collector := range customCollectors {
			registry.MustRegister(collector)
		}
	})
}
