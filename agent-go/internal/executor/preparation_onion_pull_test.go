package executor

// An onion deploy's pull must go through the supervised worker
// even though the deployment keeps explicit reconciliation of the container
// replacement. On the base commit the onion route parked the recovery phase
// at "blocked", the gate required phase "busy", and the pull ran flat: no
// worker, no stall detection, no cancellation, and the poll queue held
// behind a 45-minute deadline — a stuck pull even blocked an onion client
// revocation. This test drives a real deploy() with a fake docker and a
// fake systemd: the pull must be delegated to a preparation worker, and
// its missing receipt must come back as the defined, retryable failure.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestOnionDeployPullIsSupervised(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux private filesystem and command execution")
	}
	defer withPullBudgetKnobs(300*time.Millisecond, 400*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	systemctl := "#!/bin/sh\n" +
		"log=\"" + bin + "/systemctl.log\"\n" +
		"echo \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"show\" ]; then\n" +
		"  if grep -q '^stop ' \"$log\" 2>/dev/null; then echo inactive; else echo inactive; fi\n" +
		"fi\n"
	for name, body := range map[string]string{"docker": "exit 0", "systemd-run": "exit 0", "systemctl": systemctl} {
		script := []byte(body)
		if !strings.HasPrefix(body, "#!") {
			script = []byte("#!/bin/sh\n" + body + "\n")
		}
		if err := os.WriteFile(filepath.Join(bin, name), script, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// createPreparationWork requires the operations directory to exist.
	if err := os.Mkdir(filepath.Join(root, "operations"), 0700); err != nil {
		t.Fatal(err)
	}

	var saved []PreparationRecovery
	d := &Docker{StateDir: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), SupervisePreparation: true}
	d.SavePreparation = func(cmd *sdkclient.PollCommand, r *PreparationRecovery) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		var snap PreparationRecovery
		if err := json.Unmarshal(raw, &snap); err != nil {
			return err
		}
		saved = append(saved, snap)
		return nil
	}

	payload := json.RawMessage(`{
		"deployment_id": "dpl_a01b0c0c0c0c0c0c",
		"manifest": {"name": "onion-app", "version": "1", "runtime": {"type": "docker-compose",
			"compose_yaml": "services:\n  web:\n    image: example.invalid/web:1\n"}, "lifecycle": {}},
		"vars": {},
		"routes": [{"target_port": 80, "upstream": "dpl_a01b0c0c0c0c0c0c-web:80", "onion": {"enabled": true}}]
	}`)
	// ControlToken stays empty on purpose: the deploy path only consults the
	// control plane when a token exists, and the pull worker never needs one.
	// (The poller additionally requires a token to journal the protocol —
	// irrelevant to this executor-level drive.)
	cmd := &sdkclient.PollCommand{ID: "cmd_onion_pull", Kind: "deploy", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Payload: payload}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := d.Execute(ctx, cmd)

	if result.Status == "success" {
		t.Fatal("deploy with an unpullable image reported success")
	}
	sawWork := false
	sawBlockedPhase := false
	for _, s := range saved {
		if s.Work != nil && s.Work.Step == "pull" {
			sawWork = true
		}
		if s.Phase == "blocked" {
			sawBlockedPhase = true
		}
	}
	if !sawWork {
		t.Fatalf("onion deploy never delegated the pull to a supervised worker; result: %+v; phases: %+v", result, saved)
	}
	if sawBlockedPhase {
		t.Fatal("onion deploy still journals the legacy blocked phase, which disables pull supervision")
	}
	if !strings.Contains(result.Error, "can be retried") {
		t.Fatalf("unsupervised or undefined pull failure for an onion deploy: %q", result.Error)
	}
}
