package executor

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fakePullOutput satisfies the pre-review Len() interface AND the
// grow-only Total() one, so this file also compiles against the base
// commit as the negative control: there the watchdog reads Len().
type fakePullOutput struct{ n int64 }

func (f *fakePullOutput) Len() int     { return int(f.n) }
func (f *fakePullOutput) Total() int64 { return f.n }

// newTestPullBudget mirrors newPullBudget without the goroutine so the
// deadline math runs deterministically against synthetic clocks.
func newTestPullBudget(start time.Time, sample rxSampler, output progressReader) *pullBudget {
	return &pullBudget{
		start:   start,
		base:    composePullTimeout,
		stall:   pullStallWindow,
		ceiling: composePullCeiling,
		cancel:  func() {},
		done:    make(chan struct{}),
		sample:  sample,
		output:  output,
	}
}

func TestPullBudgetBaseDeadlineWithoutProgress(t *testing.T) {
	start := time.Now()
	b := newTestPullBudget(start, func() (int64, bool) { return 0, true }, &fakePullOutput{})
	if b.expired(start.Add(composePullTimeout - time.Second)) {
		t.Fatal("fired before the base budget")
	}
	if !b.expired(start.Add(composePullTimeout)) {
		t.Fatal("base budget did not fire")
	}
	if reason := b.stop(); !strings.Contains(reason, "stopped making progress") {
		t.Fatalf("missing stall reason: %q", reason)
	}
}

func TestPullBudgetExtendsWhileBytesArrive(t *testing.T) {
	start := time.Now()
	var rx int64 = 0
	b := newTestPullBudget(start, func() (int64, bool) { return rx, true }, &fakePullOutput{})
	// Bytes keep arriving every tick: the deadline keeps moving with the
	// stall window measured from the last progress.
	for tick := 0; tick < 40; tick++ {
		now := start.Add(time.Duration(tick+1) * pullTick)
		rx += pullMinRxProgress
		if b.expired(now) {
			t.Fatalf("fired at %s while bytes were still arriving", now.Sub(start))
		}
	}
	// Silence after the last progress: fires one stall window later.
	last := start.Add(41 * pullTick)
	if b.expired(last) {
		t.Fatal("fired before the stall window elapsed")
	}
	if !b.expired(last.Add(pullStallWindow)) {
		t.Fatal("stall window did not fire after progress stopped")
	}
	if reason := b.stop(); !strings.Contains(reason, "stopped making progress") {
		t.Fatalf("missing stall reason: %q", reason)
	}
}

func TestPullBudgetOutputCountsAsProgressWithoutRx(t *testing.T) {
	start := time.Now()
	out := &fakePullOutput{}
	// RX unavailable (non-Linux or unreadable /proc): command output alone
	// must keep the pull alive.
	b := newTestPullBudget(start, func() (int64, bool) { return 0, false }, out)
	for tick := 0; tick < 30; tick++ {
		now := start.Add(time.Duration(tick+1) * pullTick)
		out.n = int64(tick+1) * 128
		if b.expired(now) {
			t.Fatalf("fired at %s while output was still arriving", now.Sub(start))
		}
	}
	if b.expired(start.Add(30 * pullTick).Add(pullStallWindow - time.Second)) {
		t.Fatal("fired before the stall window elapsed")
	}
	if !b.expired(start.Add(30 * pullTick).Add(pullStallWindow)) {
		t.Fatal("stall window did not fire")
	}
}

// The retained output window caps at 4 KB. A watchdog reading
// the window's LENGTH loses progress detection the moment the cap fills —
// a long extraction that keeps writing then dies at a deadline while work
// continues (the A07 failure shape). Progress must come from a grow-only
// byte count: writes every tick, far past the cap, must never fire.
func TestPullBudgetOutputPastTailCapStillCountsAsProgress(t *testing.T) {
	start := time.Now()
	out := &preparationTail{limit: 4096}
	b := newTestPullBudget(start, func() (int64, bool) { return 0, false }, out)
	var written int64
	// 160 ticks * 5s = 800s, past the 300s base budget AND past the stall
	// window measured from when the retained window filled (tick 16).
	for tick := 0; tick < 160; tick++ {
		now := start.Add(time.Duration(tick+1) * pullTick)
		n, _ := out.Write(bytes.Repeat([]byte("x"), 256))
		written += int64(n)
		if b.expired(now) {
			t.Fatalf("fired at %s with output still arriving (%d bytes written, window capped at 4096)", now.Sub(start), written)
		}
	}
	if written != 160*256 {
		t.Fatalf("wrote %d bytes, want %d", written, 160*256)
	}
	// Silence after the last write: fires one stall window later.
	last := start.Add(160 * pullTick)
	if b.expired(last.Add(pullStallWindow - time.Second)) {
		t.Fatal("fired before the stall window elapsed")
	}
	if !b.expired(last.Add(pullStallWindow)) {
		t.Fatal("stall window did not fire after output stopped")
	}
}

// /proc/net/dev is cumulative since boot. Starting from
// lastRx=0, the first sample of a long-up host always reads as progress,
// so the base budget never ruled a pull that never downloaded anything.
// The first sample is a baseline, never progress.
func TestPullBudgetBootCounterIsNotProgress(t *testing.T) {
	start := time.Now()
	// Host up for weeks: a constant cumulative counter far above threshold.
	b := newTestPullBudget(start, func() (int64, bool) { return 6 << 30, true }, &fakePullOutput{})
	if b.expired(start.Add(pullTick)) {
		t.Fatal("fired at the first tick")
	}
	// No output, no new bytes: the BASE budget must fire on schedule.
	if !b.expired(start.Add(composePullTimeout)) {
		t.Fatal("boot-cumulative RX counted as progress; base budget did not rule a stalled pull")
	}
	if reason := b.stop(); !strings.Contains(reason, "stopped making progress") {
		t.Fatalf("missing stall reason: %q", reason)
	}
}

func TestPullBudgetHardCeiling(t *testing.T) {
	start := time.Now()
	var rx int64
	b := newTestPullBudget(start, func() (int64, bool) { return rx, true }, &fakePullOutput{})
	for tick := 0; tick < int(composePullCeiling/pullTick)+2; tick++ {
		now := start.Add(time.Duration(tick+1) * pullTick)
		rx += pullMinRxProgress
		if now.Before(start.Add(composePullCeiling)) {
			if b.expired(now) {
				t.Fatalf("fired at %s under the ceiling", now.Sub(start))
			}
			continue
		}
		if !b.expired(now) {
			t.Fatalf("ceiling did not fire at %s", now.Sub(start))
		}
		if reason := b.stop(); !strings.Contains(reason, "absolute budget ceiling") {
			t.Fatalf("missing ceiling reason: %q", reason)
		}
		return
	}
	t.Fatal("ceiling never fired")
}

func TestPullBudgetCancelFiresWithReason(t *testing.T) {
	start := time.Now()
	cancelled := false
	b := newTestPullBudget(start, func() (int64, bool) { return 0, true }, &fakePullOutput{})
	b.cancel = func() { cancelled = true }
	if !b.expired(start.Add(composePullTimeout + pullTick)) {
		t.Fatal("deadline did not fire")
	}
	if !cancelled {
		t.Fatal("deadline did not cancel the command context")
	}
	if b.stop() == "" {
		t.Fatal("missing budget reason")
	}
}
