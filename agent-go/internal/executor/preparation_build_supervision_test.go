package executor

// The supervision decision, the gate of the terminal release, the failure
// of a Blocked deployment's controlled build, and the synchronous
// preparation step killing its whole process group.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A Blocked deployment must never end pending, so its worker's pending
// outcome becomes a failure. For a controlled build that failure is never
// "retryable": the builder container can outlive the stopped unit.
func TestBlockedControlledBuildFailureIsNeverRetryable(t *testing.T) {
	_, r, bin := workFixture(t, "build", "exit 0")
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := settleBlockedWork(r.Work, errors.New("agent shutting down"))
	if err == nil || strings.Contains(err.Error(), "can be retried") || !strings.Contains(err.Error(), "review the host") {
		t.Fatalf("a controlled build's pending outcome must fail with the review note: %v", err)
	}
	_, pull, pullBin := workFixture(t, "pull", "exit 0")
	if err := os.WriteFile(filepath.Join(pullBin, "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := settleBlockedWork(pull.Work, errors.New("agent shutting down")); err == nil || !strings.Contains(err.Error(), "can be retried") {
		t.Fatalf("a stopped pull worker is the defined, retryable failure: %v", err)
	}
}

func TestPreparationSupervisedKeepsControlledBuildsOnTheWorker(t *testing.T) {
	d := &Docker{StateDir: t.TempDir()}
	blocked := &PreparationRecovery{Phase: "busy", Blocked: true}
	plain := &PreparationRecovery{Phase: "busy"}
	if !d.preparationSupervised("pull", blocked) || !d.preparationSupervised("build", plain) {
		t.Fatal("pulls and plain builds go to the worker")
	}
	if d.preparationSupervised("config", plain) || d.preparationSupervised("build", nil) {
		t.Fatal("only pull and build are supervised, and only with a checkpoint")
	}
	if d.preparationSupervised("build", blocked) {
		t.Fatal("without controlled builds a Blocked deployment builds synchronously")
	}
	policy, _ := json.Marshal(controlledBuildPolicy{Version: 1, Enabled: true, Image: ownedBuilderImage})
	if err := os.WriteFile(filepath.Join(d.StateDir, "controlled-builds.json"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.preparationSupervised("build", blocked) {
		t.Fatal("a Blocked deployment on a controlled-build host bypassed the controlled builder")
	}
	if err := os.WriteFile(filepath.Join(d.StateDir, "controlled-builds.json"), []byte(`{"version":1,"enabled":true,"image":"sha256:other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.preparationSupervised("build", blocked) {
		t.Fatal("an unreadable controlled-build policy must reach the worker, which refuses it")
	}
}

func TestSynchronousPreparationKillsTheProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process groups")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// Like the compose plugin: a child keeps the output pipe open after the CLI dies.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nsleep 60 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := &Docker{StateDir: root}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, _ = d.composePreparation(ctx, root, "build")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the synchronous build outlived its deadline by %s: the surviving child held the call", elapsed)
	}
}

// The terminal release needs nothing of the work left that could change
// images: a stopped unit ends a plain build, never a controlled one, whose
// builder container can outlive the unit.
func TestPreparationWorkerGoneExcludesControlledBuilds(t *testing.T) {
	d, r, bin := workFixture(t, "build", "exit 0")
	if d.PreparationWorkerGone(r) {
		t.Fatal("a live unit counted as gone")
	}
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !d.PreparationWorkerGone(r) {
		t.Fatal("a stopped plain build worker was not gone")
	}
	if d.PreparationWorkerGone(nil) {
		t.Fatal("no work is not a gone worker")
	}
	owned, or := buildWorkFixture(t, true)
	if err := os.WriteFile(filepath.Join(owned.StateDir, "bin", "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if owned.PreparationWorkerGone(or) {
		t.Fatal("a controlled build counted as gone: its builder container can outlive the unit")
	}
}
