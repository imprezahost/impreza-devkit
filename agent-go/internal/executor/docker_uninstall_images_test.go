package executor

// The uninstall teardown keeps reclaiming images by default and can
// opt out per command (keep_images).
//
// keep_images maps to `--rmi local`, not to dropping --rmi
// entirely — an image built locally from the customer's source carries
// their code and must never outlive the app, while the pulled registry
// cache is exactly what a retry wants to keep. And keep_images combined
// with purge_data is refused: a purge promises the app's data AND code
// leave the host.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// The teardown command keeps reclaiming images by default and can
// opt out per command.
func TestUninstallDownArgsImagePolicy(t *testing.T) {
	if got := uninstallDownArgs(false, false); !reflect.DeepEqual(got, []string{"down", "--remove-orphans", "--rmi", "all"}) {
		t.Fatalf("default must remove images: %v", got)
	}
	if got := uninstallDownArgs(true, false); !reflect.DeepEqual(got, []string{"down", "--remove-orphans", "--rmi", "all", "--volumes"}) {
		t.Fatalf("purge must also remove volumes: %v", got)
	}
	if got := uninstallDownArgs(false, true); !reflect.DeepEqual(got, []string{"down", "--remove-orphans", "--rmi", "local"}) {
		t.Fatalf("keep_images must spare the registry cache but still remove locally built images: %v", got)
	}
	if got := uninstallDownArgs(true, true); !reflect.DeepEqual(got, []string{"down", "--remove-orphans", "--rmi", "local", "--volumes"}) {
		t.Fatalf("keep_images+purge keeps --rmi local and --volumes (the combination itself is refused upstream): %v", got)
	}
}

// keep_images with purge_data is refused before anything runs: purging
// data while retaining locally built images would keep the customer's
// code on the host forever.
func TestUninstallRefusesKeepImagesWithPurge(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"deployment_id": "dpl_keeppurge",
		"purge_data":    true,
		"keep_images":   true,
	})
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result := d.uninstall(context.Background(), &sdkclient.PollCommand{ID: "cmd_keep_purge", Payload: payload})
	if result.Status != "failed" {
		t.Fatalf("keep_images+purge_data was accepted: %+v", result)
	}
	if !strings.Contains(result.Error, "keep_images") || !strings.Contains(result.Error, "purge_data") {
		t.Fatalf("refusal must name the rejected combination: %q", result.Error)
	}
	// Refused BEFORE touching anything: no app directory was created.
	if _, err := os.Stat(filepath.Join(d.StateDir, "apps", "dpl_keeppurge")); !os.IsNotExist(err) {
		t.Fatal("the refused uninstall touched the app directory")
	}
}
