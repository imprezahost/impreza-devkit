package executor

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var fullGitCommit = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

func validateGitCommit(commit string) error {
	if commit != "" && !fullGitCommit.MatchString(commit) {
		return fmt.Errorf("requested Git commit must be a full 40 or 64 character hexadecimal object ID")
	}
	return nil
}

// Reuse the clone's authentication only for the exact-object fetch. The
// caller's deadline bounds all subprocesses and credentials are never logged.
func checkoutGitCommit(ctx context.Context, dir, commit string, env, preArgs []string) error {
	if err := validateGitCommit(commit); err != nil {
		return err
	}
	if commit == "" {
		return nil
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, preArgs...), args...)...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.Output()
	}
	head, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("cannot verify cloned Git commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(head)), commit) {
		if _, err = run("fetch", "--depth=1", "--no-tags", "origin", strings.ToLower(commit)); err != nil {
			return fmt.Errorf("requested Git commit %s is unavailable; refusing to deploy another revision: %w", commit, err)
		}
	}
	if _, err = run("checkout", "--detach", strings.ToLower(commit), "--"); err != nil {
		return fmt.Errorf("cannot check out requested Git commit %s: %w", commit, err)
	}
	head, err = run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(head)), commit) {
		return fmt.Errorf("Git commit verification failed; refusing to deploy another revision")
	}
	return nil
}
