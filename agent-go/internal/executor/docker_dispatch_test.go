package executor

import (
	"context"
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerRejectsUnsupportedCommands(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "keep")
	if err := os.WriteFile(marker, []byte("application state"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Docker{StateDir: root}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandUpdate, "future_operation", "", "unknown\noperation"} {
		t.Run(string(kind), func(t *testing.T) {
			cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: kind, Payload: json.RawMessage(`{"secret":"must-not-appear"}`)}
			result := d.Execute(context.Background(), cmd)
			if result.CommandID != cmd.ID || result.Status != "failed" || !strings.Contains(result.Error, "Unsupported command") {
				t.Fatalf("unexpected result: %+v", result)
			}
			if strings.Contains(result.Error, "\n") || strings.Contains(result.Error, "must-not-appear") || result.LogsTail != "" {
				t.Fatalf("unsafe diagnostic: %+v", result)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "application state" {
				t.Fatal("modified application state")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatal("created unexpected state")
			}
		})
	}
}

func TestDockerRejectsMalformedAgentUpgradeBeforeStaging(t *testing.T) {
	root := t.TempDir()
	d := &Docker{StateDir: root}
	result := d.Execute(context.Background(), &sdkclient.PollCommand{
		ID: "cmd_0123456789abcdef", Kind: sdkclient.CommandAgentUpgrade,
		Payload: json.RawMessage(`{"protocol":"wrong","channel":"stable","target_version":"0.6.21"}`),
	})
	if result.Status != "failed" || !strings.Contains(result.Error, "Invalid agent update protocol") {
		t.Fatalf("malformed upgrade was accepted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "upgrade-jobs")); !os.IsNotExist(err) {
		t.Fatal("malformed upgrade staged a helper")
	}
}

func TestDockerSupportedCommandsStillDispatch(t *testing.T) {
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandDeploy, sdkclient.CommandRollback, sdkclient.CommandUninstall, sdkclient.CommandRestart, sdkclient.CommandHealthCheck, sdkclient.CommandLogsTail, sdkclient.CommandUpdateRoutes} {
		t.Run(string(kind), func(t *testing.T) {
			result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_supported", Kind: kind, Payload: json.RawMessage(`invalid json`)})
			if result.Status != "failed" || !strings.Contains(result.Error, "decode") || strings.Contains(result.Error, "Unsupported command") {
				t.Fatalf("handler was not reached: %+v", result)
			}
		})
	}
}
