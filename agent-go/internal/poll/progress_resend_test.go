package poll

// Result resends and refused claims: the resend of a saved result
// asks about a closed command with the step the server already understands
// (resending_result), never reporting "interrupted" for a transient send
// error; a resend the server refuses because the command is terminal is
// accepted and released; a claim the agent refuses to repeat always ends
// with a terminal answer — released when the server already closed it, a
// terminal failed result when it has not.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func savedResultRecord(url string) *commandRecord {
	result := sdkclient.DeployResult{CommandID: "cmd_ack", ControlToken: "fixture-control", DeploymentID: "dpl_ack", Status: "success"}
	return &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: url, CommandID: result.CommandID, ControlToken: result.ControlToken,
		Kind: sdkclient.CommandDeploy, ProgressProtocol: sdkclient.DeploymentProgressProtocol, Result: &result}
}

// TestResendOfAFailedSendReportsNoInterrupted: a result whose send fails
// twice with 500 before the 204 must not report the "interrupted" step —
// the server turns that into recovery=required for a deploy that
// succeeded. The question about a closed command rides resending_result.
// Without that, the agent reports "interrupted" once per failed send.
func TestResendOfAFailedSendReportsNoInterrupted(t *testing.T) {
	var posts atomic.Int32
	var mu sync.Mutex
	var steps []string
	done, stop := context.WithCancel(context.Background())
	defer stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/command-progress":
			var body sdkclient.DeploymentProgress
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			steps = append(steps, body.Step)
			mu.Unlock()
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_ack","terminal":false}}`)
		case "/v1/agent/poll":
			stop()
			w.WriteHeader(204)
		case "/v1/agent/deploy-result":
			if posts.Add(1) > 2 {
				w.WriteHeader(204)
				return
			}
			w.WriteHeader(500)
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	record := savedResultRecord(server.URL)
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.save(record); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	lock.Close()
	ctx, cancel := context.WithTimeout(done, 30*time.Second)
	defer cancel()
	if err = p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 3 {
		t.Fatalf("expected the third send to be acknowledged, posts=%d", posts.Load())
	}
	if saved, err := p.journal.load(); err != nil || saved != nil {
		t.Fatalf("the acknowledged result was not released: %v %v", saved, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, step := range steps {
		if step == "interrupted" {
			t.Fatalf("a failed send reported the interrupted step (all steps: %v)", steps)
		}
	}
}

// TestResendAcceptsTerminalClosedCommand: when the server refuses the
// result because the command is already terminal, the agent accepts the
// closing and releases the journal instead of re-sending forever.
// Removing the terminal acceptance makes this fail.
func TestResendAcceptsTerminalClosedCommand(t *testing.T) {
	var posts atomic.Int32
	done, stop := context.WithCancel(context.Background())
	defer stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/command-progress":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_ack","terminal":true,"status":"failed"}}`)
		case "/v1/agent/poll":
			stop()
			w.WriteHeader(204)
		case "/v1/agent/deploy-result":
			posts.Add(1)
			w.WriteHeader(409)
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	record := savedResultRecord(server.URL)
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.save(record); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	lock.Close()
	ctx, cancel := context.WithTimeout(done, 25*time.Second)
	defer cancel()
	if err = p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if posts.Load() == 0 {
		t.Fatal("the saved result was never sent")
	}
	if saved, err := p.journal.load(); err != nil || saved != nil {
		t.Fatalf("a terminal refusal of the resend was not accepted: %v %v", saved, err)
	}
}

// TestRefusedClaimReleasesOnTerminalServerWithoutResult: a claim the agent
// refuses to repeat, with the server already holding it closed, releases
// the journal WITHOUT sending any result — the server closed it, there is
// nothing left to answer. A mutation that ignores the terminal answer
// sends the explicit failed result instead and fails this test.
func TestRefusedClaimReleasesOnTerminalServerWithoutResult(t *testing.T) {
	var posts atomic.Int32
	done, stop := context.WithCancel(context.Background())
	defer stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/command-progress":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_refused","terminal":true,"status":"cancelled"}}`)
		case "/v1/agent/poll":
			stop()
			w.WriteHeader(204)
		case "/v1/agent/deploy-result":
			posts.Add(1)
			w.WriteHeader(204)
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_refused", ControlToken: "fixture",
		Kind: sdkclient.CommandDeploy, ProgressProtocol: sdkclient.DeploymentProgressProtocol}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.save(record); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	lock.Close()
	ctx, cancel := context.WithTimeout(done, 15*time.Second)
	defer cancel()
	if err = p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Fatal("the agent answered a command the server had already closed")
	}
	if saved, err := p.journal.load(); err != nil || saved != nil {
		t.Fatalf("the terminal server did not release the refused claim: %v %v", saved, err)
	}
}

// refusedClaimServer answers that the command is still open and records
// the results the agent posts; the poll stops the run.
func refusedClaimServer(t *testing.T, posts *[]string, stop context.CancelFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/command-progress":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_terminal","terminal":false}}`)
		case "/v1/agent/command-control":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_terminal","phase":"preparing","cancel_requested":false}}`)
		case "/v1/agent/poll":
			w.WriteHeader(204)
		case "/v1/agent/deploy-result":
			var result sdkclient.DeployResult
			_ = json.NewDecoder(r.Body).Decode(&result)
			*posts = append(*posts, result.Status+"|"+result.Error)
			w.WriteHeader(204)
			// The response only reaches the wire when this handler returns;
			// cancelling inside it would cut the acknowledgement and the
			// agent would treat its own answer as a failed send.
			time.AfterFunc(500*time.Millisecond, stop)
		default:
			w.WriteHeader(204)
		}
	}))
}

func runRefusedResume(t *testing.T, container string) []string {
	t.Helper()
	done, stop := context.WithCancel(context.Background())
	defer stop()
	var posts []string
	server := refusedClaimServer(t, &posts, stop)
	defer server.Close()
	dir := t.TempDir()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	p.active, _ = buildRecord(t, p, dir, terminalWork{phase: "busy"})
	if err := p.journal.save(p.active); err != nil {
		t.Fatal(err)
	}
	fakeHost(t, container, false)
	ctx, cancel := context.WithTimeout(done, 90*time.Second)
	defer cancel()
	_ = p.resumeRecord(ctx)
	return posts
}

// TestRefusedClaimWithGoneWorkerAnswersTerminal: the supervised worker is
// confirmed stopped and the server still holds the claim — the refusal
// must answer with a terminal result, after restoring what the checkpoint
// can prove, instead of waiting forever. Without it no result is
// ever sent (the loop sleeps and retries until the context ends).
func TestRefusedClaimWithGoneWorkerAnswersTerminal(t *testing.T) {
	linuxOnly(t)
	posts := runRefusedResume(t, terminalContainer)
	if len(posts) != 1 {
		t.Fatalf("the refused claim must end with exactly one terminal answer, got %d", len(posts))
	}
	if !strings.HasPrefix(posts[0], "failed|") {
		t.Fatalf("the refusal answer must be a failed result: %s", posts[0])
	}
	if !strings.Contains(posts[0], "verified and configuration restored") {
		t.Fatalf("a restorable checkpoint must say so in the answer: %s", posts[0])
	}
}

// TestRefusedClaimWithGoneWorkerAnswersWhenUnverifiable: when the worker
// is gone AND the checkpoint cannot be verified against the host, the
// refusal still answers terminally — with the fixed reason that claims
// nothing unverified — instead of holding the claim open forever.
func TestRefusedClaimWithGoneWorkerAnswersWhenUnverifiable(t *testing.T) {
	linuxOnly(t)
	posts := runRefusedResume(t, strings.Repeat("b", 64))
	if len(posts) != 1 {
		t.Fatalf("the refused claim must end with exactly one terminal answer, got %d", len(posts))
	}
	if !strings.HasPrefix(posts[0], "failed|") {
		t.Fatalf("the refusal answer must be a failed result: %s", posts[0])
	}
	if !strings.Contains(posts[0], "could not be verified") {
		t.Fatalf("an unverifiable checkpoint must be answered with the fixed refusal reason: %s", posts[0])
	}
}

// The journal of a refused claim that answered terminally is gone, so the
// next command runs (checked by the poll happening in the tests above via
// stop()); this keeps the worker files from outliving the answer.
func TestRefusedClaimAnswerCleansWorkerFiles(t *testing.T) {
	linuxOnly(t)
	dir := t.TempDir()
	done, stop := context.WithCancel(context.Background())
	defer stop()
	var posts []string
	server := refusedClaimServer(t, &posts, stop)
	defer server.Close()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	record, workerDir := buildRecord(t, p, dir, terminalWork{phase: "busy"})
	p.active = record
	if err := p.journal.save(p.active); err != nil {
		t.Fatal(err)
	}
	fakeHost(t, terminalContainer, false)
	ctx, cancel := context.WithTimeout(done, 45*time.Second)
	defer cancel()
	_ = p.resumeRecord(ctx)
	if len(posts) != 1 {
		t.Fatalf("expected one terminal answer, got %d", len(posts))
	}
	if _, err := os.Stat(filepath.Join(workerDir, "request.json")); !os.IsNotExist(err) {
		t.Fatal("request.json, which can carry proxy credentials, outlived the answered claim")
	}
}
