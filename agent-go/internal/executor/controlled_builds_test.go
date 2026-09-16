package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestControlledBuildPathsRespectWorkerIsolation(t *testing.T) {
	for _, tc := range []struct {
		binary, state string
		valid         bool
	}{
		{"/usr/local/bin/impreza-agent", "/var/lib/impreza-agent", true},
		{"/opt/impreza/agent", "/srv/impreza", true},
		{"/tmp/agent", "/var/lib/impreza-agent", false},
		{"/var/tmp/agent", "/var/lib/impreza-agent", false},
		{"/usr/local/bin/impreza-agent", "/root/state", false},
		{"/home/user/agent", "/var/lib/impreza-agent", false},
		{"/usr/local/bin/impreza-agent", "/run/user/0/state", false},
		{"/usr/local/bin/impreza-agent", "/etc/impreza-agent", false},
		{"/usr/local/bin/impreza-agent", "/usr/share/impreza", false},
		{"/usr/local/bin/impreza-agent", "/boot/state", false},
		{"/tmp-safe/agent", "/var/lib/state", true},
	} {
		if (controlledBuildPaths(tc.binary, tc.state) == nil) != tc.valid {
			t.Fatal(tc)
		}
	}
}

func TestControlledBuildPolicyFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "disabled", "enabled", "version", "image", "malformed", "unknown", "symlink", "mode"} {
		t.Run(mode, func(t *testing.T) {
			dir, _, _ := builderStoreFixture(t)
			d := &Docker{StateDir: dir}
			policy := controlledBuildPolicy{Version: 1, Enabled: mode != "disabled", Image: ownedBuilderImage}
			if mode == "version" {
				policy.Version = 2
			}
			if mode == "image" {
				policy.Image = "mutable:tag"
			}
			path := filepath.Join(dir, "controlled-builds.json")
			if mode != "missing" {
				if err := writeWorkJSON(dir, "controlled-builds.json", policy); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "malformed":
				os.WriteFile(path, []byte("{"), 0600)
			case "unknown":
				os.WriteFile(path, []byte(`{"version":1,"enabled":true,"image":"x","extra":true}`), 0600)
			case "mode":
				os.Chmod(path, 0644)
			case "symlink":
				other := filepath.Join(dir, "target")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			}
			enabled, err := d.ControlledBuildsEnabled()
			valid := mode == "missing" || mode == "disabled" || mode == "enabled"
			if (err == nil) != valid || enabled != (mode == "enabled") {
				t.Fatal(mode, enabled, err)
			}
		})
	}
}

func TestControlledBuildDisablePreservesPrivateState(t *testing.T) {
	dir, _, _ := builderStoreFixture(t)
	d := &Docker{StateDir: dir}
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.DisableControlledBuilds(); err != nil {
		t.Fatal(err)
	}
	if enabled, err := d.ControlledBuildsEnabled(); err != nil || enabled {
		t.Fatal(enabled, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "unchanged" {
		t.Fatal("state changed", err)
	}
}
