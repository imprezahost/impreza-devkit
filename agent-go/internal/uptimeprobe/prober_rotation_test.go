package uptimeprobe

// The real path test for the coverage defect: the control plane's target
// list has no cap (the server returns every enabled target), so the
// prober's rotating window is what selects MaxTargets per round. When
// fetchTargets cut the list to the first MaxTargets before the window,
// with 150 targets the last 50 were never measured — in any round.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTargetsBeyondTheHundredthAreMeasured(t *testing.T) {
	const total = MaxTargets + 50
	f := newFakeAgentServer(true)
	defer f.srv.Close()
	targets := make([]Target, total)
	for i := range targets {
		targets[i] = Target{DeploymentID: fmt.Sprintf("dpl_%03d", i), Scheme: "https", Host: fmt.Sprintf("host%03d.example", i), Kind: "domain"}
	}
	f.targets = TargetsResponse{Protocol: 1, IntervalSeconds: 60, Targets: targets}

	p := New(fakeClientFor(t, f.srv.URL), "", slog.New(slog.NewTextHandler(&discardWriter{}, nil)))
	rec := newRecordingResolver(0)
	p.resolver = rec
	orig := newTransport
	newTransport = func(ip net.IP, timeout time.Duration) *http.Transport { return failFastTransport() }
	defer func() { newTransport = orig }()

	attempted := map[string]int{}
	for round := 1; round <= 3; round++ {
		// The REAL path: GET targets through the SDK transport, then the
		// window over exactly what that call returned.
		fetched, err := p.fetchTargets(context.Background())
		if err != nil {
			t.Fatalf("round %d: fetchTargets: %v", round, err)
		}
		if len(fetched.Targets) != total {
			t.Fatalf("round %d: fetchTargets returned %d targets, the server serves %d — a cut before the window loses the tail forever", round, len(fetched.Targets), total)
		}
		results := p.probeAll(context.Background(), fetched.Targets)
		if len(results) > MaxTargets {
			t.Fatalf("round %d: %d results, over the per-round cap", round, len(results))
		}
		for host := range rec.lookedUp() {
			attempted[host]++
		}
	}
	missing := 0
	for i := 0; i < total; i++ {
		if attempted[fmt.Sprintf("host%03d.example", i)] == 0 {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("MUTATION CATCH (the cut before the window): %d of %d targets were never measured in three rounds", missing, total)
	}
}
