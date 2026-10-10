package executor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The NORMAL path of the staged build-context swap, exercised the way production
// hits it — two consecutive swaps over the same build-ctx. On Linux a
// rename over an existing non-empty directory always fails, so before the
// .old dance every git redeploy rode the remove-then-rename fallback and
// nothing in the package tested it. Removing the fallback (a
// no-fallback mutation) must fail the second swap here, on every OS.
func TestTwoConsecutiveSwapsOverTheSameBuildContext(t *testing.T) {
	final := filepath.Join(t.TempDir(), "build-ctx")
	staged := final + ".clone"

	fill := func(dir, marker string) {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a.txt", filepath.Join("sub", "b.txt")} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(dir string) string {
		raw, err := os.ReadFile(filepath.Join(dir, "a.txt"))
		if err != nil {
			return ""
		}
		return string(raw)
	}

	// First deploy: no previous context — the plain rename path.
	fill(staged, "v1")
	if err := swapStagedBuildContext(nil, staged, final); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(staged)
	if read(final) != "v1" || !os.IsNotExist(statErr) {
		t.Fatalf("first swap: content=%q staged-leftover=%v", read(final), statErr)
	}

	// Redeploy over the existing, NON-EMPTY context — the path every git
	// redeploy takes (the plain rename fails on POSIX and Windows).
	fill(staged, "v2")
	if err := swapStagedBuildContext(nil, staged, final); err != nil {
		t.Fatalf("the second swap over a non-empty context failed (%s): %v", runtime.GOOS, err)
	}
	if read(final) != "v2" {
		t.Fatalf("second swap left the old content: %q", read(final))
	}
	for _, leftover := range []string{staged, final + ".old"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("leftover %s after the swap", leftover)
		}
	}
	// The subdirectory came along (the whole tree moved, not a partial copy).
	if raw, err := os.ReadFile(filepath.Join(final, "sub", "b.txt")); err != nil || string(raw) != "v2" {
		t.Fatalf("subtree missing after the swap (%v)", err)
	}
}

// The failure path restores the previous context: a staged tree that
// vanishes between the two renames leaves the OLD context serving and no
// .old leftover — the property the staged swap exists for. (A vanished staged dir is
// the injection: the aside rename already happened, the second rename
// cannot succeed.)
func TestSwapFailureBetweenRenamesRestoresThePreviousContext(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "build-ctx")
	staged := final + ".clone"
	if err := os.MkdirAll(final, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(final, "a.txt"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "a.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The staged tree disappears after the aside rename would have run.
	if err := os.RemoveAll(staged); err != nil {
		t.Fatal(err)
	}
	if err := swapStagedBuildContext(nil, staged, final); err == nil {
		t.Fatal("a vanished staged tree produced success")
	}
	raw, rerr := os.ReadFile(filepath.Join(final, "a.txt"))
	if rerr != nil || string(raw) != "previous" {
		t.Fatalf("the previous context was not restored: %q (%v)", string(raw), rerr)
	}
	if _, err := os.Stat(final + ".old"); !os.IsNotExist(err) {
		t.Fatal("the .old leftover was not cleaned up by the restore")
	}
}
