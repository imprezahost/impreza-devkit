package client

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// AppMetricsProtocol versions the per-app metrics report. The report carries
// numbers and container states only — there is no field that could hold an
// environment value or a secret.
const AppMetricsProtocol = "app-metrics-v1"

// AppMetrics is one app in one collection cycle.
type AppMetrics struct {
	DeploymentID     string    `json:"deployment_id"`
	State            string    `json:"state"` // running | restarting | exited | dead | created | paused
	RestartCount     int       `json:"restart_count"`
	CPUPercent       float64   `json:"cpu_pct"`
	MemoryBytes      int64     `json:"memory_bytes"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	VolumeBytes      int64     `json:"volume_bytes"`
	// Proxy carries the deployment's proxy counters for the cycle. Nil on
	// agents before the proxy-metrics middleware existed — old reports are
	// unchanged on the wire.
	Proxy            *AppProxyMetrics `json:"proxy,omitempty"`
	CollectedAt      time.Time        `json:"collected_at"`
}

// AppMetricsReport is one metrics cycle for the whole agent.
type AppMetricsReport struct {
	Protocol   string       `json:"protocol"`
	ReportedAt time.Time    `json:"reported_at"`
	Apps       []AppMetrics `json:"apps"`
}

var appMetricsIDPattern = regexp.MustCompile(`^dpl_[a-f0-9]{16}$`)

// AgentMetricsReport posts one metrics cycle to the control plane.
func (c *Client) AgentMetricsReport(ctx context.Context, r AppMetricsReport) error {
	if r.Protocol != AppMetricsProtocol {
		return errors.New("metrics report must carry the app-metrics-v1 protocol")
	}
	for _, app := range r.Apps {
		if !appMetricsIDPattern.MatchString(app.DeploymentID) {
			return errors.New("metrics report carries an invalid deployment id")
		}
	}
	return c.Post(ctx, "/v1/agent/metrics", r, nil)
}

// ProxyMetricsProtocol versions the proxy request metrics the agent folds
// into its per-minute report. Deltas per report cycle; the counters
// behind them never carry a visitor identity — deployment id only.
const ProxyMetricsProtocol = "proxy-metrics-v1"

// AppProxyMetrics is one deployment's proxy counters for one cycle.
type AppProxyMetrics struct {
	Requests      int64   `json:"requests"`
	Status1xx     int64   `json:"status_1xx,omitempty"`
	Status2xx     int64   `json:"status_2xx,omitempty"`
	Status3xx     int64   `json:"status_3xx,omitempty"`
	Status4xx     int64   `json:"status_4xx,omitempty"`
	Status5xx     int64   `json:"status_5xx,omitempty"`
	LatencyCount  int64   `json:"latency_count,omitempty"`
	LatencyMsSum  float64 `json:"latency_ms_sum,omitempty"`
	LatencyP50Ms  float64 `json:"latency_p50_ms,omitempty"`
	LatencyP95Ms  float64 `json:"latency_p95_ms,omitempty"`
	LatencyP99Ms  float64 `json:"latency_p99_ms,omitempty"`
	BytesIn       int64   `json:"bytes_in,omitempty"`
	BytesOut      int64   `json:"bytes_out,omitempty"`
	ShieldChallenges int64 `json:"shield_challenges,omitempty"`
	ShieldPassed     int64 `json:"shield_passed,omitempty"`
	ShieldRateLimited int64 `json:"shield_rate_limited,omitempty"`
}
