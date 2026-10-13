//go:build linux

package executor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func ownerOf(t *testing.T, p string) (int, int) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return int(st.Uid), int(st.Gid)
}

// As root: the ownership pass covers the prepared bind sources and
// never follows a link the containers planted in ./data.
func TestApplyDataDirOwnerNeverFollowsLinksRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (real chown)")
	}
	base := t.TempDir()
	data := filepath.Join(base, "app", "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "host-file")
	if err := os.WriteFile(victim, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	victimDir := filepath.Join(base, "host-dir")
	if err := os.MkdirAll(filepath.Join(victimDir, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(data, "evil-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimDir, filepath.Join(data, "evil-dir")); err != nil {
		t.Fatal(err)
	}
	if err := prepareDataBinds(data, []string{filepath.Join(data, "state"), filepath.Join(data, "workspace", "sub")}); err != nil {
		t.Fatal(err)
	}
	if err := applyDataDirConfig(data, &sdkclient.DataDirConfig{Owner: "1000:1000", Mode: "0755"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{data, filepath.Join(data, "state"), filepath.Join(data, "workspace"), filepath.Join(data, "workspace", "sub")} {
		if u, g := ownerOf(t, p); u != 1000 || g != 1000 {
			t.Fatalf("%s owned by %d:%d, want 1000:1000", p, u, g)
		}
	}
	for _, p := range []string{victim, victimDir, filepath.Join(victimDir, "inner")} {
		if u, g := ownerOf(t, p); u != 0 || g != 0 {
			t.Fatalf("a planted link was followed: host path %s is now %d:%d", p, u, g)
		}
	}
	// the link entries themselves may change owner; their targets never do
	if u, _ := ownerOf(t, filepath.Join(data, "evil-file")); u != 1000 {
		t.Fatalf("the link entry itself kept owner %d", u)
	}
}

func TestApplyDataDirRefusesLinkedDataDirRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (real chown)")
	}
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(base, "app")
	if err := os.MkdirAll(app, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(app, "data")); err != nil {
		t.Fatal(err)
	}
	if err := applyDataDirConfig(filepath.Join(app, "data"), &sdkclient.DataDirConfig{Owner: "1000", Mode: "0777"}); err == nil {
		t.Fatal("a linked data directory was accepted")
	}
	if u, _ := ownerOf(t, target); u != 0 {
		t.Fatal("the link target changed owner")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o700 {
		t.Fatal("the link target changed mode")
	}
}
