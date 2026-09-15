package executor

import (
	"context"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func replacementFixture(t *testing.T) (*Docker, *ReplacementWork, string) {
	d, _, bin := workFixture(t, "pull", "exit 0")
	p := sdkclient.DeployPayload{DeploymentID: "dpl_test", GitAuthMethod: "must-not-copy", GitCommitSHA: "secret-source", Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ComposeYAML: "must-not-copy"}}}
	w, err := d.createReplacementWork(&sdkclient.PollCommand{ID: "cmd_test", ControlToken: "must-not-copy"}, p, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, w, bin
}
func TestReplacementReceiptRejectsAmbiguousOutcomes(t *testing.T) {
	for _, mode := range []string{"active", "dead", "wrong-request", "wrong-command", "wrong-deployment", "nonfinal", "token", "trailing", "symlink", "success", "failed"} {
		t.Run(mode, func(t *testing.T) {
			d, w, bin := replacementFixture(t)
			dir, _, _ := d.loadReplacementWork(w)
			if mode == "dead" {
				os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho inactive\n"), 0700)
			}
			if mode != "active" && mode != "dead" {
				receipt := replacementReceipt{Version: 1, ID: w.ID, RequestSHA256: w.RequestSHA256, Result: sdkclient.DeployResult{CommandID: w.CommandID, DeploymentID: w.DeploymentID, Status: "success", LogsTail: "original result"}}
				switch mode {
				case "wrong-request":
					receipt.RequestSHA256 = strings.Repeat("0", 64)
				case "wrong-command":
					receipt.Result.CommandID = "other"
				case "wrong-deployment":
					receipt.Result.DeploymentID = "dpl_other"
				case "nonfinal":
					receipt.Result.Status = "partial"
				case "token":
					receipt.Result.ControlToken = "private"
				case "failed":
					receipt.Result.Status = "failed"
				}
				if err := writePrivateWorkJSON(dir, "result.json", receipt, replacementRecordLimit); err != nil {
					t.Fatal(err)
				}
				if mode == "trailing" {
					f, _ := os.OpenFile(filepath.Join(dir, "result.json"), os.O_APPEND|os.O_WRONLY, 0600)
					f.WriteString("{}")
					f.Close()
				}
				if mode == "symlink" {
					os.Rename(filepath.Join(dir, "result.json"), filepath.Join(dir, "outside"))
					os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, "result.json"))
				}
			}
			r, err := d.CompletedReplacementWork(w)
			if mode == "success" || mode == "failed" {
				if err != nil || r.Status != mode || r.LogsTail != "original result" {
					t.Fatalf("receipt lost: %+v %v", r, err)
				}
			} else if mode == "active" {
				if !errors.Is(err, ErrReplacementPending) {
					t.Fatal(err)
				}
			} else if err == nil || errors.Is(err, ErrReplacementPending) {
				t.Fatalf("uncertain outcome accepted: %s %v", mode, err)
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "request.json"))
			if strings.Contains(string(raw), "must-not-copy") || strings.Contains(string(raw), "secret-source") {
				t.Fatal("source or API credentials copied")
			}
		})
	}
}
func TestReplacementWorkerNeverMutatesAfterDriftOrDuplicateStart(t *testing.T) {
	for _, mode := range []string{"config", "container", "started"} {
		t.Run(mode, func(t *testing.T) {
			d, w, bin := replacementFixture(t)
			dir, _, _ := d.loadReplacementWork(w)
			invocation := filepath.Join(d.StateDir, "mutation")
			script := "#!/bin/sh\nif [ \"$1\" = ps ]; then "
			if mode == "container" {
				script += "echo " + strings.Repeat("a", 64) + "; "
			}
			script += "exit 0; fi\necho mutation > '" + invocation + "'\n"
			os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700)
			if mode == "config" {
				os.WriteFile(filepath.Join(d.appDir(w.DeploymentID), "compose.yaml"), []byte("changed"), 0600)
			}
			if mode == "started" {
				os.WriteFile(filepath.Join(dir, "started"), nil, 0600)
			}
			if err := RunReplacementWorker(d.StateDir, w.ID); err == nil {
				t.Fatal("unsafe worker start accepted")
			}
			if _, err := os.Stat(invocation); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("runtime mutated")
			}
			if _, err := os.Stat(filepath.Join(dir, "result.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("receipt fabricated")
			}
		})
	}
}
func TestReplacementCancellationRetainsWorkerAndCleanupRejectsUnknownFiles(t *testing.T) {
	d, w, _ := replacementFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := d.waitReplacementWork(ctx, &sdkclient.PollCommand{ID: w.CommandID}, w); r.Status != PreparationPendingStatus {
		t.Fatal(r)
	}
	dir, _, _ := d.loadReplacementWork(w)
	os.WriteFile(filepath.Join(dir, "unexpected"), nil, 0600)
	if err := d.ForgetReplacementWork(w); err == nil {
		t.Fatal("unknown files removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "request.json")); err != nil {
		t.Fatal("request removed before cleanup validation")
	}
	os.Remove(filepath.Join(dir, "unexpected"))
	if err := d.ForgetReplacementWork(w); err != nil {
		t.Fatal(err)
	}
}
