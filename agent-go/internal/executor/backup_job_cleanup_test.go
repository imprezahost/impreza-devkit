package executor

import (
	"context"
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupJobCleanupFailedRestoreRemovesOnlyJob(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	watch := quiesceWatch(func() bool { return quiesceComposeUpSeen(statePath) }, func() { time.Sleep(300 * time.Millisecond); quiesceExitJob(t, statePath, 1) }, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := &sdkclient.PollCommand{ID: "cmd_job_cleanup_failure", Kind: sdkclient.CommandDeploy, Payload: quiescePayload(quiesceTestTarget, nil)}
	result := d.Execute(ctx, cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "nothing was changed") {
		t.Fatalf("wrong result: %s", result.Status)
	}
	if _, err := os.Stat(d.appDir(quiesceTestJob)); !os.IsNotExist(err) {
		t.Fatal("completed job directory retained")
	}
	if raw, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); err != nil || string(raw) != "restored" {
		t.Fatal("application data changed")
	}
	saved, ok := d.quiesceReadResult(quiesceTestJob, cmd.ID)
	if !ok || saved.Error != result.Error {
		t.Fatal("terminal receipt lost")
	}
}

func TestBackupJobCleanupTerminalAndPendingIsolation(t *testing.T) {
	for _, status := range []string{"exited", "running", "restarting", "paused", "created", "dead"} {
		t.Run(status, func(t *testing.T) {
			d := quiesceFixture(t)
			double := quiesceContainers(t, d, "running", status, 0)
			id := quiesceTestJob
			other := "bkpjob_fedcba9876543210"
			for _, job := range []string{id, other} {
				if err := os.MkdirAll(d.appDir(job), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d.appDir(job), ".env"), []byte("fixture-private-token"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.cleanupFinishedBackupJob(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(d.appDir(id))
			if (status == "exited") != os.IsNotExist(err) {
				t.Fatal("job cleanup disagrees with verified runtime state")
			}
			if raw, err := os.ReadFile(filepath.Join(d.appDir(other), ".env")); err != nil || string(raw) != "fixture-private-token" {
				t.Fatal("other job changed")
			}
			if raw, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); err != nil || string(raw) != "restored" {
				t.Fatal("application data changed")
			}
			for _, c := range double.State().Containers {
				if strings.Contains(c.Name, quiesceTestTarget) && c.State.Status != "running" {
					t.Fatal("application stopped")
				}
			}
		})
	}
}
func TestBackupJobCleanupInspectionFailureRetainsArtifacts(t *testing.T) {
	d := quiesceFixture(t)
	if err := os.MkdirAll(d.appDir(quiesceTestJob), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.cleanupFinishedBackupJob(ctx, quiesceTestJob); err == nil {
		t.Fatal("daemon absence trusted")
	}
	if _, err := os.Stat(d.appDir(quiesceTestJob)); err != nil {
		t.Fatal("unverified job removed")
	}
}

func TestBackupJobCleanupMissingReceiptAndOrphanRetain(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-receipt", true: "quarantined"}[orphan], func(t *testing.T) {
			d := quiesceFixture(t)
			quiesceContainers(t, d, "running", "exited", 0)
			if err := os.MkdirAll(d.appDir(quiesceTestJob), 0700); err != nil {
				t.Fatal(err)
			}
			var p sdkclient.DeployPayload
			if err := json.Unmarshal(quiescePayload(quiesceTestTarget, nil), &p); err != nil {
				t.Fatal(err)
			}
			plan, err := d.quiescePlanFor(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.beginQuiesceRecord(quiesceTestJob, "cmd_job_cleanup_receipt", plan); err != nil {
				t.Fatal(err)
			}
			if orphan {
				r, err := d.loadQuiesceRecord(quiesceTestJob)
				if err != nil {
					t.Fatal(err)
				}
				r.Phase = "quiesce_orphan"
				r.Outcome = "fail_unchanged"
				if err = d.saveQuiesceRecord(*r); err != nil {
					t.Fatal(err)
				}
				d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: r.CommandID, Status: "failed"})
			}
			err = d.cleanupFinishedBackupJob(context.Background(), quiesceTestJob)
			if !orphan && err == nil {
				t.Fatal("missing durable receipt ignored")
			}
			if _, err := os.Stat(d.appDir(quiesceTestJob)); err != nil {
				t.Fatal("unsafe recovery artifacts removed")
			}
		})
	}
}
func TestBackupJobCleanupLinkedJobRetainsTarget(t *testing.T) {
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "exited", 0)
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, d.appDir(quiesceTestJob)); err != nil {
		t.Skip("symlink permission unavailable")
	}
	if err := d.cleanupFinishedBackupJob(context.Background(), quiesceTestJob); err == nil {
		t.Fatal("linked job trusted")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "outside" {
		t.Fatal("outside tree changed")
	}
}

func TestBackupJobCleanupLiteralProjectDoesNotLoseRetryConfiguration(t *testing.T) {
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	if err := os.MkdirAll(d.appDir(quiesceTestJob), 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(d.appDir(quiesceTestJob), "compose.yaml")
	if err := os.WriteFile(compose, []byte("name: legacy_backup\nservices:\n  job:\n    image: fixture:cached\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.cleanupFinishedBackupJob(context.Background(), quiesceTestJob); err == nil {
		t.Fatal("legacy project was treated as the job's label")
	}
	if _, err := os.Stat(compose); err != nil {
		t.Fatal("legacy retry configuration discarded")
	}
}

func TestBackupJobCleanupAcknowledgedGenericResultCleansOnlyFinishedJob(t *testing.T) {
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "exited", 1)
	dir := d.appDir(quiesceTestJob)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("name: "+quiesceTestJob+"\nservices:\n  job:\n    image: fixture:cached\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandLogsTail, sdkclient.CommandHealthCheck} {
		if err := d.CleanupAcknowledgedResult(t.Context(), kind, sdkclient.DeployResult{DeploymentID: quiesceTestJob, Status: "success"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatal("read-only result removed job")
		}
	}
	if err := d.CleanupAcknowledgedResult(t.Context(), sdkclient.CommandDeploy, sdkclient.DeployResult{DeploymentID: quiesceTestJob, Status: "failed", LogsTail: "captured result"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("acknowledged finished job retained", err)
	}
	if _, err := os.Stat(d.appDir(quiesceTestTarget)); err != nil {
		t.Fatal("application removed", err)
	}
}
