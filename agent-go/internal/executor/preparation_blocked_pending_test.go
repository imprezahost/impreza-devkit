package executor

// A deployment with an onion route or data_dir ("Blocked") could end
// PENDING inside the process — a failed systemd-run, an unreadable
// receipt, the agent shutting down mid-pull — and the deploy's deferred
// cleanup returns early on pending: the removal of a first deploy's onion
// service and the configuration restore were skipped, while the resume
// path never reconciles a Blocked deploy (it reports "interrupted"). The
// onion of a deployment that does not exist stayed published. Now only
// the pull goes to a worker — the build stays synchronous unless the host
// builds under the controlled builder, which only a worker runs — and a
// pull that would end pending stops the unit, confirms it inactive and
// fails, so the cleanup runs. An unconfirmed stop still fails, with a
// review note; never pending.
//
// These tests drive a real first onion deploy through Execute: a fake
// docker plays the Tor daemon (the HUP mints the key pair and hostname),
// fake systemd tools play the worker unit.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const pendingFixtureOnion = "l4cfixtureaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion"

type onionPendingFixture struct {
	d   *Docker
	bin string
	id  string
	cmd *sdkclient.PollCommand
}

// systemctlUntilStop reports the unit active until a stop was requested.
func systemctlUntilStop(bin string) string {
	return "#!/bin/sh\necho \"$*\" >> \"" + bin + "/systemctl.log\"\n" +
		"if [ \"$1\" = show ]; then\n" +
		"  if grep -q '^stop ' \"" + bin + "/systemctl.log\"; then echo inactive; else echo active; fi\n" +
		"fi\n"
}

// newOnionPendingFixture prepares a first onion-only deploy. An empty
// systemctl script means a stateful one: active until stopped.
func newOnionPendingFixture(t *testing.T, id, systemdRun, systemctl string) *onionPendingFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux private filesystem and command execution")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if systemctl == "" {
		systemctl = systemctlUntilStop(bin)
	}
	tor := filepath.Join(root, "proxy", "tor")
	docker := "#!/bin/sh\necho \"$*\" >> \"" + bin + "/docker.log\"\n" +
		"case \"$*\" in\n" +
		"  \"inspect --format {{.State.Status}} impreza_tor\") echo running; exit 0;;\n" +
		"  \"kill -s HUP impreza_tor\")\n" +
		"    svc=\"" + tor + "/services/" + id + "\"\n" +
		"    if [ -d \"$svc\" ] && [ ! -f \"$svc/hostname\" ]; then\n" +
		"      umask 077; printf fixture-secret > \"$svc/hs_ed25519_secret_key\"; printf fixture-public > \"$svc/hs_ed25519_public_key\"\n" +
		"      echo " + pendingFixtureOnion + " > \"$svc/hostname\"\n" +
		"    fi\n" +
		"    exit 0;;\n" +
		"  \"compose build\"*) echo 'build failed on purpose'; exit 1;;\n" +
		"  \"compose up\"*) echo 'up failed on purpose'; exit 1;;\n" +
		"esac\nexit 0\n"
	for name, body := range map[string]string{"docker": docker, "systemd-run": systemdRun, "systemctl": systemctl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(root, "operations"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := NewDocker(root, discardLogger())
	d.Proxy = nil
	d.SupervisePreparation = true
	d.SavePreparation = func(*sdkclient.PollCommand, *PreparationRecovery) error { return nil }
	payload, err := json.Marshal(map[string]any{
		"deployment_id": id,
		"manifest": map[string]any{"name": "onion-app", "version": "1", "lifecycle": map[string]any{},
			"runtime": map[string]any{"type": "docker-compose", "compose_yaml": "services:\n  web:\n    image: example.invalid/web:1\n"}},
		"vars":   map[string]any{},
		"routes": []any{map[string]any{"target_port": 80, "upstream": id + "-web:80", "onion": map[string]any{"enabled": true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &sdkclient.PollCommand{ID: "cmd_l4c_blocked", Kind: sdkclient.CommandDeploy, ProgressProtocol: sdkclient.DeploymentProgressProtocol, Payload: payload}
	return &onionPendingFixture{d: d, bin: bin, id: id, cmd: cmd}
}

// assertCleanedUp checks the onion cleanup and the configuration restore of a
// FIRST deploy: no service directory, no torrc entry, the key set parked,
// and the files the attempt wrote removed.
func (f *onionPendingFixture) assertCleanedUp(t *testing.T, result sdkclient.DeployResult) {
	t.Helper()
	if result.Status != "failed" {
		t.Fatalf("a Blocked deploy ended %q instead of failed: %s", result.Status, result.Error)
	}
	tor := filepath.Join(f.d.StateDir, "proxy", "tor")
	if _, err := os.Stat(filepath.Join(tor, "services", f.id)); !os.IsNotExist(err) {
		t.Fatalf("the failed first deploy kept its onion service directory: %v", err)
	}
	torrc, err := os.ReadFile(filepath.Join(tor, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(torrc), f.id) {
		t.Fatalf("the failed first deploy left its service in the torrc:\n%s", torrc)
	}
	parked, _ := filepath.Glob(filepath.Join(tor, "parked", f.id+"*"))
	if len(parked) != 1 {
		t.Fatalf("the minted key set was not parked (%d copies)", len(parked))
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if _, err := os.Stat(filepath.Join(f.d.appDir(f.id), name)); !os.IsNotExist(err) {
			t.Fatalf("the configuration of the attempt was not restored: %s remains (%v)", name, err)
		}
	}
	if !strings.Contains(result.Error, "previous containers and configuration were preserved") {
		t.Fatalf("result does not report the restore: %q", result.Error)
	}
}

func (f *onionPendingFixture) systemctlLog() string {
	log, _ := os.ReadFile(filepath.Join(f.bin, "systemctl.log"))
	return string(log)
}

func TestBlockedOnionDeployFailedWorkerLaunchCleansUp(t *testing.T) {
	f := newOnionPendingFixture(t, "dpl_a4c1a00000000001", "#!/bin/sh\necho 'Failed to start transient service unit' >&2\nexit 1\n", "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := f.d.Execute(ctx, f.cmd)
	f.assertCleanedUp(t, result)
	if !strings.Contains(result.Error, "can be retried") {
		t.Fatalf("expected the defined, retryable failure: %q", result.Error)
	}
	if !strings.Contains(f.systemctlLog(), "stop impreza-preparation-") {
		t.Fatalf("the unit was not stopped before the failure; systemctl log: %q", f.systemctlLog())
	}
}

// The agent stopping mid-pull (an update, a reboot): the waiter sees the
// deploy's context end. The pull worker unit keeps running on its own, so
// it must be stopped and confirmed inactive before the failure.
func TestBlockedOnionDeployAgentShutdownMidPullCleansUp(t *testing.T) {
	f := newOnionPendingFixture(t, "dpl_a4c1b00000000001", "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/systemd-run.log\"\nexit 0\n", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan sdkclient.DeployResult, 1)
	go func() { done <- f.d.Execute(ctx, f.cmd) }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.bin, "systemd-run.log")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pull worker was never launched")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	var result sdkclient.DeployResult
	select {
	case result = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the deploy did not return after the agent's context ended")
	}
	f.assertCleanedUp(t, result)
	if !strings.Contains(f.systemctlLog(), "stop impreza-preparation-") {
		t.Fatalf("the still-running pull unit was not stopped; systemctl log: %q", f.systemctlLog())
	}
}

// A unit that does not confirm the stop is still a failure — with a review
// note — so the cleanup runs; never pending in a Blocked deploy.
func TestBlockedOnionDeployUnconfirmedStopFailsWithReviewNote(t *testing.T) {
	orig := preparationUnitStopTimeout
	preparationUnitStopTimeout = 900 * time.Millisecond
	defer func() { preparationUnitStopTimeout = orig }()
	f := newOnionPendingFixture(t, "dpl_a4c1c00000000001", "#!/bin/sh\nexit 1\n", "#!/bin/sh\nif [ \"$1\" = show ]; then echo active; fi\n")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := f.d.Execute(ctx, f.cmd)
	f.assertCleanedUp(t, result)
	if strings.Contains(result.Error, "can be retried") || !strings.Contains(result.Error, "review") {
		t.Fatalf("an unconfirmed stop must fail with a review note, not the retryable failure: %q", result.Error)
	}
}

// Only the pull goes to a worker in a Blocked deploy: the build runs
// synchronously under the step deadline, so it can never end pending.
func TestBlockedDeployBuildStaysSynchronous(t *testing.T) {
	// This systemd-run plays a worker that completes: it writes the success
	// receipt bound to the request, the way RunPreparationWorker does.
	worker := "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/systemd-run.log\"\n" +
		"state=''; id=''\nwhile [ $# -gt 0 ]; do case \"$1\" in --state-dir) state=\"$2\"; shift;; --work-id) id=\"$2\"; shift;; esac; shift; done\n" +
		"dir=\"$state/operations/preparation-$id\"\nhash=$(sha256sum \"$dir/request.json\" | cut -d' ' -f1)\n" +
		"umask 077\nprintf '{\"version\":1,\"id\":\"%s\",\"request_sha256\":\"%s\",\"completed\":true,\"success\":true}' \"$id\" \"$hash\" > \"$dir/result.tmp\" && mv \"$dir/result.tmp\" \"$dir/result.json\"\n"
	f := newOnionPendingFixture(t, "dpl_a4c1d00000000001", worker, "#!/bin/sh\nif [ \"$1\" = show ]; then echo active; fi\n")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := f.d.Execute(ctx, f.cmd)
	launches, _ := os.ReadFile(filepath.Join(f.bin, "systemd-run.log"))
	lines := strings.Split(strings.TrimSpace(string(launches)), "\n")
	if strings.TrimSpace(string(launches)) == "" || len(lines) != 1 {
		t.Fatalf("a Blocked deploy launched %d supervised workers; only the pull may be delegated:\n%s", len(lines), launches)
	}
	docker, _ := os.ReadFile(filepath.Join(f.bin, "docker.log"))
	if !strings.Contains(string(docker), "compose build") {
		t.Fatalf("the build did not run synchronously; docker log:\n%s", docker)
	}
	f.assertCleanedUp(t, result)
}
