package executor

// A worker unit that refuses to stop must NOT be reported as the defined,
// retryable failure — a live worker may still be running, so the operation
// keeps the conservative "review required".

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPullWorkerNoReceiptUnstoppableUnitRequiresReview(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux private filesystem and command execution")
	}
	defer withPullBudgetKnobs(300*time.Millisecond, 400*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)()
	orig := preparationUnitStopTimeout
	preparationUnitStopTimeout = 900 * time.Millisecond
	defer func() { preparationUnitStopTimeout = orig }()

	d, r, bin := workFixtureSystemd(t, "pull")
	// This systemctl never lets the unit go inactive.
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$*\" >> \""+bin+"/systemctl.log\"\nif [ \"$1\" = \"show\" ]; then echo active; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	if err == nil {
		t.Fatal("a wedged, unstoppable worker produced no error")
	}
	if strings.Contains(err.Error(), "can be retried") {
		t.Fatalf("defined retryable failure reported for a worker never confirmed stopped: %v", err)
	}
	if !strings.Contains(err.Error(), "review required") {
		t.Fatalf("expected the conservative review-required failure, got: %v", err)
	}
}
