package proxy

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestTor(t *testing.T) *Tor {
	t.Helper()
	dir := t.TempDir()
	return &Tor{StateDir: dir, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), reloadFn: func(context.Context) error { return nil }}
}

func (tor *Tor) profileOf(id string) OnionProfile {
	p, err := tor.readProfile(id)
	if err != nil {
		panic(err)
	}
	return p
}

// Uninstall parks the key material instead of deleting it, and the service
// leaves the torrc immediately (the address stops being served).
func TestRemoveHiddenServiceParksKeys(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	svcDir := filepath.Join(tor.StateDir, "services", "dpl_abc")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "hs_ed25519_secret_key"), []byte("testkey"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tor.RemoveHiddenService(ctx, "dpl_abc"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(svcDir); !os.IsNotExist(err) {
		t.Fatal("service dir still present after remove")
	}
	parked := filepath.Join(tor.StateDir, "parked", "dpl_abc", "hs_ed25519_secret_key")
	data, err := os.ReadFile(parked)
	if err != nil {
		t.Fatalf("keys not parked: %v", err)
	}
	if string(data) != "testkey" {
		t.Fatal("parked key content changed")
	}

	torrc, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(torrc), "dpl_abc") {
		t.Fatal("torrc still references the removed service")
	}
}

// A redeploy after uninstall must NOT silently resurrect the parked address:
// the services dir starts empty and Tor would generate fresh keys.
func TestParkedKeysNeverAutoReuse(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	svcDir := filepath.Join(tor.StateDir, "services", "dpl_abc")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tor.RemoveHiddenService(ctx, "dpl_abc"); err != nil {
		t.Fatal(err)
	}
	// Re-provision creates a fresh empty dir; Tor generates new keys there.
	if _, err := tor.ProvisionHiddenService(ctx, "dpl_abc", 80); err == nil {
		// No docker/Tor in the test env — a launch failure is fine, what
		// matters is the filesystem shape it left behind.
		t.Log("provision without docker errored as expected")
	}
	fresh := filepath.Join(tor.StateDir, "services", "dpl_abc")
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("re-provision did not create a fresh service dir")
	}
	if _, err := os.Stat(filepath.Join(fresh, "hs_ed25519_secret_key")); !os.IsNotExist(err) {
		t.Fatal("parked key leaked back into the fresh service dir")
	}
}

// Parked entries older than the retention are pruned on the next remove.
func TestParkedPrune(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	old := filepath.Join(tor.StateDir, "parked", "dpl_old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-ParkRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	svcDir := filepath.Join(tor.StateDir, "services", "dpl_new")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tor.RemoveHiddenService(ctx, "dpl_new"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired parked entry not pruned")
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "parked", "dpl_new")); err != nil {
		t.Fatal("fresh parked entry was pruned")
	}
}

// Remove on a deployment that never had a service stays a no-op.
func TestRemoveHiddenServiceNoop(t *testing.T) {
	tor := newTestTor(t)
	if err := tor.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := tor.RemoveHiddenService(context.Background(), "dpl_missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "parked", "dpl_missing")); !os.IsNotExist(err) {
		t.Fatal("noop remove created a parked entry")
	}
}

// ── C3: restricted discovery (private onion) ─────────────────────────────

const testPubkey = `beaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` // X25519 basepoint 9

func TestSetOnionClientsWritesAndSyncs(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()
	svcDir := filepath.Join(tor.StateDir, "services", "dpl_auth")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Add two clients.
	err := tor.SetOnionClients(ctx, "dpl_auth", map[string]string{
		"alice": testPubkey,
		"bob":   testPubkey,
	})
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(svcDir, "authorized_clients")
	data, err := os.ReadFile(filepath.Join(authDir, "alice.auth"))
	if err != nil {
		t.Fatal(err)
	}
	want := "descriptor:x25519:" + strings.ToUpper(testPubkey) + "\n"
	if string(data) != want {
		t.Fatalf("auth file content wrong: %q", data)
	}
	if runtime.GOOS != "windows" { // Windows FS does not enforce posix modes
		st, _ := os.Stat(filepath.Join(authDir, "alice.auth"))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("auth file mode %v, want 0600", st.Mode().Perm())
		}
	}

	// Sync: revoke bob, keep alice.
	if err := tor.SetOnionClients(ctx, "dpl_auth", map[string]string{"alice": testPubkey}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "bob.auth")); !os.IsNotExist(err) {
		t.Fatal("revoked client file still present")
	}
	names, err := tor.OnionClientNames("dpl_auth")
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("names after revoke: %v %v", names, err)
	}

	// Empty list → public again (no auth files left).
	if err := tor.SetOnionClients(ctx, "dpl_auth", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	names, _ = tor.OnionClientNames("dpl_auth")
	if len(names) != 0 {
		t.Fatalf("expected empty client list, got %v", names)
	}
}

func TestSetOnionClientsValidation(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(tor.StateDir, "services", "dpl_auth"), 0o700); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]map[string]string{
		"bad name traversal": {"../evil": testPubkey},
		"bad name upper":     {"Alice": testPubkey},
		"bad pubkey short":   {"alice": "abc"},
		"bad pubkey chars":   {"alice": "0189" + testPubkey[4:]},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tor.SetOnionClients(ctx, "dpl_auth", tc); err == nil {
				t.Fatal("invalid client entry accepted")
			}
		})
	}
	// A rejected batch must not disturb what's on disk.
	if err := tor.SetOnionClients(ctx, "dpl_auth", map[string]string{"alice": testPubkey}); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionClients(ctx, "dpl_auth", map[string]string{"alice": testPubkey, "bad key": "nope"}); err == nil {
		t.Fatal("mixed batch accepted")
	}
	names, _ := tor.OnionClientNames("dpl_auth")
	if len(names) != 1 || names[0] != "alice" {
		t.Fatal("rejected batch disturbed the existing list")
	}

	// No service dir → clear refusal.
	if err := tor.SetOnionClients(ctx, "dpl_missing", map[string]string{"alice": testPubkey}); err == nil {
		t.Fatal("client update on missing service accepted")
	}
}

// C1: the managed torrc always carries the privacy/observability baseline.
func TestTorrcBaseline(t *testing.T) {
	tor := newTestTor(t)
	if err := tor.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"SafeLogging 1\n",
		"MetricsPort 127.0.0.1:9052\n",
		"MetricsPortPolicy accept 127.0.0.1\n",
		"SocksPort 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("torrc missing %q:\n%s", want, body)
		}
	}
	// MetricsPort must never leave the container loopback.
	if strings.Contains(body, "MetricsPort 0.0.0.0") {
		t.Fatal("metrics exposed beyond loopback")
	}
}

// ── C2: hardening profiles ───────────────────────────────────────────────

func TestOnionProfilesRender(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()
	for _, id := range []string{"dpl_std", "dpl_hard", "dpl_max"} {
		if err := os.MkdirAll(filepath.Join(tor.StateDir, "services", id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	set := func(id, p string) {
		t.Helper()
		if err := tor.SetOnionProfile(ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	set("dpl_hard", "hardened")
	set("dpl_max", "max")

	body, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	torrc := string(body)
	sections := strings.Split(torrc, "HiddenServiceDir /var/lib/tor/services/")
	var secStd, secHard, secMax string
	for _, s := range sections[1:] {
		switch {
		case strings.HasPrefix(s, "dpl_std"):
			secStd = s
		case strings.HasPrefix(s, "dpl_hard"):
			secHard = s
		case strings.HasPrefix(s, "dpl_max"):
			secMax = s
		}
	}
	if !strings.Contains(secStd, "HiddenServiceEnableIntroDoSRatePerSec 25") || strings.Contains(secStd, "MaxStreams") || strings.Contains(secStd, "PoW") {
		t.Fatalf("standard profile wrong:\n%s", secStd)
	}
	if !strings.Contains(secHard, "RatePerSec 50") || !strings.Contains(secHard, "HiddenServiceMaxStreams 48") || strings.Contains(secHard, "PoW") {
		t.Fatalf("hardened profile wrong:\n%s", secHard)
	}
	if !strings.Contains(secMax, "HiddenServiceMaxStreams 16") || !strings.Contains(secMax, "HiddenServicePoWDefensesEnabled 1") || !strings.Contains(secMax, "HiddenServicePoWQueueRate 250") {
		t.Fatalf("max profile wrong:\n%s", secMax)
	}
	// Circuit export stays out of every tier until Caddy speaks PROXY protocol.
	if strings.Contains(torrc, "ExportCircuitID") {
		t.Fatal("circuit export rendered without a PROXY-capable listener")
	}

	// Downgrade back to standard drops the extra lines.
	set("dpl_max", "standard")
	body, _ = os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	for _, s := range strings.Split(string(body), "HiddenServiceDir /var/lib/tor/services/")[1:] {
		if strings.HasPrefix(s, "dpl_max") && strings.Contains(s, "PoW") {
			t.Fatal("downgrade to standard kept PoW lines")
		}
	}

	// Invalid profile refused without touching the stored one.
	if err := tor.SetOnionProfile(ctx, "dpl_std", "paranoid"); err == nil {
		t.Fatal("invalid profile accepted")
	}
	if tor.profileOf("dpl_std") != ProfileStandard {
		t.Fatal("refused profile still applied")
	}
	// Missing service → clear refusal.
	if err := tor.SetOnionProfile(ctx, "dpl_missing", "hardened"); err == nil {
		t.Fatal("profile set on missing service")
	}
}
