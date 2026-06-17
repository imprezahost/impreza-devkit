//go:build linux

// Package sysload reads the host's resource utilisation for the
// AgentReport.Load field sent on every heartbeat. Phase 9.23.
//
// We avoid the gopsutil dependency tree deliberately — everything we
// need is in /proc and /sys on Linux (the only platform agents run
// on per the systemd unit + install.sh). The data we collect is
// inner-VM, exactly what the customer would see running `top`, `free
// -m`, `df -h /` inside their box. The control plane's AlertScanner
// uses these numbers to decide when to fire a capacity-alert email.
//
// All three readers are best-effort. A read failure returns a zero
// value + an error, never panics. The heartbeat handler logs the
// failure once and ships whatever did read successfully — partial
// data is better than dropping the heartbeat.
//
// Linux-only — non-linux builds get the stub in sysload_other.go
// that returns ErrUnsupported. Windows / macOS dev builds still
// compile; only cross-compiled production builds (GOOS=linux) ship
// the real implementation.
package sysload

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"golang.org/x/sys/unix"
)

// Collect returns the load snapshot the agent sends in its next
// heartbeat. Safe to call from any goroutine.
func Collect() (*sdkclient.AgentLoad, error) {
	cpuPct, err := cpuPercent()
	if err != nil {
		return nil, fmt.Errorf("read cpu: %w", err)
	}
	memUsed, memTotal, err := memInfo()
	if err != nil {
		return nil, fmt.Errorf("read mem: %w", err)
	}
	diskUsed, diskTotal, err := diskUsage("/")
	if err != nil {
		return nil, fmt.Errorf("read disk: %w", err)
	}
	return &sdkclient.AgentLoad{
		CPUPercent:  cpuPct,
		MemUsedMB:   memUsed,
		MemTotalMB:  memTotal,
		DiskUsedGB:  diskUsed,
		DiskTotalGB: diskTotal,
	}, nil
}

// ───────────────────────── CPU ────────────────────────────
//
// We compute CPU% across a short window between two reads of
// /proc/stat. The first call returns the sample taken at process
// start (~0% — we have no prior baseline to diff against), then
// each call advances the baseline so subsequent reads report the
// utilization since the previous heartbeat.

type cpuSnapshot struct {
	idle  uint64
	total uint64
}

var (
	cpuMu        sync.Mutex
	cpuPrev      cpuSnapshot
	cpuPrevAt    time.Time
	cpuHaveSeed  bool
)

func cpuPercent() (float64, error) {
	cur, err := readCPUSnapshot()
	if err != nil {
		return 0, err
	}

	cpuMu.Lock()
	defer cpuMu.Unlock()

	// First call ever — seed the baseline + return 0 (no prior
	// sample to diff against). The next heartbeat will produce a
	// real reading.
	if !cpuHaveSeed {
		cpuPrev = cur
		cpuPrevAt = time.Now()
		cpuHaveSeed = true
		return 0, nil
	}

	// If less than 100ms elapsed (shouldn't happen in normal use)
	// return the previous-computed estimate (which is 0 on first
	// real call). Avoids divide-by-zero when total didn't advance.
	totalDelta := cur.total - cpuPrev.total
	if totalDelta == 0 {
		return 0, nil
	}
	idleDelta := cur.idle - cpuPrev.idle
	cpuPrev = cur
	cpuPrevAt = time.Now()

	busy := totalDelta - idleDelta
	pct := (float64(busy) / float64(totalDelta)) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return round1(pct), nil
}

func readCPUSnapshot() (cpuSnapshot, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSnapshot{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		// Fields: user, nice, system, idle, iowait, irq, softirq,
		// steal, guest, guest_nice. Idle for our purposes = idle +
		// iowait (iowait is "idle but waiting on IO"; counting it as
		// busy double-counts in pathological I/O-bound workloads).
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return cpuSnapshot{}, fmt.Errorf("/proc/stat: malformed cpu line")
		}
		var total uint64
		var idle uint64
		for i := 1; i < len(fields); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuSnapshot{}, fmt.Errorf("/proc/stat: parse field %d: %w", i, err)
			}
			total += v
			if i == 4 || i == 5 { // idle, iowait
				idle += v
			}
		}
		return cpuSnapshot{idle: idle, total: total}, nil
	}
	return cpuSnapshot{}, fmt.Errorf("/proc/stat: no cpu line")
}

// ───────────────────────── Memory ─────────────────────────

// memInfo returns (usedMB, totalMB). "Used" follows the same
// definition `free -m` shows — total - (free + buffers + cached +
// sreclaimable) — which matches what customers see when they SSH
// in. MemAvailable (kernel-computed estimate of how much memory is
// available for new allocations) is the modern equivalent.
func memInfo() (uint64, uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var totalKB, availableKB uint64
	var haveTotal, haveAvailable bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseKB(line)
			haveTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKB = parseKB(line)
			haveAvailable = true
		}
		if haveTotal && haveAvailable {
			break
		}
	}
	if !haveTotal {
		return 0, 0, fmt.Errorf("/proc/meminfo: no MemTotal")
	}
	if !haveAvailable {
		// Older kernels (< 3.14) lack MemAvailable. We could
		// approximate from MemFree + Buffers + Cached + SReclaimable
		// but the agent's minimum target is Ubuntu 22.04 / Debian 12
		// (kernels 5.x+) where MemAvailable is always present.
		return 0, 0, fmt.Errorf("/proc/meminfo: no MemAvailable (kernel too old?)")
	}
	usedKB := totalKB - availableKB
	if availableKB > totalKB {
		usedKB = 0
	}
	return usedKB / 1024, totalKB / 1024, nil
}

func parseKB(line string) uint64 {
	// "MemTotal:        4034408 kB"
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// ───────────────────────── Disk ───────────────────────────
//
// We report rootfs (/) usage only. Customers run Docker on these
// agents and Docker's overlay storage drivers count toward rootfs,
// so "/" usage IS effectively container-storage usage. A future
// follow-up could surface per-volume large-usage warnings; this is
// noted in the Phase 9.23 spec.

func diskUsage(path string) (uint64, uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bavail = blocks free for unprivileged users (≠ Bfree which
	// includes the reserved-for-root pool). Bavail matches what
	// `df -h /` shows the customer.
	totalBytes := st.Blocks * uint64(st.Bsize)
	availBytes := st.Bavail * uint64(st.Bsize)
	usedBytes := totalBytes - availBytes
	const gb = uint64(1) << 30
	return usedBytes / gb, totalBytes / gb, nil
}

// ───────────────────────── Helpers ────────────────────────

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
