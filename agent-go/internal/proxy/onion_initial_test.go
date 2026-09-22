package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialOnionProfileInstalledBeforeServiceRendering(t *testing.T) {
	tor := newTestTor(t)
	if err := tor.PrepareInitialProfile("dpl_initial", "hardened"); err != nil {
		t.Fatal(err)
	}
	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "HiddenServiceMaxStreams 48") {
		t.Fatal("initial service rendered without the requested protection")
	}
	if err := tor.PrepareInitialProfile("dpl_initial", "hardened"); err != nil {
		t.Fatal("retry", err)
	}
	if err := tor.PrepareInitialProfile("dpl_initial", "standard"); err == nil {
		t.Fatal("initialization silently downgraded an existing service")
	}
	if err := tor.PrepareInitialProfile("dpl_invalid", "invalid"); err == nil {
		t.Fatal("invalid tier accepted")
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "services/dpl_invalid")); !os.IsNotExist(err) {
		t.Fatal("invalid input created a service")
	}
}

func TestImportedIdentityAndProfileInstalledTogether(t *testing.T) {
	tor := newTestTor(t)
	secret, public, address, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	got, err := tor.ImportOnionKeyWithProfile("dpl_import", secret, public, "max")
	if err != nil || got != address {
		t.Fatal(got, err)
	}
	if tor.profileOf("dpl_import") != ProfileMax {
		t.Fatal("import discarded requested profile")
	}
	if got, err := tor.ImportOnionKeyWithProfile("dpl_import", secret, public, "max"); err != nil || got != address {
		t.Fatal("identical import retry", err)
	}
	if _, err := tor.ImportOnionKeyWithProfile("dpl_import", secret, public, "standard"); err == nil {
		t.Fatal("import retry downgraded policy")
	}
	if _, err := tor.ImportOnionKeyWithProfile("dpl_bad", secret, public, "bogus"); err == nil {
		t.Fatal("invalid import profile accepted")
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "services/dpl_bad")); !os.IsNotExist(err) {
		t.Fatal("invalid import became discoverable")
	}
}
