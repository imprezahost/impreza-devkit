package poll

// The resume loop of a supervised preparation slept and started over
// whenever the worker's receipt could not be reconciled — before it ever
// read the server's terminal answer — so a build killed at its budget, or
// an aborted checkpoint of a command the server had already closed, held
// every later command on the host forever. Once nothing of the work can
// still change images, a terminal server releases the journal after the
// previous configuration is restored and verified. A live worker, a
// controlled build (its builder container can outlive the unit), or a
// restore that cannot be verified still holds it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var x74Container = strings.Repeat("a", 64)

// terminalServer always answers that the command is closed.
func terminalServer(t *testing.T, terminals *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/command-progress":
			terminals.Add(1)
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_x74","terminal":true,"status":"failed"}}`)
		case "/v1/agent/command-control":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_x74","phase":"preparing","cancel_requested":false}}`)
		default:
			w.WriteHeader(204)
		}
	}))
}

// fakeHost puts on PATH a docker that lists one container for the app and a
// systemctl that reports the worker's unit active or stopped.
func fakeHost(t *testing.T, container string, alive bool) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	state := "inactive"
	if alive {
		state = "active"
	}
	for name, script := range map[string]string{
		"docker":    "#!/bin/sh\nif [ \"$1\" = \"ps\" ]; then echo " + container + "; fi\nexit 0\n",
		"systemctl": "#!/bin/sh\nif [ \"$1\" = \"show\" ]; then echo " + state + "; fi\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// x74Work is the journal of a supervised build the tests resume.
type x74Work struct {
	phase   string // busy or aborted
	owned   bool   // the request names the controlled builder
	invalid bool   // the receipt is bound to another request
}

// buildRecord writes a finished build worker (its receipt without a verdict,
// as the worker's own budget leaves it), the app's configuration of the
// failed deployment, and the journal record.
func buildRecord(t *testing.T, p *Poller, dir string, w x74Work) (*commandRecord, string) {
	t.Helper()
	workID := strings.Repeat("f", 32)
	workerDir := filepath.Join(p.journal.dir, "preparation-"+workID)
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"version": 1, "id": workID, "command_id": "cmd_x74", "deployment_id": "dpl_x74",
		"state_dir": dir, "step": "build", "owned_builder": w.owned})
	sum := sha256.Sum256(request)
	hash := hex.EncodeToString(sum[:])
	bound := hash
	if w.invalid {
		bound = strings.Repeat("0", 64)
	}
	receipt, _ := json.Marshal(map[string]any{"version": 1, "id": workID, "request_sha256": bound, "completed": false, "output": "killed at its budget"})
	for name, raw := range map[string][]byte{"request.json": request, "started": nil, "result.json": receipt} {
		if err := os.WriteFile(filepath.Join(workerDir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := filepath.Join(dir, "apps", "dpl_x74")
	if err := os.MkdirAll(app, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: p.cfg.ControlPlaneURL, CommandID: "cmd_x74", ControlToken: "fixture",
		ProgressProtocol: sdkclient.DeploymentProgressProtocol,
		Preparation: &executor.PreparationRecovery{Version: 1, Phase: w.phase, DeploymentID: "dpl_x74",
			Files:      []executor.PreparationFile{{Name: "compose.yaml", Data: []byte("old"), Mode: 0o600, Exists: true}, {Name: ".env"}, {Name: "startup.json"}},
			Containers: []string{x74Container},
			Work:       &executor.PreparationWork{ID: workID, Step: "build", CommandID: "cmd_x74", RequestSHA256: hash}}}
	return record, workerDir
}

type resumeOutcome struct {
	released  bool
	terminals int32
	workerDir string
	config    string
}

func runResume(t *testing.T, w x74Work, window time.Duration) resumeOutcome {
	t.Helper()
	dir := t.TempDir()
	var count atomic.Int32
	server := terminalServer(t, &count)
	defer server.Close()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	record, workerDir := buildRecord(t, p, dir, w)
	p.active = record
	if err := p.journal.save(p.active); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	_ = p.resumeRecord(ctx)
	saved, err := p.journal.load()
	config, _ := os.ReadFile(filepath.Join(dir, "apps", "dpl_x74", "compose.yaml"))
	return resumeOutcome{released: err == nil && saved == nil && p.active == nil, terminals: count.Load(), workerDir: workerDir, config: string(config)}
}

func linuxOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux worker files and shell command doubles")
	}
}

func assertReleased(t *testing.T, out resumeOutcome, what string) {
	t.Helper()
	if !out.released {
		t.Fatalf("%s was held after %d terminal answers: every later command on the host waits forever", what, out.terminals)
	}
	if out.config != "old" {
		t.Fatalf("%s was released without restoring the previous configuration (compose.yaml is %q)", what, out.config)
	}
	if _, err := os.Stat(filepath.Join(out.workerDir, "request.json")); !os.IsNotExist(err) {
		t.Fatalf("%s: request.json, which can carry proxy credentials, outlived the released journal", what)
	}
}

func assertHeld(t *testing.T, out resumeOutcome, what string) {
	t.Helper()
	if out.released || out.config != "new" {
		t.Fatalf("%s was released or changed (released=%v, compose.yaml %q)", what, out.released, out.config)
	}
	if out.terminals == 0 {
		t.Fatal("the loop never reported its state")
	}
}

func TestTerminalServerReleasesAPlainBuildWithoutVerdict(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, x74Container, false)
	assertReleased(t, runResume(t, x74Work{phase: "busy"}, 20*time.Second), "a closed command's build killed at its budget")
}

func TestTerminalServerReleasesAPlainBuildWithAnInvalidReceipt(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, x74Container, false)
	assertReleased(t, runResume(t, x74Work{phase: "busy", invalid: true}, 20*time.Second),
		"a closed command's stopped build with a receipt that cannot be reconciled")
}

func TestTerminalServerReleasesAnAbortedCheckpoint(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, x74Container, false)
	assertReleased(t, runResume(t, x74Work{phase: "aborted"}, 20*time.Second), "a closed command's aborted checkpoint")
}

func TestTerminalServerKeepsAControlledBuildInReview(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, x74Container, false)
	// The builder container can outlive a stopped unit; the controlled
	// builder's own recovery owns it (a request the agent cannot bind counts
	// as a controlled build too).
	assertHeld(t, runResume(t, x74Work{phase: "busy", owned: true}, 3*time.Second), "a controlled build in review")
}

func TestTerminalServerKeepsAnUnverifiableCheckpoint(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, strings.Repeat("b", 64), false) // the containers changed since the preparation
	assertHeld(t, runResume(t, x74Work{phase: "aborted"}, 3*time.Second), "a checkpoint whose restore cannot be verified")
}

func TestLiveWorkerStillHoldsTheJournal(t *testing.T) {
	linuxOnly(t)
	fakeHost(t, x74Container, true)
	assertHeld(t, runResume(t, x74Work{phase: "busy", invalid: true}, 3*time.Second), "a build whose worker may still be changing images")
}
