// Package frontend implements an OpenAI API compatible HTTP frontend for the
// async processor. It serves three modes selected per request by header, with
// a configurable default for requests that carry none: enqueue (202 + fetch
// later), wait (hold the connection until the result lands), and direct
// (label and proxy straight to the inference gateway).
// See docs/openai-frontend-design.md.
package frontend

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Mode selects how a request is served.
type Mode string

const (
	// ModeDirect labels the request and proxies it to the inference gateway.
	ModeDirect Mode = "direct"
	// ModeEnqueue enqueues onto the broker and returns 202 with the request id.
	ModeEnqueue Mode = "enqueue"
	// ModeWait enqueues and holds the connection until the result lands or the
	// wait cap expires (then falls back to the enqueue response).
	ModeWait Mode = "wait"
)

// ObjectivePair holds the InferenceObjective names stamped for a tier,
// selected by the request's quota classification.
type ObjectivePair struct {
	Reserved string `json:"reserved,omitempty"`
	Overflow string `json:"overflow,omitempty"`
}

// Route maps a request to a broker queue and tier. Empty Model or Tenant
// matches anything; the first matching route wins.
type Route struct {
	Model  string `json:"model,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	Queue  string `json:"queue,omitempty"`
	Tier   string `json:"tier,omitempty"`
}

// TimeoutBounds holds the default and maximum request deadline for a mode.
type TimeoutBounds struct {
	DefaultSeconds int64 `json:"defaultSeconds,omitempty"`
	MaxSeconds     int64 `json:"maxSeconds,omitempty"`
}

// QuotaConfig configures direct-mode quota classification. It mirrors the
// redis-quota gate's concurrency mode and key scheme so the frontend and the
// AP's queue gates draw from the same counters. Queued modes are classified
// by the AP's gates at dequeue and are not counted here.
type QuotaConfig struct {
	// Prefix + Attribute + ":" + tenant forms the counter key, matching the
	// redis-quota gate's fmt.Sprintf("%s%s:%s", prefix, attribute, value).
	Prefix    string `json:"prefix,omitempty"`
	Attribute string `json:"attribute,omitempty"`
	// Limits maps tenant name to reserved concurrency. Tenants without an
	// entry are always classified reserved.
	Limits map[string]int `json:"limits,omitempty"`
	// WindowSeconds is the counter TTL, matching the gate's window (default 300).
	WindowSeconds int `json:"windowSeconds,omitempty"`
}

// Config is the frontend configuration, loaded from a YAML file.
type Config struct {
	ListenAddr string `json:"listenAddr,omitempty"`
	RedisURL   string `json:"redisURL"`
	IGWBaseURL string `json:"igwBaseURL"`

	// TenantHeader names the header carrying the tenant key (default X-Team).
	TenantHeader string `json:"tenantHeader,omitempty"`
	// ModeHeader selects the serving mode per request (default X-AP-Mode). A
	// request without the header is served in DefaultMode.
	ModeHeader string `json:"modeHeader,omitempty"`
	// DefaultMode serves requests that carry no mode header (default direct,
	// so a stock SDK pointed at the frontend behaves like the gateway).
	// Unrecognized values fail startup rather than silently falling back.
	DefaultMode Mode `json:"defaultMode,omitempty"`
	// RequestIDHeader lets clients supply their own request id (default X-Request-Id).
	RequestIDHeader string `json:"requestIDHeader,omitempty"`
	// TimeoutHeader lets clients request a deadline in seconds (default
	// X-Request-Timeout-Seconds), capped by MaxTimeoutSeconds.
	TimeoutHeader string `json:"timeoutHeader,omitempty"`

	// DefaultTimeoutSeconds and MaxTimeoutSeconds are the fallback bounds for
	// modes without an entry in Timeouts.
	DefaultTimeoutSeconds int64 `json:"defaultTimeoutSeconds,omitempty"`
	MaxTimeoutSeconds     int64 `json:"maxTimeoutSeconds,omitempty"`
	// Timeouts overrides the deadline bounds per mode. Enqueue defaults much
	// higher than the connection-bound modes: deferred work legitimately
	// outlives any connection.
	Timeouts map[Mode]TimeoutBounds `json:"timeouts,omitempty"`
	// WaitCapSeconds bounds how long ModeWait holds a connection.
	WaitCapSeconds int64 `json:"waitCapSeconds,omitempty"`
	// MaxBodyBytes caps request body size (default 10 MiB).
	MaxBodyBytes int64 `json:"maxBodyBytes,omitempty"`

	// Routes select the broker queue and tier per request. The frontend's
	// queues must not set result_queue_name in the AP's queue config, so the
	// per-request result key routing applies.
	Routes       []Route `json:"routes,omitempty"`
	DefaultQueue string  `json:"defaultQueue,omitempty"`
	DefaultTier  string  `json:"defaultTier,omitempty"`

	Quota QuotaConfig `json:"quota,omitempty"`

	// ForwardHeaders lists client headers copied onto queued messages and
	// forwarded at dispatch (direct mode forwards all headers natively).
	// Defaults to the gateway SLO ordering headers. Identity headers
	// (objective, fairness) are always stamped by the frontend and cannot be
	// forwarded from clients.
	ForwardHeaders []string `json:"forwardHeaders,omitempty"`

	// WakeupMode selects how wait mode learns a result has landed.
	// "notify": multiplexed keyspace-notification wake-up (requires Redis
	// notify-keyspace-events to include K and l). "poll": periodic
	// non-destructive polling. "auto" (default): notify when the Redis
	// server's config supports it, else poll.
	WakeupMode string `json:"wakeupMode,omitempty"`

	// Objectives maps tier -> objective names by classification. Stamped on
	// direct-mode requests as ObjectiveHeader. Tiers without an entry are not
	// stamped (the EPP then uses its default band).
	Objectives      map[string]ObjectivePair `json:"objectives,omitempty"`
	ObjectiveHeader string                   `json:"objectiveHeader,omitempty"`
	FairnessHeader  string                   `json:"fairnessHeader,omitempty"`
}

const (
	defaultTenant            = "default"
	resultKeyPrefix          = "results:req:"
	defaultListenAddr        = ":8080"
	defaultTimeoutSecs       = 60
	defaultMaxTimeout        = 600
	defaultEnqueueTimeout    = 3600
	defaultEnqueueMaxTimeout = 86400
	defaultWaitCapSecs       = 55
	defaultMaxBodyBytes      = 10 << 20
)

// timeoutBounds resolves the deadline bounds for a mode, falling back to the
// flat DefaultTimeoutSeconds/MaxTimeoutSeconds fields.
func (c *Config) timeoutBounds(m Mode) TimeoutBounds {
	b := c.Timeouts[m]
	if b.DefaultSeconds <= 0 {
		b.DefaultSeconds = c.DefaultTimeoutSeconds
	}
	if b.MaxSeconds <= 0 {
		b.MaxSeconds = c.MaxTimeoutSeconds
	}
	return b
}

func (c *Config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.TenantHeader == "" {
		c.TenantHeader = "X-Team"
	}
	if c.ModeHeader == "" {
		c.ModeHeader = "X-AP-Mode"
	}
	if c.DefaultMode == "" {
		c.DefaultMode = ModeDirect
	}
	if c.RequestIDHeader == "" {
		c.RequestIDHeader = "X-Request-Id"
	}
	if c.TimeoutHeader == "" {
		c.TimeoutHeader = "X-Request-Timeout-Seconds"
	}
	if c.DefaultTimeoutSeconds <= 0 {
		c.DefaultTimeoutSeconds = defaultTimeoutSecs
	}
	if c.MaxTimeoutSeconds <= 0 {
		c.MaxTimeoutSeconds = defaultMaxTimeout
	}
	if c.Timeouts == nil {
		c.Timeouts = map[Mode]TimeoutBounds{}
	}
	// Enqueue mode defaults to hour-scale deadlines: nothing is holding a
	// connection, and deferred work legitimately takes longer than any
	// connection-bound request would.
	if _, ok := c.Timeouts[ModeEnqueue]; !ok {
		c.Timeouts[ModeEnqueue] = TimeoutBounds{DefaultSeconds: defaultEnqueueTimeout, MaxSeconds: defaultEnqueueMaxTimeout}
	}
	if c.WaitCapSeconds <= 0 {
		c.WaitCapSeconds = defaultWaitCapSecs
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.DefaultQueue == "" {
		c.DefaultQueue = "request-sortedset"
	}
	if c.Quota.Prefix == "" {
		c.Quota.Prefix = "quota:"
	}
	if c.Quota.Attribute == "" {
		c.Quota.Attribute = "team"
	}
	if c.Quota.WindowSeconds <= 0 {
		c.Quota.WindowSeconds = 300
	}
	if c.WakeupMode == "" {
		c.WakeupMode = "auto"
	}
	if c.ForwardHeaders == nil {
		c.ForwardHeaders = []string{"x-llm-d-slo-ttft-ms", "x-llm-d-slo-tpot-ms"}
	}
	if c.ObjectiveHeader == "" {
		c.ObjectiveHeader = "x-llm-d-inference-objective"
	}
	if c.FairnessHeader == "" {
		c.FairnessHeader = "x-llm-d-inference-fairness-id"
	}
}

func (c *Config) validate() error {
	if c.RedisURL == "" {
		return fmt.Errorf("redisURL is required")
	}
	if c.IGWBaseURL == "" {
		return fmt.Errorf("igwBaseURL is required")
	}
	switch c.DefaultMode {
	case ModeDirect, ModeEnqueue, ModeWait:
	default:
		return fmt.Errorf("defaultMode must be %q, %q, or %q, got %q", ModeDirect, ModeEnqueue, ModeWait, c.DefaultMode)
	}
	return nil
}

// LoadConfig reads and validates a YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path from trusted CLI flag
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// route returns the queue and tier for a (model, tenant) pair.
func (c *Config) route(model, tenant string) (queue, tier string) {
	for _, r := range c.Routes {
		if (r.Model == "" || r.Model == model) && (r.Tenant == "" || r.Tenant == tenant) {
			q, t := r.Queue, r.Tier
			if q == "" {
				q = c.DefaultQueue
			}
			if t == "" {
				t = c.DefaultTier
			}
			return q, t
		}
	}
	return c.DefaultQueue, c.DefaultTier
}
