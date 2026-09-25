package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestUninstallMissingAppStillRequiresOnionRecovery(t *testing.T) {
	// No external daemon is contacted by this regression.
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Proxy = nil
	if err := os.MkdirAll(d.Tor.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Tor.StateDir, "policy-change.json"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(sdkclient.UninstallPayload{DeploymentID: "dpl_uninstall_retry", PurgeData: true})
	if err != nil {
		t.Fatal(err)
	}
	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_uninstall_retry", Kind: sdkclient.CommandUninstall, Payload: payload})
	if result.Status != "failed" || !strings.Contains(result.Error, "onion removal must be retried") {
		t.Fatalf("incomplete onion cleanup reported as successful: %s", result.Status)
	}
	if _, err := os.Stat(filepath.Join(d.Tor.StateDir, "policy-change.json")); err != nil {
		t.Fatal("uninstall discarded the pending recovery record", err)
	}
}

func TestInternalJobCleanupDoesNotTouchOnionIdentity(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Proxy = nil
	if err := os.MkdirAll(d.Tor.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(d.Tor.StateDir, "policy-change.json")
	if err := os.WriteFile(journal, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"bkpjob", "rstjob", "tskjob", "rdjob", "clijob", "pitrjob"} {
		if err := d.removeDeploymentExposure(context.Background(), prefix+"_"+strings.Repeat("a", 16)); err != nil {
			t.Fatalf("internal %s cleanup contacted Tor: %v", prefix, err)
		}
	}
	for _, id := range []string{"bkpjob_a", "bkpjob_" + strings.Repeat("a", 16) + "/other", "dpl_" + strings.Repeat("a", 16)} {
		if err := d.removeDeploymentExposure(context.Background(), id); err == nil {
			t.Fatalf("invalid ID or real app bypassed onion recovery: %s", id)
		}
	}
	if raw, err := os.ReadFile(journal); err != nil || string(raw) != "pending" {
		t.Fatal("internal cleanup changed onion recovery state", err)
	}
}
