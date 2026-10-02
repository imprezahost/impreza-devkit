package ingress

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Only the Reloaded member triggers; other firewalld traffic on the bus does
// not. A monitor that ends (firewalld restarted) is started again.
func TestWatchFirewalldReloadTriggersOnlyOnReloaded(t *testing.T) {
	var starts, reloads int32
	outputs := []string{
		"/org/fedoraproject/FirewallD1: org.fedoraproject.FirewallD1.config.zone.Updated ('public',)\n" +
			"/org/fedoraproject/FirewallD1: org.fedoraproject.FirewallD1.Reloaded ()\n",
		"/org/fedoraproject/FirewallD1: org.freedesktop.DBus.Properties.PropertiesChanged ('x', {}, @as [])\n" +
			"/org/fedoraproject/FirewallD1: org.fedoraproject.FirewallD1.Reloaded ()\n",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := func(context.Context) (io.ReadCloser, func() error, error) {
		n := atomic.AddInt32(&starts, 1)
		if int(n) > len(outputs) {
			cancel()
			return nil, nil, errors.New("gone")
		}
		return io.NopCloser(strings.NewReader(outputs[n-1])), func() error { return nil }, nil
	}
	done := make(chan struct{})
	go func() {
		watchReload(ctx, start, func() { atomic.AddInt32(&reloads, 1) }, time.Millisecond, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop")
	}
	if reloads != 2 || starts < 3 {
		t.Fatalf("reloads=%d starts=%d", reloads, starts)
	}
}

// No gdbus or no firewalld: the watcher keeps retrying with a growing,
// capped backoff and never calls back.
func TestWatchFirewalldReloadBacksOffWithoutMonitor(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := func(context.Context) (io.ReadCloser, func() error, error) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return nil, nil, errors.New("gdbus not available")
	}
	watchReload(ctx, start, func() { t.Error("reload without a monitor") }, 10*time.Millisecond, 40*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 3 || len(times) > 12 {
		t.Fatalf("%d attempts in 200 ms", len(times))
	}
	if gap := times[len(times)-1].Sub(times[len(times)-2]); gap < 35*time.Millisecond {
		t.Fatalf("backoff not capped at the maximum: last gap %s", gap)
	}
}

// Each signal runs the reconcile at each delay; a burst while a series is
// pending coalesces into one more series at most.
func TestAfterReloadRunsTheSeriesAndCoalesces(t *testing.T) {
	old := ReloadDelays
	ReloadDelays = []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	defer func() { ReloadDelays = old }()
	var runs int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := AfterReload(ctx, func() { atomic.AddInt32(&runs, 1) })
	trigger()
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 5; i++ {
		trigger() // one queued, the rest dropped
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&runs); n != 6 {
		t.Fatalf("want 2 series of 3 runs, got %d runs", n)
	}
}
