package executor

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var runtimeDeploymentID = regexp.MustCompile(`^dpl_[a-zA-Z0-9_-]{1,28}$`)

// CollectRuntime is read-only and has a single eight-second budget. A partial
// snapshot never implies missing applications are stopped. Only managed state
// directories are sampled; Docker environment and healthcheck logs stay local.
func (d *Docker) CollectRuntime(ctx context.Context) *sdkclient.RuntimeSnapshot {
	snapshot := &sdkclient.RuntimeSnapshot{Protocol: "runtime-v1", Deployments: []sdkclient.RuntimeObservation{}, Complete: true}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// Hidden-service daemon health rides the same snapshot (bounded, no
	// descriptors, no addresses — version/image/running only).
	if d.Tor != nil {
		snapshot.Tor = d.Tor.Runtime(ctx)
	}
	dir, err := os.Open(filepath.Join(d.StateDir, "apps"))
	if err != nil {
		snapshot.Complete = errors.Is(err, os.ErrNotExist)
		return snapshot
	}
	defer dir.Close()
	entries, err := dir.ReadDir(101)
	if err != nil && !errors.Is(err, io.EOF) {
		snapshot.Complete = false
		return snapshot
	}
	if len(entries) > 100 {
		entries = entries[:100]
		snapshot.Complete = false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !runtimeDeploymentID.MatchString(entry.Name()) {
			continue
		}
		if ctx.Err() != nil {
			snapshot.Complete = false
			break
		}
		sampleCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		observation := d.collectRuntimeApp(sampleCtx, entry.Name())
		stop()
		snapshot.Deployments = append(snapshot.Deployments, observation)
	}
	return snapshot
}

// limitedRuntimeOutput bounds both memory and the amount of inspect data read.
// Stderr is discarded: it may include local paths or interpolated configuration.
type runtimeOutputBuffer struct {
	data     []byte
	limit    int
	exceeded bool
}

func (b *runtimeOutputBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if len(p) > remaining {
		b.data = append(b.data, p[:remaining]...)
		b.exceeded = true
		return remaining, errors.New("runtime output limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func limitedRuntimeOutput(cmd *exec.Cmd, limit int64) ([]byte, error) {
	output := &runtimeOutputBuffer{limit: int(limit)}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	// A Docker CLI plugin can inherit stdout. Bound pipe cleanup even when
	// that child outlives the cancelled CLI process.
	cmd.WaitDelay = 200 * time.Millisecond
	err := cmd.Run()
	if output.exceeded {
		return nil, errors.New("runtime output limit")
	}
	return output.data, err
}

type runtimeContainer struct {
	Service  string
	Status   string
	Health   string
	ExitCode int
	OneOff   string
}

func (d *Docker) collectRuntimeApp(ctx context.Context, id string) sdkclient.RuntimeObservation {
	observation := sdkclient.RuntimeObservation{DeploymentID: id, State: "unknown", Reason: "collection_failed"}
	// Timestamp includes collection time, not just successful report delivery.
	observation.ObservedAt = time.Now().UTC()
	config := d.dockerCmd(ctx, "compose", "config", "--services")
	config.Dir = d.appDir(id)
	serviceOutput, err := limitedRuntimeOutput(config, 16384)
	if err != nil {
		return observation
	}
	services := strings.Fields(string(serviceOutput))
	if len(services) == 0 || len(services) > 64 {
		return observation
	}
	idsOut, err := limitedRuntimeOutput(d.dockerCmd(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project="+strings.ToLower(id)), 16384)
	if err != nil {
		return observation
	}
	ids := strings.Fields(string(idsOut))
	if len(ids) > 100 {
		return observation
	}
	var containers []runtimeContainer
	if len(ids) > 0 {
		// Explicit projection prevents secrets from entering memory or the report.
		const format = `{"Service":{{json (index .Config.Labels "com.docker.compose.service")}},"Status":{{json .State.Status}},"Health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},"ExitCode":{{.State.ExitCode}},"OneOff":{{json (index .Config.Labels "com.docker.compose.oneoff")}}}`
		args := append([]string{"inspect", "--format", format}, ids...)
		output, err := limitedRuntimeOutput(d.dockerCmd(ctx, args...), 262144)
		if err != nil {
			return observation
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			var c runtimeContainer
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return observation
			}
			if !strings.EqualFold(c.OneOff, "true") {
				containers = append(containers, c)
			}
		}
	}
	observation.Counts, observation.State = runtimeVerdict(services, containers)
	observation.Reason = ""
	return observation
}

func runtimeVerdict(services []string, containers []runtimeContainer) (sdkclient.RuntimeCounts, string) {
	counts := sdkclient.RuntimeCounts{ExpectedServices: len(services)}
	seen := map[string]bool{}
	for _, c := range containers {
		counts.Total++
		seen[c.Service] = true
		switch c.Status {
		case "running":
			counts.Running++
			switch c.Health {
			case "healthy":
				counts.Healthy++
			case "unhealthy":
				counts.Unhealthy++
			case "starting":
				counts.Starting++
			case "":
			default:
				counts.Failed++
			}
		case "exited":
			counts.Stopped++
			if c.ExitCode != 0 {
				counts.Failed++
			}
		case "created":
			counts.Stopped++
		default:
			counts.Failed++ // paused, dead, restarting and unknown states need attention
		}
	}
	for _, service := range services {
		if !seen[service] {
			counts.MissingServices++
		}
	}
	if counts.Total == 0 {
		return counts, "stopped"
	}
	if counts.Failed > 0 || counts.Unhealthy > 0 || ((counts.MissingServices > 0 || counts.Stopped > 0) && counts.Running > 0) {
		return counts, "degraded"
	}
	if counts.Running == 0 {
		return counts, "stopped"
	}
	if counts.Starting > 0 {
		return counts, "starting"
	}
	// A stopped declared service makes a mixed stack degraded, including an init
	// service. Without a healthcheck a running container is not proven healthy.
	if counts.Healthy == counts.Running && counts.MissingServices == 0 {
		return counts, "healthy"
	}
	return counts, "running"
}
