package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentMetricsReportTransport(t *testing.T) {
	var got AppMetricsReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/agent/metrics" {
			t.Error("metrics report went to the wrong route")
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if strings.Contains(string(raw), "PASSWORD") || strings.Contains(string(raw), "SECRET") {
			t.Error("metrics report carried a secret-shaped value")
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Error("metrics report did not decode")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	report := AppMetricsReport{
		Protocol:   AppMetricsProtocol,
		ReportedAt: time.Now().UTC(),
		Apps: []AppMetrics{{
			DeploymentID: "dpl_" + strings.Repeat("a", 16), State: "running",
			RestartCount: 2, CPUPercent: 1.5, MemoryBytes: 13107200, MemoryLimitBytes: 536870912, VolumeBytes: 1200000000,
			CollectedAt: time.Now().UTC(),
		}},
	}
	if err := c.AgentMetricsReport(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if got.Protocol != AppMetricsProtocol || len(got.Apps) != 1 || got.Apps[0].MemoryBytes != 13107200 {
		t.Fatalf("metrics report lost its shape: %+v", got)
	}

	bad := report
	bad.Apps[0].DeploymentID = "other-app"
	if err := c.AgentMetricsReport(context.Background(), bad); err == nil {
		t.Fatal("an invalid deployment id was reported")
	}
	bad = report
	bad.Protocol = "runtime-v1"
	if err := c.AgentMetricsReport(context.Background(), bad); err == nil {
		t.Fatal("a foreign protocol was reported")
	}
}
