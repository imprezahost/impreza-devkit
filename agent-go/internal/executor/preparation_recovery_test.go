package executor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func recoveryFixture() *PreparationRecovery {
	return &PreparationRecovery{Version: 1, Phase: "ready", DeploymentID: "dpl_test", Files: []PreparationFile{{Name: "compose.yaml", Data: []byte("previous"), Mode: 0600, Exists: true}, {Name: ".env"}, {Name: "startup.json"}}, Containers: []string{strings.Repeat("a", 64)}}
}
func TestRecoveryRejectsUnprovenOrInvalidState(t *testing.T) {
	for _, phase := range []string{"busy", "blocked", "replacing"} {
		r := recoveryFixture()
		r.Phase = phase
		if r.Validate() != nil || r.Recoverable() {
			t.Fatal(phase)
		}
	}
	for _, mutate := range []func(*PreparationRecovery){func(r *PreparationRecovery) { r.Version = 2 }, func(r *PreparationRecovery) { r.Phase = "finished" }, func(r *PreparationRecovery) { r.DeploymentID = "../../outside" }, func(r *PreparationRecovery) { r.Files[0].Name = "../../secret" }, func(r *PreparationRecovery) { r.Files = r.Files[:2] }, func(r *PreparationRecovery) { r.Files[0].Mode = 04755 }, func(r *PreparationRecovery) { r.Containers = append(r.Containers, r.Containers[0]) }, func(r *PreparationRecovery) { r.Containers = []string{"not-an-id"} }} {
		r := recoveryFixture()
		mutate(r)
		if r.Validate() == nil || r.Recoverable() {
			t.Fatal("invalid state accepted")
		}
	}
	if (&PreparationRecovery{Version: 1, Phase: "unstarted"}).Recoverable() != true {
		t.Fatal("unstarted marker rejected")
	}
	if (*PreparationRecovery)(nil).Recoverable() {
		t.Fatal("legacy state accepted")
	}
}
func TestRecoveryRestoresExactlyAndRejectsDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux filesystem and Docker command fixture")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0700)
	id := strings.Repeat("a", 64)
	script := filepath.Join(bin, "docker")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' "+id+"\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := &Docker{StateDir: root}
	dir := d.appDir("dpl_test")
	os.MkdirAll(dir, 0700)
	r := recoveryFixture()
	for _, name := range []string{"compose.yaml", ".env", "startup.json"} {
		os.WriteFile(filepath.Join(dir, name), []byte("new"), 0600)
	}
	os.Mkdir(filepath.Join(dir, "data"), 0700)
	os.WriteFile(filepath.Join(dir, "data", "sentinel"), []byte("keep"), 0600)
	for i := 0; i < 2; i++ {
		if err := d.ReconcilePreparation(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if string(b) != "previous" {
		t.Fatal("configuration not restored")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "data", "sentinel"))
	if string(b) != "keep" {
		t.Fatal("data touched")
	}
	for _, name := range []string{".env", "startup.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("new file not removed")
		}
	}
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' "+strings.Repeat("b", 64)+"\n"), 0700)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("drift"), 0600)
	if err := d.ReconcilePreparation(context.Background(), r); err == nil {
		t.Fatal("container identity drift accepted")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if string(b) != "drift" {
		t.Fatal("wrote before rejecting drift")
	}
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' "+id+"\n"), 0700)
	os.Symlink(filepath.Join(dir, "data", "sentinel"), filepath.Join(dir, ".env"))
	if err := d.ReconcilePreparation(context.Background(), r); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestRecoveryDoesNotCreateMissingApplicationDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux Docker command fixture")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0700)
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' "+strings.Repeat("a", 64)+"\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := &Docker{StateDir: root}
	if err := d.ReconcilePreparation(context.Background(), recoveryFixture()); err == nil {
		t.Fatal("missing app directory accepted")
	}
	if _, err := os.Stat(d.appDir("dpl_test")); !os.IsNotExist(err) {
		t.Fatal("missing state recreated")
	}
}
