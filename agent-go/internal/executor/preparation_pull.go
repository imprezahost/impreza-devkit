package executor

// Supervised pull budget. `docker compose pull` on a 100 Mbit VPS
// takes longer than five minutes for any image set above ~3,75 GB
// compressed, and the old fixed deadline turned that into an ambiguous
// "review required" that blocked every later command on the host. The
// budget below keeps deadline semantics but makes two things true:
//
//   - a pull that keeps making progress (registry bytes on the wire, or
//     new command output) keeps its deadline extended, up to an absolute
//     ceiling;
//   - whenever the budget does expire, the worker records a DEFINED,
//     retryable failure with the reason in the retained output — a pull
//     only populates the local image cache, so retrying it is always safe.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pullTick is how often the watchdog samples progress signals. A var so
// the watchdog test can shrink it.
var pullTick = 5 * time.Second

// pullMinRxProgress is the per-tick received-byte delta that counts as
// progress. Below ~200 KB/s sustained, a multi-GB pull is hopeless on the
// plans this agent serves; heartbeats and panel traffic stay far under.
const pullMinRxProgress = 1 << 20

// progressReader exposes how many bytes the watched command has written
// in TOTAL. It must be grow-only: a capped buffer length stops growing
// once the retained window fills, and output progress detection would die
// with it.
type progressReader interface {
	Total() int64
}

// rxSampler returns the host's cumulative received bytes on its physical
// interfaces. The bool is false when the counter is unavailable
// (non-Linux, /proc unreadable, no physical interface) — progress then
// relies on command output alone.
type rxSampler func() (int64, bool)

func hostRxBytes() (int64, bool) {
	return physicalRxBytes("/proc/net/dev", "/sys/devices/virtual/net")
}

// physicalRxBytes sums the received-byte counters in procNetDev of the
// interfaces virtualDir does not list. Virtual interfaces stay out:
// container traffic crosses veth*, docker0 and br-* on top of the uplink,
// so the old all-but-loopback sum counted it twice, and on a busy Tor host
// the stall detection never fired. The kernel lists every virtual device
// (lo included) under /sys/devices/virtual/net, whatever its name.
func physicalRxBytes(procNetDev, virtualDir string) (int64, bool) {
	if info, err := os.Stat(virtualDir); err != nil || !info.IsDir() {
		return 0, false
	}
	data, err := os.ReadFile(procNetDev)
	if err != nil {
		return 0, false
	}
	var total int64
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		name, counters, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || name == "lo" || strings.ContainsAny(name, `/\`) {
			continue
		}
		// A stat error other than absence cannot prove the interface
		// physical; leaving it out only makes the stall detection stricter.
		if _, err := os.Lstat(filepath.Join(virtualDir, name)); !os.IsNotExist(err) {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 1 {
			continue
		}
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			total += v
			found = true
		}
	}
	return total, found
}

// pullBudget supervises one pull command and owns its context deadline:
// base from launch, stall-window extensions while progress is observed,
// hard-capped at the absolute ceiling.
type pullBudget struct {
	mu sync.Mutex

	start        time.Time
	base         time.Duration
	stall        time.Duration
	ceiling      time.Duration
	lastRx       int64
	rxInit       bool
	lastOut      int64
	lastProgress time.Time
	progressed   bool
	reason       string

	done   chan struct{}
	sample rxSampler
	output progressReader
	cancel context.CancelFunc
}

// newPullBudget starts the watchdog for one pull command. cancel belongs
// to the command's context; stop must be called once the command
// returned, and returns the reason when the budget killed it.
//
// Baseline samples are taken NOW: /proc/net/dev is cumulative since boot
// and the output buffer may already hold bytes, so their absolute values
// must never count as this pull's progress (with lastRx=0
// the first tick always "made progress" and the base budget never ruled).
func newPullBudget(cancel context.CancelFunc, output progressReader, sample rxSampler) *pullBudget {
	b := &pullBudget{
		start:   time.Now(),
		base:    composePullTimeout,
		stall:   pullStallWindow,
		ceiling: composePullCeiling,
		sample:  sample,
		output:  output,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	b.lastOut = output.Total()
	if rx, ok := sample(); ok {
		b.lastRx = rx
		b.rxInit = true
	}
	go b.watch()
	return b
}

// stop halts the watchdog and reports why the budget expired, if it did.
// Call after the command returned and before writing the receipt.
func (b *pullBudget) stop() string {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

func (b *pullBudget) watch() {
	ticker := time.NewTicker(pullTick)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			if b.expired(time.Now()) {
				return
			}
		}
	}
}

// expired evaluates one sample; it fires the cancellation exactly once.
func (b *pullBudget) expired(now time.Time) bool {
	rx, rxOK := b.sample()
	outTotal := b.output.Total()

	b.mu.Lock()
	progress := outTotal > b.lastOut
	if rxOK {
		if !b.rxInit {
			// The counter is cumulative since boot: the first successful
			// sample only establishes the baseline, it is not progress.
			b.rxInit = true
		} else if rx-b.lastRx >= pullMinRxProgress {
			progress = true
		}
	}
	if progress {
		b.progressed = true
		b.lastProgress = now
	}
	// Deadline: the base budget from launch; once progress was observed,
	// a stall window measured from the last progress; never past the
	// absolute ceiling.
	deadline := b.start.Add(b.base)
	if b.progressed {
		deadline = b.lastProgress.Add(b.stall)
	}
	if hardCap := b.start.Add(b.ceiling); deadline.After(hardCap) {
		deadline = hardCap
	}
	b.lastOut = outTotal
	if rxOK {
		b.lastRx = rx
	}
	fired := !now.Before(deadline)
	if fired && b.reason == "" {
		if !now.Before(b.start.Add(b.ceiling)) {
			b.reason = fmt.Sprintf("supervised pull exceeded its absolute budget ceiling of %s; the deployment can be retried", b.ceiling)
		} else {
			b.reason = fmt.Sprintf("supervised pull stopped making progress for %s (no registry bytes or command output); the deployment can be retried", b.stall)
		}
	}
	b.mu.Unlock()
	if fired && b.cancel != nil {
		b.cancel()
	}
	return fired
}
