package uptimeprobe

// Prober guard proofs: each guard, with the mutation that removes it
// failing by construction.
//
//   1. refusal logs carry the range CLASS and the deployment id — never
//      the hostname nor the resolved address (TestRefusalLogSanitized);
//   2. ONE 15 s context covers resolve + dial + answer together
//      (TestDeadlineCoversResolveAndAnswer);
//   3. the MaxTargets window rotates per round — 150 synthetic targets,
//      two rounds, every host measured at least once
//      (TestRotatingWindowCoversAllTargets); the fixed-window code this
//      replaces never resolves hosts 100..149;
//   4. kind=onion reaches the local SOCKS only for a valid v3 .onion
//      hostname (TestOnionV3BeforeSocks) — a clearnet or malformed name
//      never opens the SOCKS, and a valid v3 onion is NOT refused.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingResolver answers every host with one public IP and records the
// lookups; it can also stall, for the deadline proof.
type recordingResolver struct {
	mu     sync.Mutex
	seen   map[string]int
	delay  time.Duration
	answer []net.IPAddr
}

func newRecordingResolver(delay time.Duration) *recordingResolver {
	return &recordingResolver{seen: map[string]int{}, delay: delay,
		answer: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
}

func (r *recordingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	r.seen[host]++
	r.mu.Unlock()
	return r.answer, nil
}

func (r *recordingResolver) lookedUp() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.seen))
	for k, v := range r.seen {
		out[k] = v
	}
	return out
}

// failFastTransport never dials: the probe window machinery is under test,
// not the network.
func failFastTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, fmt.Errorf("fixture: no dial")
		},
		DisableKeepAlives: true,
	}
}

// ── 1 · the refusal log names the class, never the target ──────────────────

func TestRefusalLogSanitized(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	p := New(nil, "", log)
	p.resolver = staticResolver{[]net.IPAddr{{IP: net.ParseIP("100.64.0.1")}}}

	res := p.probeOne(context.Background(), Target{DeploymentID: "dpl_priv", Host: "sensitive-host.example", Kind: "domain"})
	out := buf.String()
	if res.OK != 0 || res.StatusClass != "err" {
		t.Fatalf("refused target must be an err result: %+v", res)
	}
	if !strings.Contains(out, "dpl_priv") || !strings.Contains(out, "cgnat") {
		t.Fatalf("the log names the deployment and the range class:\n%s", out)
	}
	for _, forbidden := range []string{"sensitive-host.example", "100.64.0.1"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("MUTATION CATCH: the log leaks %q:\n%s", forbidden, out)
		}
	}
}

// staticResolver answers every host with a fixed list (no recording).
type staticResolver struct{ addrs []net.IPAddr }

func (s staticResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return s.addrs, nil
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── 2 · one 15 s budget covers resolve + answer ────────────────────────────

func TestDeadlineCoversResolveAndAnswer(t *testing.T) {
	// A resolver that stalls 3 s, then an HTTP server that never answers:
	// under the old code (resolver outside the budget, client.Timeout on
	// the request alone) the total ran past 15 s; the single per-target
	// context must cut the whole probe at the deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(&discardWriter{}, nil))
	p := New(nil, "", log)
	p.resolver = newRecordingResolver(3 * time.Second)
	orig := newTransport
	d := &net.Dialer{}
	newTransport = func(ip net.IP, timeout time.Duration) *http.Transport {
		return &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, srv.Listener.Addr().String())
			},
			DisableKeepAlives: true,
		}
	}
	defer func() { newTransport = orig }()

	started := time.Now()
	res := p.probeOne(context.Background(), Target{DeploymentID: "dpl_slow", Host: "slow.example", Scheme: "http", Kind: "domain"})
	elapsed := time.Since(started)

	if res.OK != 0 || res.StatusClass != "err" {
		t.Fatalf("the hanging target must fail: %+v", res)
	}
	// Declared measurement margin: 0.5 s for goroutine scheduling and the
	// resolver's own sleep tick.
	if elapsed > PerTargetTimeout+500*time.Millisecond {
		t.Fatalf("MUTATION CATCH: DNS delay + HEAD exceeded the single budget: %s", elapsed)
	}
	if elapsed < PerTargetTimeout-2*time.Second {
		t.Fatalf("the probe must run INTO the deadline (DNS 3 s + hang), cut at %s", elapsed)
	}
}

// ── 3 · the window rotates: 150 targets, two rounds, all measured ──────────

func TestRotatingWindowCoversAllTargets(t *testing.T) {
	p := New(nil, "", slog.New(slog.NewTextHandler(&discardWriter{}, nil)))
	rec := newRecordingResolver(0)
	p.resolver = rec
	orig := newTransport
	newTransport = func(ip net.IP, timeout time.Duration) *http.Transport { return failFastTransport() }
	defer func() { newTransport = orig }()

	const total = 150
	targets := make([]Target, total)
	for i := range targets {
		targets[i] = Target{DeploymentID: fmt.Sprintf("dpl_%03d", i), Host: fmt.Sprintf("host%03d.example", i), Kind: "domain"}
	}

	r1 := p.probeAll(context.Background(), targets)
	if len(r1) != MaxTargets {
		t.Fatalf("first round window = %d, cap is %d", len(r1), MaxTargets)
	}
	seen := rec.lookedUp()
	for i := 0; i < MaxTargets; i++ {
		if seen[fmt.Sprintf("host%03d.example", i)] == 0 {
			t.Fatalf("first round missed host %d inside its window", i)
		}
	}

	r2 := p.probeAll(context.Background(), targets)
	if len(r2) != MaxTargets {
		t.Fatalf("second round window = %d, cap is %d", len(r2), MaxTargets)
	}
	seen = rec.lookedUp()
	missing := 0
	for i := 0; i < total; i++ {
		if seen[fmt.Sprintf("host%03d.example", i)] == 0 {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("MUTATION CATCH: %d of %d targets were never measured across two rounds (the fixed window never leaves the head of the list)", missing, total)
	}
}

// ── 4 · kind=onion opens the SOCKS only for a valid v3 .onion ───────────────

func TestOnionV3BeforeSocks(t *testing.T) {
	// A fake SOCKS endpoint that counts connections and never speaks.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns int64
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&conns, 1)
			c.Close()
		}
	}()
	defer listener.Close()

	p := New(nil, listener.Addr().String(), slog.New(slog.NewTextHandler(&discardWriter{}, nil)))

	// A clearnet hostname with kind=onion: refused before the SOCKS.
	res := p.probeOne(context.Background(), Target{DeploymentID: "dpl_cl", Host: "example.com", Kind: "onion"})
	if res.OK != 0 || res.StatusClass != "err" {
		t.Fatalf("clearnet kind=onion must be refused: %+v", res)
	}
	if n := atomic.LoadInt64(&conns); n != 0 {
		t.Fatalf("MUTATION CATCH: the clearnet hostname reached the SOCKS endpoint (%d conns)", n)
	}

	// Malformed onions: also refused before the SOCKS.
	for _, host := range []string{"short.onion", strings.Repeat("a", 56) + ".onion.x", strings.Repeat("A", 56) + ".onion", strings.Repeat("1", 57) + ".onion"} {
		p.probeOne(context.Background(), Target{DeploymentID: "dpl_bad", Host: host, Kind: "onion"})
		if n := atomic.LoadInt64(&conns); n != 0 {
			t.Fatalf("MUTATION CATCH: malformed onion %q reached the SOCKS endpoint", host)
		}
	}

	// A VALID v3 onion is not over-blocked: it reaches the SOCKS (and fails
	// there — the fake never speaks the handshake), which is the fail-closed
	// answer, not a refusal.
	valid := strings.Repeat("a", 56) + ".onion"
	res = p.probeOne(context.Background(), Target{DeploymentID: "dpl_ok", Host: valid, Kind: "onion"})
	if n := atomic.LoadInt64(&conns); n == 0 {
		t.Fatalf("a valid v3 onion must reach the local SOCKS (over-blocking)")
	}
	if res.OK != 0 {
		t.Fatalf("the fake SOCKS cannot answer — the result must be err: %+v", res)
	}
}

// helpers

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
