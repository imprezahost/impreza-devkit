package executor

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func workFixture(t *testing.T, step, script string) (*Docker, *PreparationRecovery, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux private filesystem and command execution")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"docker": script, "systemd-run": "exit 0", "systemctl": "echo active"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("UNRELATED_API_SECRET", "must-not-copy")
	d := &Docker{StateDir: root}
	for _, dir := range []string{filepath.Join(root, "operations"), d.appDir("dpl_test")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.appDir("dpl_test"), "compose.yaml"), []byte("new compose"), 0600); err != nil {
		t.Fatal(err)
	}
	r := recoveryFixture()
	r.Phase = "busy"
	w, err := d.createPreparationWork(&sdkclient.PollCommand{ID: "cmd_test"}, r.DeploymentID, step)
	if err != nil {
		t.Fatal(err)
	}
	r.Work = w
	return d, r, bin
}
func TestPreparationWorkerCompletesOnceAndBindsReceipt(t *testing.T) {
	for _, step := range []string{"pull", "build"} {
		t.Run(step, func(t *testing.T) {
			d, r, _ := workFixture(t, step, `printf '%s\n' "$*" > invocation; echo diagnostic`)
			if _, err := d.CompletedPreparationWork(r, "cmd_test"); !errors.Is(err, ErrPreparationPending) {
				t.Fatal(err)
			}
			if err := RunPreparationWorker(d.StateDir, r.Work.ID); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(d.appDir(r.DeploymentID), "invocation"))
			if err != nil {
				t.Fatal(err)
			}
			want := "compose " + step + "\n"
			if step == "pull" {
				want = "compose pull --ignore-buildable\n"
			}
			if string(got) != want {
				t.Fatalf("unexpected command: %q", got)
			}
			ready, err := d.CompletedPreparationWork(r, "cmd_test")
			if err != nil || !ready.Recoverable() || r.Phase != "busy" {
				t.Fatalf("%+v %v", ready, err)
			}
			if _, err = d.CompletedPreparationWork(r, "another-command"); err == nil {
				t.Fatal("wrong operation accepted")
			}
			if err = RunPreparationWorker(d.StateDir, r.Work.ID); err == nil {
				t.Fatal("worker replayed")
			}
			dir, _, _ := d.loadPreparationWork(r.Work, r.DeploymentID)
			raw, _ := os.ReadFile(filepath.Join(dir, "request.json"))
			if strings.Contains(string(raw), "must-not-copy") {
				t.Fatal("unrelated environment copied")
			}
			var receipt preparationWorkResult
			readWorkJSON(filepath.Join(dir, "result.json"), &receipt)
			receipt.RequestSHA256 = strings.Repeat("0", 64)
			writeWorkJSON(dir, "result.json", receipt)
			if _, err = d.CompletedPreparationWork(r, "cmd_test"); err == nil {
				t.Fatal("wrong receipt accepted")
			}
		})
	}
}
func TestPreparationWorkerRejectsChangedInputsAndMissingProof(t *testing.T) {
	d, r, bin := workFixture(t, "build", "echo should-not-run > invocation")
	os.WriteFile(filepath.Join(d.appDir(r.DeploymentID), "compose.yaml"), []byte("changed"), 0600)
	if err := RunPreparationWorker(d.StateDir, r.Work.ID); err == nil {
		t.Fatal("changed inputs accepted")
	}
	if _, err := os.Stat(filepath.Join(d.appDir(r.DeploymentID), "invocation")); !os.IsNotExist(err) {
		t.Fatal("work ran despite input drift")
	}
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0700)
	if _, err := d.CompletedPreparationWork(r, "cmd_test"); err == nil || errors.Is(err, ErrPreparationPending) {
		t.Fatal("missing worker did not require review", err)
	}
	// A result for another deployment cannot be accepted even if its digest is valid.
	dir, _, _ := d.loadPreparationWork(r.Work, r.DeploymentID)
	var request preparationWorkRequest
	readWorkJSON(filepath.Join(dir, "request.json"), &request)
	request.DeploymentID = "dpl_other"
	writeWorkJSON(dir, "request.json", request)
	raw, _ := json.Marshal(request)
	r.Work.RequestSHA256 = workHash(raw)
	if _, err := d.CompletedPreparationWork(r, "cmd_test"); err == nil {
		t.Fatal("different deployment accepted")
	}
}
func TestPreparationWorkerFailureAndSignalDoNotAuthorizeRecovery(t *testing.T) {
	for _, script := range []string{"echo failure; exit 1", "kill -KILL $$"} {
		t.Run(script, func(t *testing.T) {
			d, r, _ := workFixture(t, "build", script)
			if err := RunPreparationWorker(d.StateDir, r.Work.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.CompletedPreparationWork(r, "cmd_test"); err == nil || errors.Is(err, ErrPreparationPending) {
				t.Fatal("unsuccessful work authorized restoration", err)
			}
			result, err := d.preparationWorkResult(r.Work, r.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(script, "KILL") && result.Completed {
				t.Fatal("signal classified as normal exit")
			}
		})
	}
}
func TestPreparationWorkerStateCannotEscapeOrClaimReplacement(t *testing.T) {
	for _, phase := range []string{"unstarted", "replacing", "blocked"} {
		r := recoveryFixture()
		r.Phase = phase
		r.Work = &PreparationWork{ID: strings.Repeat("a", 32), Step: "build", CommandID: "cmd_test", RequestSHA256: strings.Repeat("b", 64)}
		if r.Validate() == nil {
			t.Fatal("worker accepted outside supported phase", phase)
		}
	}
	for _, step := range []string{"up", "down", "build; evil"} {
		w := &PreparationWork{ID: strings.Repeat("a", 32), Step: step, CommandID: "cmd_test", RequestSHA256: strings.Repeat("b", 64)}
		if w.validate() == nil {
			t.Fatal("unsafe command", step)
		}
	}
	d := &Docker{StateDir: t.TempDir()}
	if _, err := d.workDirectory("../../other"); err == nil {
		t.Fatal("path traversal accepted")
	}
}
func TestPreparationWorkerProtectsPrivateFilesAndCleanup(t *testing.T) {
	d, r, _ := workFixture(t, "pull", "exit 0")
	dir, _, _ := d.loadPreparationWork(r.Work, r.DeploymentID)
	os.WriteFile(filepath.Join(dir, "unexpected"), []byte("preserve"), 0600)
	if err := d.ForgetPreparationWork(r.Work); err == nil {
		t.Fatal("unexpected file removed")
	}
	os.Remove(filepath.Join(dir, "unexpected"))
	if err := RunPreparationWorker(d.StateDir, r.Work.ID); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "result.json"))
	os.Symlink(filepath.Join(dir, "request.json"), filepath.Join(dir, "result.json"))
	if _, err := d.CompletedPreparationWork(r, "cmd_test"); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := d.ForgetPreparationWork(r.Work); err == nil {
		t.Fatal("cleanup followed symlink")
	}
	os.Remove(filepath.Join(dir, "result.json"))
	if err := d.ForgetPreparationWork(r.Work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("completed private worker files remain")
	}
}
func TestPreparationWaitCancellationNeverMeansCompletion(t *testing.T) {
	d, r, _ := workFixture(t, "build", "exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID); !errors.Is(err, ErrPreparationPending) {
		t.Fatal(err)
	}
	if preparationResult("cmd_test", ErrPreparationPending).Status != PreparationPendingStatus {
		t.Fatal("pending result could escape as terminal")
	}
}
func TestPreparationOutputIsBounded(t *testing.T) {
	b := &preparationTail{limit: 4}
	if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatal(n, err)
	}
	b.Write([]byte("gh"))
	if string(b.data) != "efgh" {
		t.Fatal(string(b.data))
	}
}
