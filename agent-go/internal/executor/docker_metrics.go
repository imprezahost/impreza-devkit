package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// CollectAppMetrics gathers one metrics point per managed app: container
// state, restart count, CPU percent and memory from `docker stats`, and the
// byte size of the app's volumes from one `docker system df -v`. The report
// carries numbers and states only — never environment, configuration or
// logs, so nothing in it can hold a secret. Read-only, with a hard budget.
func (d *Docker) CollectAppMetrics(ctx context.Context) *sdkclient.AppMetricsReport {
	report := &sdkclient.AppMetricsReport{Protocol: sdkclient.AppMetricsProtocol, ReportedAt: time.Now().UTC(), Apps: []sdkclient.AppMetrics{}}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	sizes := d.hostVolumeSizes(ctx)
	dir, err := os.Open(filepath.Join(d.StateDir, "apps"))
	if err != nil {
		return report
	}
	defer dir.Close()
	entries, err := dir.ReadDir(101)
	if err != nil && !errors.Is(err, io.EOF) {
		return report
	}
	for _, entry := range entries {
		if !entry.IsDir() || !runtimeDeploymentID.MatchString(entry.Name()) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		sampleCtx, stop := context.WithTimeout(ctx, 4*time.Second)
		report.Apps = append(report.Apps, d.collectAppMetrics(sampleCtx, entry.Name(), sizes))
		stop()
	}
	return report
}

// One app: state + restarts from inspect, CPU/memory from one stats call,
// volume bytes from the host-wide map collected once per cycle.
func (d *Docker) collectAppMetrics(ctx context.Context, id string, sizes map[string]int64) sdkclient.AppMetrics {
	m := sdkclient.AppMetrics{DeploymentID: id, State: "unknown", CollectedAt: time.Now().UTC()}
	idsOut, err := limitedRuntimeOutput(d.dockerCmd(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project="+strings.ToLower(id)), 16384)
	if err != nil {
		return m
	}
	ids := strings.Fields(string(idsOut))
	if len(ids) > 100 {
		return m
	}
	if len(ids) > 0 {
		// Explicit projection: state and restart count only — inspect output
		// carries the container's environment, which is never read here.
		const format = `{"Status":{{json .State.Status}},"Restarts":{{.RestartCount}}}`
		args := append([]string{"inspect", "--format", format}, ids...)
		output, err := limitedRuntimeOutput(d.dockerCmd(ctx, args...), 262144)
		if err != nil {
			return m
		}
		state := ""
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			var c struct {
				Status   string
				Restarts int
			}
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return m
			}
			m.RestartCount += c.Restarts
			state = mergeMetricState(state, c.Status)
		}
		if state != "" {
			m.State = state
		}
	}
	if m.State == "" {
		m.State = "exited"
	}
	if len(ids) > 0 && (m.State == "running" || m.State == "restarting") {
		if cpu, mem, limit, ok := d.appStats(ctx, ids); ok {
			m.CPUPercent = cpu
			m.MemoryBytes = mem
			m.MemoryLimitBytes = limit
		}
	}
	for _, line := range strings.Fields(string(mustRuntimeOutput(d.dockerCmd(ctx, "volume", "ls", "-q", "--filter", "name="+id+"_")))) {
		m.VolumeBytes += sizes[line]
	}
	return m
}

// State priority: a restarting container outranks running siblings; running
// outranks everything else; anything left is not serving.
func mergeMetricState(current, next string) string {
	rank := map[string]int{"restarting": 5, "running": 4, "paused": 3, "created": 2, "exited": 1, "dead": 1}
	if rank[next] > rank[current] {
		return next
	}
	return current
}

// docker stats --no-stream, one JSON object per line. Summed per app.
func (d *Docker) appStats(ctx context.Context, ids []string) (float64, int64, int64, bool) {
	args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, ids...)
	out, err := limitedRuntimeOutput(d.dockerCmd(ctx, args...), 262144)
	if err != nil {
		return 0, 0, 0, false
	}
	var cpu float64
	var mem, limit int64
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row struct {
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return 0, 0, 0, false
		}
		cpu += parsePercent(row.CPUPerc)
		used, total := parseMemUsage(row.MemUsage)
		mem += used
		limit += total
		seen = true
	}
	return cpu, mem, limit, seen
}

// docker system df -v, once per cycle: volume name → bytes.
func (d *Docker) hostVolumeSizes(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	dfCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := limitedRuntimeOutput(d.dockerCmd(dfCtx, "system", "df", "-v", "--format", "{{json .Volumes}}"), 4*1024*1024)
	if err != nil {
		return out
	}
	var volumes []struct {
		Name string `json:"Name"`
		Size string `json:"Size"`
	}
	if json.Unmarshal(raw, &volumes) != nil {
		return out
	}
	for _, v := range volumes {
		out[v.Name] = parseHumanBytes(v.Size)
	}
	return out
}

func parsePercent(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return v
}

// "12.5MiB / 512MiB" → (used, total). Docker prints IEC units.
func parseMemUsage(s string) (int64, int64) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	return parseHumanBytes(strings.TrimSpace(parts[0])), parseHumanBytes(strings.TrimSpace(parts[1]))
}

// Human byte sizes, decimal (kB/MB/GB) and binary (KiB/MiB/GiB) alike.
func parseHumanBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40}, {"TB", 1000_000_000_000},
		{"GiB", 1 << 30}, {"GB", 1000_000_000},
		{"MiB", 1 << 20}, {"MB", 1000_000},
		{"KiB", 1 << 10}, {"kB", 1000},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0
			}
			return int64(v * float64(u.mult))
		}
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func mustRuntimeOutput(cmd *exec.Cmd) []byte {
	out, _ := limitedRuntimeOutput(cmd, 262144)
	return out
}
