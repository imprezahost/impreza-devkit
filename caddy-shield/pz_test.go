package caddyshield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// TestPageJavaScriptSHA256 extracts the inline solver from the rendered
// challenge page and checks, via Node when available, that the pure-JS
// SHA-256 matches Go's implementation on representative inputs — including
// the exact challenge:nonce concatenation the server verifies.
func TestPageJavaScriptSHA256(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available for JS cross-check")
	}
	page := renderChallengePage("00112233445566778899aabbccddeeff", 2, "/x")
	start := strings.Index(page, "// Pure-JS SHA-256")
	end := strings.Index(page, "var target=")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("solver block not found in page")
	}
	solver := page[start:end]

	inputs := []string{
		"",
		"abc",
		"00112233445566778899aabbccddeeff:0",
		"00112233445566778899aabbccddeeff:65537",
		strings.Repeat("a1b2c3:", 40) + "123456",
	}
	var script strings.Builder
	script.WriteString(solver)
	script.WriteString("\nvar inputs=[")
	for i, in := range inputs {
		if i > 0 {
			script.WriteString(",")
		}
		script.WriteString(jsonString(in))
	}
	script.WriteString("];for(var i=0;i<inputs.length;i++){console.log(sha256hex(inputs[i]));}")

	dir := t.TempDir()
	file := filepath.Join(dir, "solver.js")
	if err := os.WriteFile(file, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).Output()
	if err != nil {
		t.Fatalf("node run: %v", err)
	}
	got := strings.Fields(string(out))
	if len(got) != len(inputs) {
		t.Fatalf("expected %d hashes, got %d", len(inputs), len(got))
	}
	for i, in := range inputs {
		sum := sha256.Sum256([]byte(in))
		want := hex.EncodeToString(sum[:])
		if got[i] != want {
			t.Fatalf("input %d (%q): js=%s go=%s", i, in, got[i], want)
		}
	}
}

func jsonString(s string) string {
	// JSON string literals are valid JS literals.
	b, _ := json.Marshal(s)
	return string(b)
}

// TestPowProvisionValidation covers the negative configuration paths: a
// misconfigured gate must fail loudly instead of silently passing traffic.
func TestPowProvisionValidation(t *testing.T) {
	cases := []func(*PowMiddleware){
		func(m *PowMiddleware) { m.Deployment = "" },
		func(m *PowMiddleware) { m.Difficulty = 0 },
		func(m *PowMiddleware) { m.Difficulty = 9 },
		func(m *PowMiddleware) { m.MaxDifficulty = 2 }, // below difficulty
		func(m *PowMiddleware) { m.CookieTTLSeconds = 90000 },
		func(m *PowMiddleware) { m.ChallengeTTLSeconds = 700 },
		func(m *PowMiddleware) { m.AdaptiveThreshold = -1 },
	}
	for i, mutate := range cases {
		m := &PowMiddleware{Deployment: "dpl_x", Difficulty: 3, MaxDifficulty: 6}
		mutate(m)
		if err := m.Provision(caddy.Context{}); err == nil {
			t.Fatalf("case %d: invalid config accepted", i)
		}
	}
}

// TestRateLimitProvisionValidation mirrors the negative paths for the
// limiter.
func TestRateLimitProvisionValidation(t *testing.T) {
	cases := []func(*RateLimitMiddleware){
		func(m *RateLimitMiddleware) { m.Deployment = "" },
		func(m *RateLimitMiddleware) { m.Limit = 5 },
		func(m *RateLimitMiddleware) { m.Limit = 200000 },
		func(m *RateLimitMiddleware) { m.WindowSeconds = 7200 },
		func(m *RateLimitMiddleware) { m.BanSeconds = 90000 },
		func(m *RateLimitMiddleware) { m.StrikeFactor = 11 },
	}
	for i, mutate := range cases {
		m := &RateLimitMiddleware{Deployment: "dpl_x", Limit: 100, WindowSeconds: 60}
		mutate(m)
		if err := m.Provision(caddy.Context{}); err == nil {
			t.Fatalf("case %d: invalid config accepted", i)
		}
	}
}

// TestCappedStoreEviction exercises the bounded-store contract.
func TestCappedStoreEviction(t *testing.T) {
	now := int64(1000)
	s := newCappedStore(3, func() int64 { return now })
	if !s.add("a", 1100) || !s.add("b", 1100) || !s.add("c", 1100) {
		t.Fatal("adds within cap failed")
	}
	if s.add("d", 1100) {
		t.Fatal("add beyond cap accepted")
	}
	now = 1099
	if !s.has("a") {
		t.Fatal("live entry missing")
	}
	now = 1200
	if s.has("a") || s.has("b") {
		t.Fatal("expired entries still present")
	}
	if !s.add("d", 1300) {
		t.Fatal("add after sweep failed")
	}
	if !s.take("d") {
		t.Fatal("take of live entry failed")
	}
	if s.take("d") {
		t.Fatal("double take succeeded — single-use violated")
	}
}

// TestAdaptiveDifficultySteps drives the rolling counter: sustained load
// above the threshold raises difficulty to the cap; quiet time lowers it.
func TestAdaptiveDifficultySteps(t *testing.T) {
	m := &PowMiddleware{Deployment: "dpl_a", Difficulty: 2, MaxDifficulty: 4, AdaptiveThreshold: 5, CookieTTLSeconds: 60, ChallengeTTLSeconds: 60}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	if m.currentDifficulty() != 2 {
		t.Fatal("base difficulty wrong")
	}
	// Overloaded consecutive windows push difficulty up: two full hot
	// windows must COMPLETE (roll over) before the step applies, so the
	// loop fills and closes three windows.
	for i := 0; i < 3; i++ {
		for j := 0; j < 10; j++ {
			m.countAdaptive()
		}
		m.mu.Lock()
		m.windowMin -= 60 // advance the window boundary
		m.mu.Unlock()
	}
	if d := m.currentDifficulty(); d != 3 {
		t.Fatalf("difficulty after load = %d, want 3", d)
	}
	// Flush the still-pending hot window so its rollover cannot refresh
	// lastHot after the time shift below.
	m.countAdaptive()
	m.mu.Lock()
	m.windowMin -= 60
	m.mu.Unlock()
	// Sustained quiet windows walk difficulty back down; the step-down
	// additionally requires >5min since the last hot window, so emulate
	// elapsed time before the quiet phase.
	m.mu.Lock()
	m.lastHot = m.lastHot.Add(-10 * time.Minute)
	m.mu.Unlock()
	for i := 0; i < 4; i++ {
		m.countAdaptive()
		m.mu.Lock()
		m.windowMin -= 60
		m.mu.Unlock()
	}
	if d := m.currentDifficulty(); d != 2 {
		t.Fatalf("difficulty after quiet = %d, want 2", d)
	}
}
