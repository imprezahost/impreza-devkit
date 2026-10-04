package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// A fake host for ready-swap: containers, the proxy upstream and the HTTP
// answers of each version. After every mutation it checks the one property
// the feature promises: the route points at a running container whose
// version answers an accepted status (a request at that instant succeeds).

type swapCrash struct{}

type fakeSwapCtr struct {
	version string
	status  string
	ip      string
}

type swapWorld struct {
	t        *testing.T
	d        *Docker
	id       string
	appDir   string
	ctrs     map[string]*fakeSwapCtr
	upstream string
	// liveUpstream, when set, is what the running proxy serves (it can lag
	// the fragment); empty means the two agree.
	liveUpstream string
	// liveStuck keeps the running proxy where it is across reloads.
	liveStuck  bool
	answers    map[string]int // version -> HTTP status
	crashes    map[string]bool
	resolved   string
	failRoute  map[string]bool
	ops        []string
	opTimes    []time.Time
	crashAt    int
	crashAfter bool
	now        time.Time
	violations []string
	lenient    bool
	nextIP     int
	previous   *runtimeRelease
}

const swapPrevImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newSwapWorld(t *testing.T) *swapWorld {
	t.Helper()
	return newSwapWorldOn(t, &Docker{StateDir: t.TempDir()}, "dpl_"+strings.Repeat("s", 16))
}

func newSwapWorldOn(t *testing.T, d *Docker, id string) *swapWorld {
	t.Helper()
	w := &swapWorld{t: t, id: id, answers: map[string]int{"prev": 200, "new": 200}, crashes: map[string]bool{}, failRoute: map[string]bool{}, now: time.Unix(1_800_000_000, 0), nextIP: 3}
	d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	w.d = d
	w.appDir = w.d.appDir(w.id)
	if err := os.MkdirAll(filepath.Join(w.appDir, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	w.ctrs = map[string]*fakeSwapCtr{w.id + "-app": {version: "prev", status: "running", ip: "10.0.0.2"}}
	w.upstream = w.id + "-app"
	w.resolved = `{"name":"` + w.id + `","services":{"app":{"image":"img:new","container_name":"` + w.id + `-app","networks":{"default":null,"impreza-proxy":null},"environment":{"K":"v"},"restart":"unless-stopped"}},"networks":{"impreza-proxy":{"external":true}}}`
	prevCompose := `{"services":{"app":{"image":"` + swapPrevImage + `","container_name":"` + w.id + `-app","networks":{"default":null,"impreza-proxy":null},"pull_policy":"never"}}}`
	w.previous = &runtimeRelease{Metadata: sdkclient.DeploymentRelease{ID: "rel_20260930T000000.000000000_aaaaaaaaaaaaaaaa", ImageIDs: map[string]string{"app": swapPrevImage}}, Compose: json.RawMessage(prevCompose), Env: []byte("K=prev\n"), EnvExists: true}
	raw, _ := json.Marshal(w.previous)
	if err := os.WriteFile(filepath.Join(w.appDir, "releases", w.previous.Metadata.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// The deploy already wrote the new configuration before replacement.
	_ = os.WriteFile(filepath.Join(w.appDir, "compose.yaml"), []byte("services:\n  app:\n    image: img:new\n"), 0600)
	_ = os.WriteFile(filepath.Join(w.appDir, ".env"), []byte("K=new\n"), 0600)
	w.install(w.d)
	return w
}

func (w *swapWorld) install(d *Docker) {
	d.readySwapRouter = w
	d.readySwapCompose = w.compose
	d.readySwapInspect = w.inspect
	d.readySwapProbe = w.probe
	d.readySwapSleep = func(ctx context.Context, wait time.Duration) bool { w.now = w.now.Add(wait); return ctx.Err() == nil }
	d.readySwapClock = func() time.Time { return w.now }
}

func (w *swapWorld) tick(op string) {
	w.ops = append(w.ops, op)
	w.opTimes = append(w.opTimes, w.now)
	if !w.crashAfter && w.crashAt > 0 && len(w.ops) == w.crashAt {
		panic(swapCrash{})
	}
}

// done is called once an operation took effect.
func (w *swapWorld) done() {
	if w.crashAfter && w.crashAt > 0 && len(w.ops) == w.crashAt {
		panic(swapCrash{})
	}
}

// serving: a request through the route right now succeeds.
func (w *swapWorld) check(after string) {
	c := w.ctrs[w.upstream]
	ok := c != nil && c.status == "running" && !w.crashes[c.version] && w.answers[c.version] >= 200 && w.answers[c.version] < 400
	if !ok && !w.lenient {
		w.violations = append(w.violations, fmt.Sprintf("after %s the route (%s) does not serve", after, w.upstream))
	}
}

func (w *swapWorld) swapFileImage(file, service string) string {
	raw, err := os.ReadFile(filepath.Join(w.appDir, file))
	if err != nil {
		w.t.Fatalf("swap file: %v", err)
	}
	var m struct {
		Services map[string]struct{ Image string } `json:"services"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		w.t.Fatalf("swap file json: %v", err)
	}
	if m.Services[service].Image == swapPrevImage {
		return "prev"
	}
	return "new"
}

func (w *swapWorld) compose(_ context.Context, _ string, file string, args ...string) ([]byte, error) {
	switch args[0] {
	case "config":
		return []byte(w.resolved), nil
	case "logs":
		return []byte("app log line"), nil
	}
	op := strings.Join(args, " ")
	w.tick("compose " + op)
	switch {
	case args[0] == "up" && args[len(args)-1] == "app-next":
		if file != readySwapComposeName {
			w.t.Fatalf("next started from %s", file)
		}
		v := w.swapFileImage(file, "app-next")
		st := "running"
		if w.crashes[v] {
			st = "exited"
		}
		w.nextIP++
		w.ctrs[w.id+"-app-next"] = &fakeSwapCtr{version: v, status: st, ip: fmt.Sprintf("10.0.0.%d", w.nextIP)}
	case args[0] == "up" && args[len(args)-1] == "app":
		v := w.swapFileImage(file, "app")
		st := "running"
		if w.crashes[v] || w.crashes["app:"+v] {
			st = "exited"
		}
		w.nextIP++
		w.ctrs[w.id+"-app"] = &fakeSwapCtr{version: v, status: st, ip: fmt.Sprintf("10.0.0.%d", w.nextIP)}
	case args[0] == "rm" && args[len(args)-1] == "app-next":
		delete(w.ctrs, w.id+"-app-next")
	case args[0] == "stop" && args[len(args)-1] == "app":
		if c := w.ctrs[w.id+"-app"]; c != nil {
			c.status = "exited"
		}
	default:
		w.t.Fatalf("unexpected compose %s", op)
	}
	w.check("compose " + op)
	w.done()
	return nil, nil
}

func (w *swapWorld) inspect(_ context.Context, name string) (swapContainer, error) {
	c := w.ctrs[name]
	if c == nil {
		return swapContainer{}, nil
	}
	return swapContainer{Exists: true, Status: c.status, IP: c.ip, ExitCode: map[bool]int{true: 1}[c.status == "exited"]}, nil
}

func (w *swapWorld) probe(_ context.Context, url, host string) (int, error) {
	if host != "app.example.test" {
		w.t.Fatalf("probe without the app's Host: %q", host)
	}
	for _, c := range w.ctrs {
		if strings.HasPrefix(url, "http://"+c.ip+":8080/healthz") {
			if c.status != "running" {
				return 0, errors.New("connection refused")
			}
			return w.answers[c.version], nil
		}
	}
	return 0, errors.New("no route to host")
}

func (w *swapWorld) UpstreamHost(string) (string, string, error) { return w.upstream, "8080", nil }

// In the fake host the fragment and the running proxy agree unless a test
// splits them (liveUpstream).
func (w *swapWorld) LiveUpstream(context.Context, string) (string, error) {
	if w.liveUpstream != "" {
		return w.liveUpstream, nil
	}
	return w.upstream, nil
}

func (w *swapWorld) RetargetUpstream(_ context.Context, _ string, to string) error {
	w.tick("route " + strings.TrimPrefix(to, w.id))
	if w.failRoute[to] {
		return errors.New("injected reload rejection")
	}
	w.upstream = to
	if !w.liveStuck {
		// The reload took: the running proxy now follows the fragment.
		w.liveUpstream = ""
	}
	w.check("route " + to)
	w.done()
	return nil
}

func (w *swapWorld) payload() sdkclient.DeployPayload {
	return sdkclient.DeployPayload{
		DeploymentID: w.id,
		Routes:       []sdkclient.Route{{Hostname: "app.example.test", Upstream: w.id + "-app:8080"}},
		ReadySwap:    &sdkclient.ReadySwapPolicy{Path: "/healthz", StatusMin: 200, StatusMax: 399, TimeoutSeconds: 30, DrainSeconds: 5},
	}
}

func (w *swapWorld) run() (sdkclient.DeployResult, *sdkclient.ReadySwapReport, bool) {
	return w.d.readySwap(context.Background(), &sdkclient.PollCommand{ID: "cmd_swap"}, w.payload(), w.previous, "")
}

func (w *swapWorld) noViolations() {
	w.t.Helper()
	if len(w.violations) > 0 {
		w.t.Fatalf("the route stopped serving: %v (ops %v)", w.violations, w.ops)
	}
}

func (w *swapWorld) journalGone() {
	w.t.Helper()
	for _, name := range []string{readySwapJournalName, readySwapComposeName} {
		if _, err := os.Stat(filepath.Join(w.appDir, name)); !os.IsNotExist(err) {
			w.t.Fatalf("%s left behind", name)
		}
	}
}

func TestReadySwapSwitchesOnlyAfterTheNewVersionIsReady(t *testing.T) {
	w := newSwapWorld(t)
	r, rep, handled := w.run()
	if !handled || r.Status != "success" || rep.Outcome != "switched" || rep.Phase != swapDone {
		t.Fatalf("result %+v report %+v", r, rep)
	}
	w.noViolations()
	w.journalGone()
	if w.upstream != w.id+"-app" || w.ctrs[w.id+"-app"].version != "new" || w.ctrs[w.id+"-app-next"] != nil {
		t.Fatalf("end state: upstream %s, ctrs %v", w.upstream, w.ctrs)
	}
	want := []string{
		"compose up -d --no-build --pull never --no-deps app-next",
		"route -app-next",
		"compose up -d --no-build --pull never --no-deps app",
		"route -app",
		"compose rm -s -f app-next",
	}
	if !slices.Equal(w.ops, want) {
		t.Fatalf("ops %v", w.ops)
	}
	// The container that just lost the route keeps serving in-flight
	// requests for the drain time before it is replaced or removed.
	if w.opTimes[2].Sub(w.opTimes[1]) < 5*time.Second || w.opTimes[4].Sub(w.opTimes[3]) < 5*time.Second {
		t.Fatalf("no drain: %v", w.opTimes)
	}
}

func TestReadySwapKeepsThePreviousVersionWhenTheNewOneIsNotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(*swapWorld)
		reason string
	}{
		"wrong status":  {func(w *swapWorld) { w.answers["new"] = 500 }, "HTTP 500"},
		"crash":         {func(w *swapWorld) { w.crashes["new"] = true }, "stopped"},
		"route refused": {func(w *swapWorld) { w.failRoute[w.id+"-app-next"] = true }, "proxy did not accept"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSwapWorld(t)
			tc.setup(w)
			start := w.now
			r, rep, handled := w.run()
			if !handled || r.Status != "failed" || rep.Outcome != "kept_previous" || !strings.Contains(rep.Reason, tc.reason) {
				t.Fatalf("report %+v error %s", rep, r.Error)
			}
			w.noViolations()
			w.journalGone()
			if w.upstream != w.id+"-app" || w.ctrs[w.id+"-app"].version != "prev" || w.ctrs[w.id+"-app-next"] != nil {
				t.Fatalf("end state: upstream %s, ctrs %v", w.upstream, w.ctrs)
			}
			for _, op := range w.ops {
				if strings.HasSuffix(op, "--no-deps app") {
					t.Fatal("the running version was replaced")
				}
			}
			compose, _ := os.ReadFile(filepath.Join(w.appDir, "compose.yaml"))
			if string(compose) != string(w.previous.Compose) {
				t.Fatal("the previous configuration files were not put back")
			}
			if r.Rollback == nil || r.Rollback.Status != "restored" {
				t.Fatalf("rollback %+v", r.Rollback)
			}
			if name == "wrong status" && w.now.Sub(start) < 30*time.Second {
				t.Fatalf("gave up before the timeout (%v)", w.now.Sub(start))
			}
		})
	}
}

func TestReadySwapTimeoutNamesTheLastAnswer(t *testing.T) {
	w := newSwapWorld(t)
	w.answers["new"] = 503
	_, rep, _ := w.run()
	if rep.Outcome != "kept_previous" || !strings.Contains(rep.Reason, "not ready within 30 seconds") || !strings.Contains(rep.Reason, "HTTP 503 (accepted 200-399)") {
		t.Fatalf("reason %q", rep.Reason)
	}
}

func TestReadySwapRestoresThePreviousReleaseWhenTheNewAppFails(t *testing.T) {
	w := newSwapWorld(t)
	w.crashes["app:new"] = true // the new version runs as -app-next, not as -app
	r, rep, _ := w.run()
	if r.Status != "failed" || rep.Outcome != "recovered_previous" {
		t.Fatalf("report %+v error %s", rep, r.Error)
	}
	w.noViolations()
	w.journalGone()
	if w.upstream != w.id+"-app" || w.ctrs[w.id+"-app"].version != "prev" || w.ctrs[w.id+"-app-next"] != nil {
		t.Fatalf("end state: upstream %s, ctrs %v", w.upstream, w.ctrs)
	}
}

func TestReadySwapRefusesIneligibleAppsBeforeTouchingContainers(t *testing.T) {
	base := func(w *swapWorld) map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(w.resolved), &m)
		return m
	}
	app := func(m map[string]any) map[string]any {
		return m["services"].(map[string]any)["app"].(map[string]any)
	}
	for name, tc := range map[string]struct {
		mutate func(*swapWorld, map[string]any, *sdkclient.DeployPayload)
		reason string
	}{
		"host port": {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) {
			app(m)["ports"] = []any{map[string]any{"target": 8080, "published": "20001"}}
		}, "publishes a port"},
		"writable volume": {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) {
			app(m)["volumes"] = []any{map[string]any{"type": "volume", "source": "data", "target": "/data"}}
		}, "writable volume"},
		"two services": {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) {
			m["services"].(map[string]any)["db"] = map[string]any{"image": "db"}
		}, "single-service"},
		"no proxy net": {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) {
			app(m)["networks"] = map[string]any{"default": nil}
		}, "proxy network"},
		"other name":   {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) { app(m)["container_name"] = "other" }, "not named"},
		"network mode": {func(_ *swapWorld, m map[string]any, _ *sdkclient.DeployPayload) { app(m)["network_mode"] = "host" }, "network mode"},
		"no route":     {func(_ *swapWorld, _ map[string]any, p *sdkclient.DeployPayload) { p.Routes = nil }, "no route"},
		"tor egress":   {func(_ *swapWorld, _ map[string]any, p *sdkclient.DeployPayload) { p.Manifest.Runtime.TorEgress = true }, "Tor egress"},
		"bad policy":   {func(_ *swapWorld, _ map[string]any, p *sdkclient.DeployPayload) { p.ReadySwap.Path = "healthz" }, "readiness path"},
		"route elsewhere": {func(w *swapWorld, _ map[string]any, _ *sdkclient.DeployPayload) {
			w.upstream = w.id + "-app-next"
			w.ctrs[w.id+"-app-next"] = &fakeSwapCtr{version: "prev", status: "running", ip: "10.0.0.9"}
		}, "current route"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSwapWorld(t)
			m := base(w)
			p := w.payload()
			tc.mutate(w, m, &p)
			raw, _ := json.Marshal(m)
			w.resolved = string(raw)
			r, rep, handled := w.d.readySwap(context.Background(), &sdkclient.PollCommand{ID: "cmd_swap"}, p, w.previous, "")
			if !handled || r.Status != "failed" || rep.Outcome != "refused" || !strings.Contains(rep.Reason, tc.reason) {
				t.Fatalf("report %+v", rep)
			}
			if len(w.ops) != 0 {
				t.Fatalf("containers or routes touched: %v", w.ops)
			}
			if !strings.Contains(r.Error, "Nothing was changed") {
				t.Fatalf("error %q", r.Error)
			}
		})
	}
}

func TestReadySwapAcceptsReadOnlyStorage(t *testing.T) {
	w := newSwapWorld(t)
	var m map[string]any
	_ = json.Unmarshal([]byte(w.resolved), &m)
	m["services"].(map[string]any)["app"].(map[string]any)["volumes"] = []any{
		map[string]any{"type": "volume", "source": "assets", "target": "/srv", "read_only": true},
		map[string]any{"type": "tmpfs", "target": "/tmp"},
	}
	raw, _ := json.Marshal(m)
	w.resolved = string(raw)
	if _, rep, _ := w.run(); rep.Outcome != "switched" {
		t.Fatalf("a qualifying app was refused: %+v", rep)
	}
}

func TestReadySwapWithoutAHealthyRunningVersionReplacesNormally(t *testing.T) {
	w := newSwapWorld(t)
	_, rep, handled := w.d.readySwap(context.Background(), &sdkclient.PollCommand{ID: "cmd_swap"}, w.payload(), nil, "")
	if handled || rep == nil || rep.Outcome != "not_used" || len(w.ops) != 0 {
		t.Fatalf("handled=%v report %+v ops %v", handled, rep, w.ops)
	}
}

// The agent (or the worker) stops at every step of the swap; the recovery
// that runs next must leave exactly one version serving, and the route must
// serve at every instant, the recovery included.
func TestReadySwapInterruptedAtEveryStepSettlesOnOneVersion(t *testing.T) {
	full := newSwapWorld(t)
	full.run()
	steps := len(full.ops)
	// By design: until the new `app` is being created, the previous version
	// keeps (or gets back) the route; from then on the new one is finished.
	expect := map[string]string{
		"before 1": "recovered_previous", "after 1": "recovered_previous",
		"before 2": "recovered_previous", "after 2": "recovered_previous",
		"before 3": "recovered_new", "after 3": "recovered_new",
		"before 4": "recovered_new", "after 4": "recovered_new",
		"before 5": "recovered_new", "after 5": "recovered_new",
	}
	for i := 0; i < 2*steps; i++ {
		k, after := i%steps+1, i >= steps
		t.Run(fmt.Sprintf("crash %s %d (%s)", map[bool]string{false: "before", true: "after"}[after], k, full.ops[k-1]), func(t *testing.T) {
			w := newSwapWorld(t)
			w.crashAt, w.crashAfter = k, after
			func() {
				defer func() {
					if v := recover(); v != nil {
						if _, ok := v.(swapCrash); !ok {
							panic(v)
						}
					}
				}()
				w.run()
				t.Fatal("no crash")
			}()
			w.crashAt, w.crashAfter = 0, false
			// A fresh process: nothing in memory, only the disk and the host.
			d := &Docker{StateDir: w.d.StateDir, Log: w.d.Log}
			w.install(d)
			r := d.RecoverReadySwap(context.Background(), w.id)
			if _, err := os.Stat(filepath.Join(w.appDir, readySwapJournalName)); os.IsNotExist(err) && r == nil {
				// The crash hit after the swap cleaned up: nothing to settle.
				if w.ctrs[w.id+"-app"].version != "new" {
					t.Fatal("clean state without the new version")
				}
				return
			}
			if r == nil || r.ReadySwap == nil {
				t.Fatalf("no recovery result: %+v", r)
			}
			w.noViolations()
			w.journalGone()
			app, next := w.ctrs[w.id+"-app"], w.ctrs[w.id+"-app-next"]
			if w.upstream != w.id+"-app" || next != nil || app == nil || app.status != "running" {
				t.Fatalf("not settled on app alone: upstream %s ctrs %v", w.upstream, w.ctrs)
			}
			switch r.ReadySwap.Outcome {
			case "recovered_previous":
				if app.version != "prev" || r.Status != "failed" {
					t.Fatalf("previous outcome with %s/%s", app.version, r.Status)
				}
			case "recovered_new":
				if app.version != "new" || r.Status != "success" {
					t.Fatalf("new outcome with %s/%s", app.version, r.Status)
				}
			default:
				t.Fatalf("outcome %s", r.ReadySwap.Outcome)
			}
			if want := expect[fmt.Sprintf("%s %d", map[bool]string{false: "before", true: "after"}[after], k)]; r.ReadySwap.Outcome != want {
				t.Fatalf("outcome %s, the design settles this point on %s", r.ReadySwap.Outcome, want)
			}
			if r.CommandID != "cmd_swap" {
				t.Fatalf("command id %q", r.CommandID)
			}
		})
	}
}

func TestReadySwapRecoveryPrefersTheNewCopyWhenThePreviousIsGone(t *testing.T) {
	w := newSwapWorld(t)
	w.crashAt = 2 // after next started, while the route moves
	func() {
		defer func() { _ = recover() }()
		w.run()
	}()
	w.crashAt = 0
	w.ctrs[w.id+"-app"].status = "exited" // the previous version died meanwhile
	w.lenient = true                      // the route is already broken before recovery
	w.upstream = w.id + "-app-next"
	w.lenient = false
	d := &Docker{StateDir: w.d.StateDir, Log: w.d.Log}
	w.install(d)
	r := d.RecoverReadySwap(context.Background(), w.id)
	if r == nil || r.ReadySwap.Outcome != "recovered_new" || w.ctrs[w.id+"-app"].version != "new" || w.upstream != w.id+"-app" {
		t.Fatalf("result %+v ctrs %v", r, w.ctrs)
	}
	w.noViolations()
}

func TestReadySwapPolicyBounds(t *testing.T) {
	ok := sdkclient.ReadySwapPolicy{Path: "/", StatusMin: 200, StatusMax: 399, TimeoutSeconds: 60, DrainSeconds: 5}
	if err := validateReadySwapPolicy(&ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*sdkclient.ReadySwapPolicy){
		"relative path": func(p *sdkclient.ReadySwapPolicy) { p.Path = "healthz" },
		"traversal":     func(p *sdkclient.ReadySwapPolicy) { p.Path = "/../etc" },
		"crlf":          func(p *sdkclient.ReadySwapPolicy) { p.Path = "/a\r\nHost: x" },
		"status order":  func(p *sdkclient.ReadySwapPolicy) { p.StatusMin, p.StatusMax = 400, 200 },
		"status range":  func(p *sdkclient.ReadySwapPolicy) { p.StatusMax = 600 },
		"short timeout": func(p *sdkclient.ReadySwapPolicy) { p.TimeoutSeconds = 5 },
		"long drain":    func(p *sdkclient.ReadySwapPolicy) { p.DrainSeconds = 61 },
	} {
		p := ok
		mut(&p)
		if validateReadySwapPolicy(&p) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestReadySwapModelCopiesAppWithoutPorts(t *testing.T) {
	resolved := `{"services":{"app":{"image":"img:new","container_name":"dpl_x-app","ports":[{"target":8080}],"hostname":"h","environment":{"K":"v"}}}}`
	raw, err := readySwapModel([]byte(resolved), "dpl_x")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Services map[string]map[string]any `json:"services"`
	}
	_ = json.Unmarshal(raw, &m)
	next := m.Services["app-next"]
	if next["container_name"] != "dpl_x-app-next" || next["ports"] != nil || next["hostname"] != nil || next["image"] != "img:new" || m.Services["app"]["container_name"] != "dpl_x-app" {
		t.Fatalf("model %v", m.Services)
	}
}

// A supervised worker that died mid-swap: once systemd reliably reports it
// stopped, the agent settles the swap and seals the outcome as the worker's
// receipt, so the operation ends with a verified result.
func TestReadySwapDeadWorkerIsSettledAndSealed(t *testing.T) {
	for _, mode := range []string{"dead", "active", "other-work", "flapping"} {
		t.Run(mode, func(t *testing.T) {
			d, w, bin := replacementFixture(t)
			world := newSwapWorldOn(t, d, w.DeploymentID)
			world.crashAt = 2 // the worker dies while moving the route to next
			func() {
				defer func() { _ = recover() }()
				world.d.readySwap(context.Background(), &sdkclient.PollCommand{ID: w.CommandID}, world.payload(), world.previous, w.ID)
			}()
			world.crashAt = 0
			if mode == "other-work" {
				rec, _ := readSwapJournal(world.appDir)
				rec.WorkID = "rw_" + strings.Repeat("0", 32)
				_ = d.writeSwapJournal(world.appDir, *rec)
			}
			// flapping: the liveness read and the first confirmation say
			// stopped, the confirmation two seconds later says active.
			script := map[string]string{"dead": "echo inactive", "active": "echo active", "other-work": "echo inactive",
				"flapping": "n=$(cat '" + bin + "/n' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + bin + "/n'; if [ $n -le 2 ]; then echo inactive; else echo active; fi"}[mode]
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			opsBefore := len(world.ops)
			r, err := d.CompletedReplacementWork(w)
			if mode != "dead" {
				if err == nil || len(world.ops) != opsBefore {
					t.Fatalf("a live or foreign swap was touched: %v %v", err, world.ops[opsBefore:])
				}
				if _, statErr := os.Stat(filepath.Join(world.appDir, readySwapJournalName)); statErr != nil {
					t.Fatal("journal removed")
				}
				return
			}
			if err != nil || r == nil || r.ReadySwap == nil || r.ReadySwap.Outcome != "recovered_previous" || r.CommandID != w.CommandID {
				t.Fatalf("result %+v err %v", r, err)
			}
			world.noViolations()
			world.journalGone()
			// Sealed: the next read is the receipt, not a second recovery.
			ops := len(world.ops)
			again, err := d.CompletedReplacementWork(w)
			if err != nil || again.ReadySwap == nil || again.ReadySwap.Outcome != "recovered_previous" || len(world.ops) != ops {
				t.Fatalf("receipt not sealed: %+v %v", again, err)
			}
		})
	}
}

// The daemon runs the replacement in a supervised worker with a reduced
// payload. The policy has to cross that boundary, and so does Sandbox, or the
// worker's eligibility check would let a sandboxed app swap (and on the real
// daemon every swap would run as a normal redeploy).
func TestReadySwapPolicyCrossesTheWorkerBoundary(t *testing.T) {
	policy := &sdkclient.ReadySwapPolicy{Path: "/healthz", StatusMin: 200, StatusMax: 399, TimeoutSeconds: 30, DrainSeconds: 5}
	sandbox := &sdkclient.SandboxSpec{}
	in := sdkclient.DeployPayload{DeploymentID: "dpl_readyswap00c0de", ReadySwap: policy}
	in.Manifest.Runtime.Sandbox = sandbox
	out := replacementPayload(in)
	if out.ReadySwap == nil || *out.ReadySwap != *policy {
		t.Fatalf("ready_swap lost at the daemon -> worker boundary: %+v", out.ReadySwap)
	}
	if out.Manifest.Runtime.Sandbox != sandbox {
		t.Fatal("sandbox lost at the daemon -> worker boundary: the worker would not refuse a sandboxed app")
	}
	// And through the worker's own request file, which is JSON.
	raw, err := json.Marshal(replacementRequest{Payload: out})
	if err != nil {
		t.Fatal(err)
	}
	var back replacementRequest
	if err := json.Unmarshal(raw, &back); err != nil || back.Payload.ReadySwap == nil || *back.Payload.ReadySwap != *policy || back.Payload.Manifest.Runtime.Sandbox == nil {
		t.Fatalf("policy or sandbox lost through the worker request: %+v %v", back.Payload.ReadySwap, err)
	}
	if err := readySwapEligibility(out, []byte(`{"services":{"app":{}}}`)); err == nil || !strings.Contains(err.Error(), "sandboxed") {
		t.Fatalf("the worker accepts a sandboxed app: %v", err)
	}
}

// The passes must be consecutive. An inspection error between good answers
// breaks the sequence (two passes, an error, then three more: 6 inspections
// and 5 HTTP answers, not 4 and 3).
func TestReadySwapInspectionErrorBreaksTheSequence(t *testing.T) {
	now := time.Unix(0, 0)
	inspections, probes := 0, 0
	d := &Docker{
		readySwapClock: func() time.Time { return now },
		readySwapSleep: func(_ context.Context, wait time.Duration) bool { now = now.Add(wait); return true },
		readySwapInspect: func(context.Context, string) (swapContainer, error) {
			inspections++
			if inspections == 3 {
				return swapContainer{}, errors.New("injected inspect error")
			}
			return swapContainer{Exists: true, Status: "running", IP: "192.0.2.2"}, nil
		},
		readySwapProbe: func(context.Context, string, string) (int, error) { probes++; return 200, nil },
	}
	if err := d.awaitSwapReady(context.Background(), "dpl_x-app-next", "8080", "app.swap.invalid", sdkclient.ReadySwapPolicy{Path: "/", StatusMin: 200, StatusMax: 399, TimeoutSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	if inspections != 6 || probes != 5 {
		t.Fatalf("accepted after %d inspections and %d HTTP passes; an inspection error must reset the sequence", inspections, probes)
	}
}

// An agent stopped between writing the fragment and the reload leaves the
// fragment on `app` and the running proxy on `-app-next`. The recovery of
// that phase must reload and confirm the running proxy before it removes
// `-app-next`, or the app answers 502 until some later reload.
func TestReadySwapRecoveryRepublishesWhenTheProxyLagsTheFragment(t *testing.T) {
	split := func(t *testing.T, stuck bool) (*swapWorld, *sdkclient.DeployResult) {
		w := newSwapWorld(t)
		w.ctrs[w.id+"-app"] = &fakeSwapCtr{version: "new", status: "running", ip: "192.0.2.2"}
		w.ctrs[w.id+"-app-next"] = &fakeSwapCtr{version: "new", status: "running", ip: "192.0.2.3"}
		w.upstream, w.liveUpstream, w.liveStuck = w.id+"-app", w.id+"-app-next", stuck
		rec := readySwapRecord{Version: 1, DeploymentID: w.id, CommandID: "cmd_split", Phase: swapRoutedApp, ReleaseID: w.previous.Metadata.ID, Port: "8080", Host: "app.example.test", Policy: sdkclient.ReadySwapPolicy{Path: "/healthz", StatusMin: 200, StatusMax: 399, TimeoutSeconds: 30, DrainSeconds: 5}}
		if err := w.d.writeSwapJournal(w.appDir, rec); err != nil {
			t.Fatal(err)
		}
		return w, w.d.RecoverReadySwap(context.Background(), w.id)
	}
	served := func(w *swapWorld) *fakeSwapCtr {
		live, _ := w.LiveUpstream(context.Background(), w.id)
		return w.ctrs[live]
	}
	t.Run("reload takes", func(t *testing.T) {
		w, r := split(t, false)
		if r == nil || r.Status != "success" || r.ReadySwap == nil || r.ReadySwap.Outcome != "recovered_new" {
			t.Fatalf("result %+v", r)
		}
		if live, _ := w.LiveUpstream(context.Background(), w.id); live != w.id+"-app" {
			t.Fatalf("the running proxy stayed on %s", live)
		}
		if c := served(w); c == nil || c.status != "running" || c.version != "new" {
			t.Fatalf("the running proxy serves %+v; violations %v", c, w.violations)
		}
		if w.ctrs[w.id+"-app-next"] != nil && w.ctrs[w.id+"-app-next"].status == "running" {
			t.Fatal("-app-next left running")
		}
	})
	t.Run("reload does not take", func(t *testing.T) {
		w, r := split(t, true)
		if r != nil && r.Status == "success" {
			t.Fatalf("success while the running proxy is still on -app-next: %+v", r.ReadySwap)
		}
		if c := served(w); c == nil || c.status != "running" {
			t.Fatalf("the container the running proxy uses was removed or stopped: %+v; violations %v", c, w.violations)
		}
	})
}

// A swap only starts from a known state: the running proxy on `app`.
func TestReadySwapRefusesWhenTheRunningProxyIsElsewhere(t *testing.T) {
	w := newSwapWorld(t)
	w.liveUpstream = w.id + "-app-next"
	r, rep, handled := w.d.readySwap(context.Background(), &sdkclient.PollCommand{ID: "cmd_swap"}, w.payload(), w.previous, "")
	if !handled || r.Status != "failed" || rep == nil || rep.Outcome != "refused" || !strings.Contains(rep.Reason, "running proxy") {
		t.Fatalf("result %+v report %+v", r, rep)
	}
	if len(w.ops) != 0 {
		t.Fatalf("containers or routes touched: %v", w.ops)
	}
}
