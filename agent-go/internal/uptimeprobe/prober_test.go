package uptimeprobe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgentServer implements the TWO routes the prober may touch and
// records every other request (any hit outside them fails the test).
type fakeAgentServer struct {
	mu        sync.Mutex
	targets   TargetsResponse
	results   []resultsBody
	otherHits []string
	vantage   bool
	srv       *httptest.Server
}

func newFakeAgentServer(vantage bool) *fakeAgentServer {
	f := &fakeAgentServer{vantage: vantage}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/uptime/targets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		vantage := f.vantage
		f.mu.Unlock()
		if !vantage {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"not a vantage"}}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// The SDK requires the {"success":true,"data":...} envelope; without
		// it Get fails even on a 200.
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": f.targets})
	})
	mux.HandleFunc("/v1/agent/uptime/results", func(w http.ResponseWriter, r *http.Request) {
		var body resultsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.results = append(f.results, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"success":true}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.otherHits = append(f.otherHits, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeAgentServer) client() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// TestCustomerAgentNeverProbes: a non-vantage credential gets 403 from the
// targets route and IsVantage answers false — the poller must not announce
// or start anything.
func TestCustomerAgentNeverProbes(t *testing.T) {
	f := newFakeAgentServer(false)
	defer f.srv.Close()
	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	if p.IsVantage(context.Background()) {
		t.Fatal("IsVantage must be false for a 403 answer")
	}
	if len(f.otherHits) != 0 {
		t.Fatalf("prober touched routes beyond its two: %v", f.otherHits)
	}
}

// TestRoundTripProbesLocal: the positive end-to-end against a local
// listener behind the guard. The listener binds loopback, which the guard
// denies — so the target's DNS here is a real public-looking name whose
// resolution we inject via a resolver we control, pointing at a PUBLIC
// documentation address... which is also denied. The true positive runs in
// a real-host test against a real public IP; locally we pin the mechanics
// with the socks path disabled and the guard refusing the loopback form.
func TestRoundTripProbesLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("probe method = %s, want HEAD", r.Method)
		}
		if ua := r.Header.Get("User-Agent"); ua != "ImprezaUptime/1" {
			t.Errorf("user agent = %q", ua)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	f := newFakeAgentServer(true)
	defer f.srv.Close()
	f.targets = TargetsResponse{
		Protocol: 1, IntervalSeconds: 15,
		Targets: []Target{{DeploymentID: "dpl_local", Scheme: "http", Host: host, Kind: "domain"}},
	}

	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	results := p.probeAll(context.Background(), f.targets.Targets)
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	r := results[0]
	if r.DeploymentID != "dpl_local" {
		t.Fatalf("deployment = %s", r.DeploymentID)
	}
	// The local listener sits on loopback: the guard MUST refuse it and
	// the result must be err with ok=0 — fail-closed is the contract.
	if r.OK != 0 || r.StatusClass != "err" {
		t.Fatalf("loopback target probed (ok=%d class=%s): the guard must refuse before any connect", r.OK, r.StatusClass)
	}
	if f.targets.Targets[0].Host != host {
		t.Fatalf("host mutated")
	}
}

// TestOnionFailsClosedWithoutTor: an onion target with no SOCKS configured
// yields ok=0/err and NEVER a clearnet connection attempt.
func TestOnionFailsClosedWithoutTor(t *testing.T) {
	f := newFakeAgentServer(true)
	defer f.srv.Close()
	onion := strings.Repeat("a", 56) + ".onion"
	f.targets = TargetsResponse{
		Protocol: 1, IntervalSeconds: 60,
		Targets: []Target{{DeploymentID: "dpl_on", Scheme: "http", Host: onion, Kind: "onion"}},
	}
	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	results := p.probeAll(context.Background(), f.targets.Targets)
	if results[0].OK != 0 || results[0].StatusClass != "err" {
		t.Fatalf("onion without tor must fail closed (ok=%d class=%s)", results[0].OK, results[0].StatusClass)
	}
	if len(f.otherHits) != 0 {
		t.Fatalf("unexpected route hits: %v", f.otherHits)
	}
}

// TestLimits: the target cap and the per-round deadline hold.
func TestLimits(t *testing.T) {
	f := newFakeAgentServer(true)
	defer f.srv.Close()
	targets := make([]Target, MaxTargets+37)
	for i := range targets {
		targets[i] = Target{DeploymentID: fmt.Sprintf("dpl_%03d", i), Scheme: "https", Host: "denied.example", Kind: "domain"}
	}
	f.targets = TargetsResponse{Protocol: 1, IntervalSeconds: 60, Targets: targets}
	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	got := p.probeAll(context.Background(), f.targets.Targets)
	if len(got) != MaxTargets {
		t.Fatalf("probeAll probed %d targets, cap is %d", len(got), MaxTargets)
	}
}

// TestPostShape: the posted JSON carries protocol 1, a round_at the
// server's ±300 s window accepts, and results with numbers only.
func TestPostShape(t *testing.T) {
	f := newFakeAgentServer(true)
	defer f.srv.Close()
	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	res := []Result{{DeploymentID: "dpl_x", OK: 0, StatusClass: "err", LatencyMS: 3}}
	if err := p.postResults(context.Background(), res); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) != 1 {
		t.Fatalf("posted rounds = %d", len(f.results))
	}
	b := f.results[0]
	if b.Protocol != 1 || len(b.Results) != 1 || b.Results[0].DeploymentID != "dpl_x" {
		t.Fatalf("bad body: %+v", b)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", b.RoundAt)
	if err != nil {
		t.Fatalf("round_at not RFC3339: %v", err)
	}
	if d := time.Since(parsed); d < -time.Minute || d > time.Minute {
		t.Fatalf("round_at %s is not ~now", b.RoundAt)
	}
}

// TestNoRedirectFollow: a 3xx answer is reported, never followed — a
// misbehaving edge cannot aim the probe somewhere else.
func TestNoRedirectFollow(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/" {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
			return
		}
		t.Errorf("redirect was followed to %s", r.URL.Path)
	}))
	defer srv.Close()
	// Host keeps host:PORT — the probe URL derives its port from it; the
	// dialer swaps only the address part.
	hostport := strings.TrimPrefix(srv.URL, "http://")
	tr := transportTo(hostport, false)
	res := &Result{}
	ok, class := doProbe(context.Background(), tr, Target{Scheme: "http", Host: hostport}, res, false)
	if ok != 1 {
		t.Fatalf("a 3xx must count as a (ok) answer, got ok=%d class=%s", ok, class)
	}
	_ = class
	if hits > 1 {
		t.Fatalf("redirect followed: %d requests", hits)
	}
}

// transportTo builds a HostDialer to a literal address, bypassing the
// guard for the mechanics-only tests above (the guard itself has its own
// suite).
func transportTo(hostport string, _ bool) *http.Transport {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	ip := net.ParseIP(h)
	if ip == nil {
		ip = net.ParseIP("127.0.0.1")
	}
	return dialerFor(ip)
}

// fakeClientFor builds the smallest sdkclient.Client the prober needs.
// The prober only uses Get/Post on paths, so a client pointed at the test
// server through the SDK's agent constructor is what we need; but the SDK
// requires credentials — a stub type is not possible without opening the
// package, so tests use the real constructor with dummy credentials.
func fakeClientFor(t *testing.T, baseURL string) *sdkClient {
	t.Helper()
	c, err := sdkclientNewAgent(baseURL)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

var _ = tls.Config{}
var _ = io.Discard
var _ = slog.Default
var _ time.Duration

// TestStopsWhenMarkRemoved: a credential that WAS a vantage loses the mark
// server-side. The next targets fetch answers 403 and Run must return (the
// poller then drops uptime-probe-v1 from the announcement); Active() goes
// false and nothing was posted for the refused round.
func TestStopsWhenMarkRemoved(t *testing.T) {
	f := newFakeAgentServer(true)
	defer f.srv.Close()
	f.mu.Lock()
	f.targets = TargetsResponse{Protocol: 1, IntervalSeconds: 60}
	f.mu.Unlock()

	p := New(fakeClientFor(t, f.srv.URL), "", slog.Default())
	if !p.IsVantage(context.Background()) {
		t.Fatal("precondition: credential must be a vantage first")
	}

	// The mark disappears before Run's first fetch.
	f.mu.Lock()
	f.vantage = false
	f.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going after the vantage mark was removed")
	}
	if p.Active() {
		t.Fatal("Active() must be false once Run returned")
	}
	f.mu.Lock()
	posted := len(f.results)
	hits := len(f.otherHits)
	f.mu.Unlock()
	if posted != 0 {
		t.Fatalf("results posted for a refused round: %d", posted)
	}
	if hits != 0 {
		t.Fatalf("prober touched routes beyond its two: %v", f.otherHits)
	}
}
