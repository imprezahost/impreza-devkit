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
