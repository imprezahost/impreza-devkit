package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// metricsBudgetScale stretches the collector's time budgets (the cycle,
// one app's sample, the volume sizes); the Windows tests raise it for their
// slower docker double.
var metricsBudgetScale = 1

func metricsBudget(d time.Duration) time.Duration { return d * time.Duration(metricsBudgetScale) }

// CollectAppMetrics gathers one metrics point per managed app: container
// state, restart count, CPU percent and memory from `docker stats`, and the
// byte size of the app's volumes from one `docker system df -v`. The report
// carries numbers and states only — never environment, configuration or
// logs, so nothing in it can hold a secret. Read-only, with a hard budget.
func (d *Docker) CollectAppMetrics(ctx context.Context) *sdkclient.AppMetricsReport {
	report := &sdkclient.AppMetricsReport{Protocol: sdkclient.AppMetricsProtocol, ReportedAt: time.Now().UTC(), Apps: []sdkclient.AppMetrics{}}
	ctx, cancel := context.WithTimeout(ctx, metricsBudget(25*time.Second))
	defer cancel()
	sizes := d.hostVolumeSizes(ctx)
	// Proxy counters: deployment-labeled deltas scraped from the
	// proxy container's loopback admin endpoint. Absent on old containers —
	// degrade silently.
	var proxyDeltas map[string]*sdkclient.AppProxyMetrics
	if d.Proxy != nil {
		proxyDeltas = d.Proxy.ScrapeMetrics(ctx)
	}
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
		sampleCtx, stop := context.WithTimeout(ctx, metricsBudget(4*time.Second))
		point := d.collectAppMetrics(sampleCtx, entry.Name(), sizes)
		if delta, ok := proxyDeltas[entry.Name()]; ok {
			point.Proxy = delta
		}
		report.Apps = append(report.Apps, point)
		stop()
	}
	return report
}

// One app: state + restarts from inspect, CPU/memory from one stats call,
// volume bytes from the host-wide map collected once per cycle.
func (d *Docker) collectAppMetrics(ctx context.Context, id string, sizes map[string]int64) sdkclient.AppMetrics {
	m := sdkclient.AppMetrics{DeploymentID: id, State: "unknown", CollectedAt: time.Now().UTC()}
	// The app's containers are those of its Compose project, the one every
	// Compose call pins: a stack whose compose file names its project
	// literally is not labelled with the deployment ID, and reading none of
	// its containers would report a running app as exited.
	project, err := composeProject(d.appDir(id))
	if err != nil {
		return m
	}
	idsOut, err := limitedRuntimeOutput(d.dockerCmd(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project="+project), 16384)
	if err != nil {
		return m
	}
	ids := strings.Fields(string(idsOut))
	if len(ids) > 100 {
		return m
	}
	live := false
	var all []metricContainer
	if len(ids) > 0 {
		// Explicit projection: state, restart count, the memory limit, the
		// mounts and the Compose labels the verdict needs — inspect output
		// carries the container's environment, which is never read here.
		const format = `{"ID":{{json .ID}},"Service":{{json (index .Config.Labels "com.docker.compose.service")}},"Status":{{json .State.Status}},"ExitCode":{{.State.ExitCode}},"Restarts":{{.RestartCount}},"OneOff":{{json (index .Config.Labels "com.docker.compose.oneoff")}},"DependsOn":{{json (index .Config.Labels "com.docker.compose.depends_on")}},"Memory":{{.HostConfig.Memory}},"Mounts":{{json .Mounts}}}`
		args := append([]string{"inspect", "--format", format}, ids...)
		output, err := limitedRuntimeOutput(d.dockerCmd(ctx, args...), 262144)
		if err != nil {
			return m
		}
		var containers []metricContainer
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			var c metricContainer
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return m
			}
			m.RestartCount += c.Restarts
			live = live || c.Status == "running" || c.Status == "restarting"
			all = append(all, c)
			if !strings.EqualFold(c.OneOff, "true") {
				containers = append(containers, c)
			}
		}
		m.State = metricState(d.metricServices(ctx, id), containers)
	} else {
		// No container at all: down. This read "unknown" before, a state
		// the server discards, so no point ever reached the down alert.
		m.State = "exited"
	}
	// A partly stopped stack still reports what its running containers use.
	if live {
		if cpu, rows, ok := d.appStats(ctx, ids); ok {
			m.CPUPercent = cpu
			m.MemoryBytes, m.MemoryLimitBytes = appMemory(all, rows)
		}
	}
	// The project's named volumes by Compose's own label: a name prefix
	// missed the importer's ${DEPLOYMENT_ID}-vol-* volumes and every project
	// named literally.
	for _, line := range strings.Fields(string(mustRuntimeOutput(d.dockerCmd(ctx, "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+project)))) {
		m.VolumeBytes += sizes[line]
	}
	m.VolumeBytes += appBindBytes(d.appDir(id), all)
	return m
}

type metricContainer struct {
	ID        string
	Service   string
	Status    string
	ExitCode  int
	Restarts  int
	OneOff    string
	DependsOn string
	// Memory is the container's own memory limit, 0 when it has none.
	Memory int64
	Mounts []struct {
		Type   string
		Source string
	}
}

// appMemory is what the app's running containers use together and the most
// they may use: the sum of their own limits, or, when one of them runs
// without a limit, what that one may take — the host's memory, which Docker
// reports as its limit — counted once. Adding the host's memory for every
// unlimited container reported 15.5 GiB on a server with 8 GB.
func appMemory(containers []metricContainer, rows map[string]statsRow) (used, limit int64) {
	var limited, unlimited int64
	anyUnlimited := false
	for _, c := range containers {
		if c.Status != "running" && c.Status != "restarting" {
			continue
		}
		var row statsRow
		found := false
		for short, r := range rows {
			if short != "" && strings.HasPrefix(c.ID, short) {
				row, found = r, true
				break
			}
		}
		if !found {
			continue
		}
		used += row.used
		if c.Memory == 0 {
			anyUnlimited = true
			if row.limit > unlimited {
				unlimited = row.limit
			}
		} else if row.limit > 0 {
			limited += row.limit
		} else {
			limited += c.Memory
		}
	}
	if anyUnlimited {
		return used, unlimited
	}
	return used, limited
}

// Bind-mounted data: most catalog apps keep their data in ./data, a
// bind mount that Docker's volume accounting never sees, so their volume
// bytes read 0. Walking a large tree takes long, so the walk runs in the
// background — one at a time for the whole agent, capped in entries and
// time — and the collector reports the last measurement of the same mounts;
// the first cycles after a start, or after the mounts change, may still
// read 0. Only directories inside the app's own directory count.
const (
	bindUsageRefresh = 10 * time.Minute
	bindUsageBudget  = 60 * time.Second
	bindUsageEntries = 1000000
)

type bindMeasure struct {
	roots    string
	bytes    int64
	measured time.Time
}

var bindUsage = struct {
	sync.Mutex
	walking bool
	byApp   map[string]bindMeasure
}{byApp: map[string]bindMeasure{}}

func appBindBytes(appDir string, containers []metricContainer) int64 {
	roots := bindRoots(appDir, containers)
	if len(roots) == 0 {
		return 0
	}
	key := strings.Join(roots, "\x00")
	bindUsage.Lock()
	defer bindUsage.Unlock()
	last, ok := bindUsage.byApp[appDir]
	ok = ok && last.roots == key
	if (!ok || time.Since(last.measured) > bindUsageRefresh) && !bindUsage.walking {
		bindUsage.walking = true
		go func() {
			bytes := walkBindRoots(roots)
			bindUsage.Lock()
			bindUsage.byApp[appDir] = bindMeasure{roots: key, bytes: bytes, measured: time.Now()}
			bindUsage.walking = false
			bindUsage.Unlock()
		}()
	}
	if ok {
		return last.bytes
	}
	return 0
}

// bindRoots is the set of bind-mount sources inside appDir, nested ones
// folded into the directory that contains them. Both sides are compared as
// they resolve now: a link along a source's path (a container can write
// one into a mounted directory) must not take the walk outside the app.
func bindRoots(appDir string, containers []metricContainer) []string {
	base, err := filepath.EvalSymlinks(filepath.Clean(appDir))
	if err != nil {
		return nil
	}
	var sources []string
	for _, c := range containers {
		for _, mount := range c.Mounts {
			if mount.Type != "bind" || mount.Source == "" {
				continue
			}
			source, err := filepath.EvalSymlinks(filepath.Clean(mount.Source))
			if err != nil {
				continue
			}
			if source == base || strings.HasPrefix(source, base+string(filepath.Separator)) {
				sources = append(sources, source)
			}
		}
	}
	sort.Strings(sources)
	var roots []string
	for _, source := range sources {
		inside := false
		for _, root := range roots {
			if source == root || strings.HasPrefix(source, root+string(filepath.Separator)) {
				inside = true
				break
			}
		}
		if !inside {
			roots = append(roots, source)
		}
	}
	return roots
}

// walkBindRoots sums the sizes of the regular files under roots, never
// following a link, within bindUsageEntries entries and bindUsageBudget. A
// walk cut short reports what it saw: a lower bound, not a guess.
func walkBindRoots(roots []string) int64 {
	deadline := time.Now().Add(bindUsageBudget)
	var total int64
	entries := 0
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			entries++
			if entries > bindUsageEntries || time.Now().After(deadline) {
				return fs.SkipAll
			}
			if entry.Type().IsRegular() {
				if info, err := entry.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}

// metricServices lists the services the stack declares, as the runtime
// collector reads them; nil when Compose cannot say, and the verdict then
// judges the services it sees.
func (d *Docker) metricServices(ctx context.Context, id string) []string {
	pinned, err := composeCommand(d.appDir(id), "config", "--services")
	if err != nil {
		return nil
	}
	config := d.dockerCmd(ctx, pinned...)
	config.Dir = d.appDir(id)
	out, err := limitedRuntimeOutput(config, 16384)
	if err != nil {
		return nil
	}
	services := strings.Fields(string(out))
	if len(services) > 64 {
		return nil
	}
	return services
}

// metricState folds one stack into the single state its metrics point
// carries, the one the `down` alert reads: running only when every service
// the stack expects to keep running has a running container. Before,
// any running container made the whole app running, so a stopped web next
// to a live database never opened the alert. A declared one-shot that
// finished — exit 0, depended on with service_completed_successfully, the
// runtime verdict's mark — is not a stopped service. A restarting
// container still outranks everything, and a service that is down reports
// its own state (paused, created, exited or dead; exited when it has no
// container at all).
func metricState(services []string, containers []metricContainer) string {
	if len(containers) == 0 {
		return "exited"
	}
	oneShot := map[string]bool{}
	byService := map[string][]metricContainer{}
	for _, c := range containers {
		if c.Status == "restarting" {
			return "restarting"
		}
		for _, service := range dependsOnCompleted(c.DependsOn) {
			oneShot[service] = true
		}
		byService[c.Service] = append(byService[c.Service], c)
	}
	if len(services) == 0 {
		for service := range byService {
			services = append(services, service)
		}
	}
	down := ""
	for _, service := range services {
		up := false
		state := ""
		for _, c := range byService[service] {
			if c.Status == "running" || (c.Status == "exited" && c.ExitCode == 0 && oneShot[service]) {
				up = true
			}
			state = mergeMetricState(state, c.Status)
		}
		if state == "" {
			state = "exited"
		}
		if !up {
			down = mergeMetricState(down, state)
		}
	}
	if down != "" {
		return down
	}
	return "running"
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

// statsRow is one container's memory use and limit, as docker stats reads them.
type statsRow struct {
	used  int64
	limit int64
}

// docker stats --no-stream, one JSON object per line: CPU summed per app,
// memory per container (by the short ID stats prints), for appMemory.
func (d *Docker) appStats(ctx context.Context, ids []string) (float64, map[string]statsRow, bool) {
	args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, ids...)
	out, err := limitedRuntimeOutput(d.dockerCmd(ctx, args...), 262144)
	if err != nil {
		return 0, nil, false
	}
	var cpu float64
	rows := map[string]statsRow{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row struct {
			ID       string `json:"ID"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return 0, nil, false
		}
		cpu += parsePercent(row.CPUPerc)
		used, total := parseMemUsage(row.MemUsage)
		rows[row.ID] = statsRow{used: used, limit: total}
	}
	return cpu, rows, len(rows) > 0
}

// docker system df -v, once per cycle: volume name → bytes.
func (d *Docker) hostVolumeSizes(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	dfCtx, cancel := context.WithTimeout(ctx, metricsBudget(8*time.Second))
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
