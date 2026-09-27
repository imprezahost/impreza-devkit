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
	// DependsOn is the raw com.docker.compose.depends_on label, which
	// Compose writes as service:condition:restart entries joined by ','
	// (init:service_completed_successfully:false): how this container's
	// service declared its dependencies, including condition
	// service_completed_successfully — the explicit mark that a service is
	// expected to exit 0 and stay down.
	DependsOn string
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
		const format = `{"Service":{{json (index .Config.Labels "com.docker.compose.service")}},"Status":{{json .State.Status}},"Health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},"ExitCode":{{.State.ExitCode}},"OneOff":{{json (index .Config.Labels "com.docker.compose.oneoff")}},"DependsOn":{{json (index .Config.Labels "com.docker.compose.depends_on")}}}`
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

// dependsOnCompleted parses the com.docker.compose.depends_on label into
// the services declared completable (condition service_completed_successfully).
// Compose writes service:condition:restart entries joined by ','
// (init:service_completed_successfully:false, measured on compose 2.40.3);
// service names cannot contain ':' or ','. A JSON object
// ({"init":{"condition":"service_completed_successfully"}}) is accepted
// defensively too. An unparsable label marks nothing — the unmarked
// direction is the safe one (stops stay unexpected).
func dependsOnCompleted(label string) []string {
	if label == "" {
		return nil
	}
	var marked []string
	if strings.HasPrefix(label, "{") {
		var deps map[string]struct {
			Condition string `json:"condition"`
		}
		if err := json.Unmarshal([]byte(label), &deps); err != nil {
			return nil
		}
		for service, dep := range deps {
			if dep.Condition == "service_completed_successfully" {
				marked = append(marked, service)
			}
		}
		return marked
	}
	for _, entry := range strings.Split(label, ",") {
		parts := strings.Split(entry, ":")
		if len(parts) >= 2 && parts[0] != "" && parts[1] == "service_completed_successfully" {
			marked = append(marked, parts[0])
		}
	}
	return marked
}

func runtimeVerdict(services []string, containers []runtimeContainer) (sdkclient.RuntimeCounts, string) {
	counts := sdkclient.RuntimeCounts{ExpectedServices: len(services)}
	// A container that exited 0 counts as completed ONLY when the stack
	// declares the service one-shot: another service depends on it with
	// condition service_completed_successfully (the tls-init/synapse-init
	// shape). Anything else that stopped — a long-running service parked
	// with `docker stop` exits 0 too — is an unexpected stop, and must
	// keep degrading the stack.
	oneShot := map[string]bool{}
	for _, c := range containers {
		for _, service := range dependsOnCompleted(c.DependsOn) {
			oneShot[service] = true
		}
	}
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
			} else if oneShot[c.Service] {
				// A declared one-shot init job that finished its work
				// . It stays inside Stopped for wire compatibility
				// with older control planes; Completed marks the subset
				// the verdict must not treat as a degraded container.
				counts.Completed++
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
	// Completed one-shots (exited 0) do not count towards the unexpected
	// stop that degrades a mixed stack — the settle gate accepts exit 0 on
	// purpose, and the runtime collector must agree with it.
	unexpectedStopped := counts.Stopped - counts.Completed
	if counts.Failed > 0 || counts.Unhealthy > 0 || ((counts.MissingServices > 0 || unexpectedStopped > 0) && counts.Running > 0) {
		return counts, "degraded"
	}
	if counts.Running == 0 {
		return counts, "stopped"
	}
	if counts.Starting > 0 {
		return counts, "starting"
	}
	// A missing declared service or a genuinely stopped one still makes a
	// mixed stack degraded. Without a healthcheck a running container is
	// not proven healthy.
	if counts.Healthy == counts.Running && counts.MissingServices == 0 {
		return counts, "healthy"
	}
	return counts, "running"
}
