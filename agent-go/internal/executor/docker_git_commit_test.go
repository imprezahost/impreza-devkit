package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCommitCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "-b", "main")
	commit := func(value string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "value"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		git(repo, "add", "value")
		git(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", value)
		return git(repo, "rev-parse", "HEAD")
	}
	old := commit("old")
	newer := commit("new")
	clone := filepath.Join(t.TempDir(), "clone")
	git(repo, "clone", "--no-local", repo, clone)
	for _, sha := range []string{strings.ToUpper(old), newer, newer} {
		if err := checkoutGitCommit(context.Background(), clone, sha, os.Environ(), nil); err != nil {
			t.Fatal(err)
		}
		if got := git(clone, "rev-parse", "HEAD"); got != strings.ToLower(sha) {
			t.Fatalf("wrong commit %s", got)
		}
	}
	for _, sha := range []string{"--help", "HEAD", "abc1234", "../../bad", strings.Repeat("0", 40)} {
		if err := checkoutGitCommit(context.Background(), clone, sha, os.Environ(), nil); err == nil {
			t.Fatalf("accepted %q", sha)
		}
		if got := git(clone, "rev-parse", "HEAD"); got != newer {
			t.Fatal("failure changed checked-out commit")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkoutGitCommit(ctx, clone, old, os.Environ(), nil); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := checkoutGitCommit(context.Background(), clone, "", os.Environ(), nil); err != nil {
		t.Fatal(err)
	}
	// A requested SHA that names a blob rather than a commit must never pass.
	blob := git(repo, "rev-parse", "HEAD:value")
	if err := checkoutGitCommit(context.Background(), clone, blob, os.Environ(), nil); err == nil {
		t.Fatal("accepted non-commit object")
	}
}
