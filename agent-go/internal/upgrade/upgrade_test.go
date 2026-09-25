package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/packaging"
)

func TestUpdateRequestValidation(t *testing.T) {
	for _, tc := range []struct{ id, channel, version string }{
		{"cmd_0123456789abcdef", "stable", "0.6.21"},
		{"cmd_0123456789abcdef", "beta", "1.0.0"},
	} {
		if err := validate(tc.id, tc.channel, tc.version); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ id, channel, version string }{
		{"../cmd_0123456789abcdef", "stable", "0.6.21"},
		{"cmd_0123456789abcdef", "pinned", "0.6.21"},
		{"cmd_0123456789abcdef", "stable", "0.6.21;touch /tmp/unsafe"},
	} {
		if err := validate(tc.id, tc.channel, tc.version); err == nil {
			t.Fatalf("unsafe update request accepted: %+v", tc)
		}
	}
}

func TestStartRejectsChangedOrSymlinkedUpdater(t *testing.T) {
	state := t.TempDir()
	id := "cmd_0123456789abcdef"
	dir := filepath.Join(state, "upgrade-jobs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(request{Protocol: Protocol, Channel: "stable", Version: "0.6.21"})
	for name, body := range map[string][]byte{"request.json": meta, "update.sh": packaging.UpdateScript, "run.sh": []byte(wrapper)} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var started bool
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "systemd-run" {
			started = true
		}
		return nil, nil
	}
	if err := start(context.Background(), state, id, runner); err != nil || !started {
		t.Fatalf("valid staged helper did not start: %v", err)
	}
	started = false
	if err := os.WriteFile(filepath.Join(dir, "update.sh"), []byte("exit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := start(context.Background(), state, id, runner); err == nil || started {
		t.Fatal("changed helper executed")
	}
	if err := os.Remove(filepath.Join(dir, "update.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "run.sh"), filepath.Join(dir, "update.sh")); err == nil {
		if err := start(context.Background(), state, id, runner); err == nil || started {
			t.Fatal("symlinked helper executed")
		}
	}
}

func TestStartDoesNotRunWhenMetadataIsHostile(t *testing.T) {
	state := t.TempDir()
	id := "cmd_0123456789abcdef"
	dir := filepath.Join(state, "upgrade-jobs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), []byte(`{"protocol":"agent-upgrade-v1","channel":"stable","version":"0.6.21","extra":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := start(context.Background(), state, id, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("must not execute")
	})
	if err == nil || !strings.Contains(err.Error(), "invalid prepared") {
		t.Fatalf("hostile metadata passed: %v", err)
	}
}
