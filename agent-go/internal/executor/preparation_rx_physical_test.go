package executor

// The pull watchdog's RX sample summed every interface
// but loopback. Container traffic crosses veth*, docker0 and br-* on top
// of the uplink, so it counted twice — and on a busy Tor host the stall
// detection never fired (only the 45-minute ceiling did). Only physical
// interfaces count now.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// procRx reads the received-byte counter of every interface.
func procRx(t *testing.T) map[string]int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		t.Fatal(err)
	}
	rx := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		name, counters, ok := strings.Cut(line, ":")
		fields := strings.Fields(counters)
		if !ok || len(fields) == 0 {
			continue
		}
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			rx[strings.TrimSpace(name)] = v
		}
	}
	return rx
}

// Against the host's real counters: hostRxBytes must equal the sum over
// the physical interfaces, read before and after it (the counters only
// grow). Skips when no virtual interface has received anything — nothing
// then tells the two sums apart; run it on a host after container
// traffic.
func TestHostRxIgnoresVirtualInterfaces(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc and /sys")
	}
	physical := func(rx map[string]int64) (sum, virtual int64) {
		for name, v := range rx {
			if _, err := os.Lstat(filepath.Join("/sys/devices/virtual/net", name)); err == nil {
				if name != "lo" {
					virtual += v
				}
				continue
			}
			sum += v
		}
		return sum, virtual
	}
	before, virtual := physical(procRx(t))
	if virtual == 0 {
		t.Skip("no virtual interface has received traffic on this host")
	}
	got, ok := hostRxBytes()
	after, _ := physical(procRx(t))
	if !ok {
		t.Fatal("RX counter unavailable on a Linux host with physical interfaces")
	}
	if got < before || got > after {
		t.Fatalf("hostRxBytes = %d, physical interfaces read %d..%d (virtual interfaces hold %d): container traffic is counted", got, before, after, virtual)
	}
}
