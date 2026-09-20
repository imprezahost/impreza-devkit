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
	CollectedAt      time.Time `json:"collected_at"`
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
