package executor

// The rule of preparation_rx_physical_test.go against a fixed
// /proc/net/dev and sysfs listing.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhysicalRxBytesSkipsVirtualInterfaces(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "dev")
	virtual := filepath.Join(dir, "virtual")
	if err := os.WriteFile(proc, []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 900000000 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 5 5 0 0 0 0 0 0
  ens19: 20 1 0 0 0 0 0 0 5 5 0 0 0 0 0 0
docker0: 70000 7 0 0 0 0 0 0 5 5 0 0 0 0 0 0
vethab12cd: 80000 8 0 0 0 0 0 0 5 5 0 0 0 0 0 0
br-0123456789ab: 90000 9 0 0 0 0 0 0 5 5 0 0 0 0 0 0
  wg0: 60000 6 0 0 0 0 0 0 5 5 0 0 0 0 0 0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lo", "docker0", "vethab12cd", "br-0123456789ab", "wg0"} {
		if err := os.MkdirAll(filepath.Join(virtual, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := physicalRxBytes(proc, virtual)
	if !ok || got != 1020 {
		t.Fatalf("physical RX = %d (ok %v), want 1020 (eth0 + ens19 only)", got, ok)
	}
	// Loopback stays out even where sysfs does not list it.
	if err := os.Remove(filepath.Join(virtual, "lo")); err != nil {
		t.Fatal(err)
	}
	if got, _ := physicalRxBytes(proc, virtual); got != 1020 {
		t.Fatalf("loopback counted: %d", got)
	}
	// No sysfs listing: the physical set cannot be told apart.
	if _, ok := physicalRxBytes(proc, filepath.Join(dir, "missing")); ok {
		t.Fatal("RX reported without a virtual-device listing")
	}
	// Only virtual interfaces: no RX signal at all, never their traffic.
	for _, name := range []string{"eth0", "ens19"} {
		if err := os.MkdirAll(filepath.Join(virtual, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := physicalRxBytes(proc, virtual); ok || got != 0 {
		t.Fatalf("virtual-only host reported RX %d (ok %v)", got, ok)
	}
}
