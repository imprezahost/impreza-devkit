package executor

// Supervised build recovery:
//   - a plain build worker that is gone — its receipt without a verdict, as
//     every build over its ten-minute budget writes, or no receipt with the
//     unit stopped — reconciles as a defined failure instead of a "review
//     required" that held the whole command queue forever; only the
//     controlled builder keeps review;
//   - a Blocked deployment on a controlled-build host hands its build to
//     the worker, the only place the controlled builder runs;
//   - forgetting the worker files never leaves request.json, or a partial
//     copy of it, behind.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildWorkFixture is a finished build worker: its receipt carries no verdict.
func buildWorkFixture(t *testing.T, owned bool) (*Docker, *PreparationRecovery) {
	t.Helper()
	d, r, _ := workFixture(t, "build", "exit 0")
	dir, err := d.workDirectory(r.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		var request map[string]any
		raw, err := os.ReadFile(filepath.Join(dir, "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		// The controlled builder only runs a request bound to this boot's
		// local daemon.
		if bootID, _ := request["boot_id"].(string); bootID == "" {
			t.Fatal("the worker request carries no boot id")
		}
		request["owned_builder"] = true
		request["local_boot_recovery"] = true
		if raw, err = json.Marshal(request); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "request.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		r.Work.RequestSHA256 = workHash(raw)
	}
	receipt := preparationWorkResult{Version: 1, ID: r.Work.ID, RequestSHA256: r.Work.RequestSHA256, Completed: false, Output: "build killed at its budget"}
	if err := writeWorkJSON(dir, "result.json", receipt); err != nil {
		t.Fatal(err)
	}
	return d, r
}

func TestPlainBuildWithoutVerdictReconcilesAsDefinedFailure(t *testing.T) {
	d, r := buildWorkFixture(t, false)
	next, err := d.CompletedPreparationWork(r, r.Work.CommandID)
	if err != nil {
		t.Fatalf("a finished plain build without a verdict still asks for review (the queue freeze): %v", err)
	}
	if next.Phase != "aborted" {
		t.Fatalf("expected the defined, retryable aborted path, got %q", next.Phase)
	}
}

func TestControlledBuildWithoutVerdictKeepsReview(t *testing.T) {
	d, r := buildWorkFixture(t, true)
	if _, err := d.CompletedPreparationWork(r, r.Work.CommandID); err == nil || !strings.Contains(err.Error(), "review required") {
		t.Fatalf("the controlled builder's state is its own recovery's: %v", err)
	}
}

func TestStoppedPlainBuildWorkerWithoutReceiptReconciles(t *testing.T) {
	d, r, bin := workFixture(t, "build", "exit 0")
	unit := func(state string) {
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho "+state+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unit("inactive")
	next, err := d.CompletedPreparationWork(r, r.Work.CommandID)
	if err != nil || next.Phase != "aborted" {
		t.Fatalf("a stopped plain build worker without a receipt still asks for review: %v %+v", err, next)
	}
	unit("active")
	if _, err := d.CompletedPreparationWork(r, r.Work.CommandID); err != ErrPreparationPending {
		t.Fatalf("a live worker must stay pending: %v", err)
	}
}

func TestForgetNeverLeavesTheRequestBehind(t *testing.T) {
	d, r, _ := workFixture(t, "pull", "exit 0")
	dir, err := d.workDirectory(r.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The temporary file writeAtomic leaves when a worker dies mid-write is known.
	if err := os.WriteFile(filepath.Join(dir, ".impreza-tmp-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.ForgetPreparationWork(r.Work); err != nil {
		t.Fatalf("a leftover atomic-write temp file blocked the cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("worker directory survived")
	}

	d2, r2, _ := workFixture(t, "pull", "exit 0")
	dir2, err := d2.workDirectory(r2.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"stray": "x", ".impreza-tmp-456": `{"env":["HTTPS_PROXY=http://user:fixture@proxy"]}`} {
		if err := os.WriteFile(filepath.Join(dir2, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := d2.ForgetPreparationWork(r2.Work); err == nil {
		t.Fatal("an unexpected file must keep the directory for review")
	}
	for _, name := range []string{"request.json", ".impreza-tmp-456"} {
		if _, err := os.Stat(filepath.Join(dir2, name)); !os.IsNotExist(err) {
			t.Fatalf("%s, which can carry the proxy environment, outlived a refused cleanup", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir2, "stray")); err != nil {
		t.Fatal("the unexpected file must stay for review")
	}
	// Once the review removes the unexpected file, the cleanup finishes.
	if err := os.Remove(filepath.Join(dir2, "stray")); err != nil {
		t.Fatal(err)
	}
	if err := d2.ForgetPreparationWork(r2.Work); err != nil {
		t.Fatalf("the reviewed worker directory could not be cleaned: %v", err)
	}
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatal("the reviewed worker directory survived")
	}
}

func TestBlockedDeployControlledBuildUsesTheWorker(t *testing.T) {
	// The pull worker completes, as in TestBlockedDeployBuildStaysSynchronous.
	worker := "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/systemd-run.log\"\n" +
		"state=''; id=''\nwhile [ $# -gt 0 ]; do case \"$1\" in --state-dir) state=\"$2\"; shift;; --work-id) id=\"$2\"; shift;; esac; shift; done\n" +
		"dir=\"$state/operations/preparation-$id\"\nhash=$(sha256sum \"$dir/request.json\" | cut -d' ' -f1)\n" +
		"umask 077\nprintf '{\"version\":1,\"id\":\"%s\",\"request_sha256\":\"%s\",\"completed\":true,\"success\":true}' \"$id\" \"$hash\" > \"$dir/result.tmp\" && mv \"$dir/result.tmp\" \"$dir/result.json\"\n"
	f := newOnionPendingFixture(t, "dpl_a74b000000000001", worker, "#!/bin/sh\nif [ \"$1\" = show ]; then echo active; fi\n")
	policy, err := json.Marshal(controlledBuildPolicy{Version: 1, Enabled: true, Image: ownedBuilderImage})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.d.StateDir, "controlled-builds.json"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := f.d.Execute(ctx, f.cmd)
	docker, _ := os.ReadFile(filepath.Join(f.bin, "docker.log"))
	if composeStepLogged(string(docker), "build") {
		t.Fatalf("the root daemon built a Blocked deployment on a controlled-build host, outside the controlled builder; docker log:\n%s", docker)
	}
	// The fixture's command has no control token, which the controlled
	// builder requires: the worker path refuses it before any build runs.
	if !strings.Contains(result.Error, "controlled builds") {
		t.Fatalf("the build did not reach the controlled-build path: %s", result.Error)
	}
	f.assertCleanedUp(t, result)
}
