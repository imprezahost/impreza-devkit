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

const (
	fenceDeployment = "dpl_aaaaaaaaaaaaaaaa"
	fenceCutover    = "fov_aaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestFailoverFencePersistsOutsideApplicationAndBlocksReplay(t *testing.T) {
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f := failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 2, fenceCutover, ""}
	if err := d.writeFailoverFence(f); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.appDir(fenceDeployment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(d.appDir(fenceDeployment)); err != nil {
		t.Fatal(err)
	}
	reloaded := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := reloaded.readFailoverFence(fenceDeployment)
	if err != nil || got == nil || *got != f {
		t.Fatalf("fence did not survive application removal: %+v, %v", got, err)
	}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandDeploy, sdkclient.CommandRollback,
		sdkclient.CommandRestart, sdkclient.CommandUpdateRoutes, sdkclient.CommandTrafficSwitch,
		sdkclient.CommandOnionRotate} {
		payload, _ := json.Marshal(map[string]string{"deployment_id": fenceDeployment})
		result := reloaded.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture", Kind: kind, Payload: payload})
		if result.Status != "failed" || !strings.Contains(result.Error, "fenced") {
			t.Fatalf("%s escaped persistent fence: %+v", kind, result)
		}
	}
}

func TestFailoverFenceFailsClosedOnCorruptionAndPathEscape(t *testing.T) {
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := d.writeFailoverFence(failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 2, fenceCutover, ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.failoverFencePath("../escape"); err == nil {
		t.Fatal("path traversal accepted")
	}
	path, _ := d.failoverFencePath(fenceDeployment)
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := d.Execute(context.Background(), &sdkclient.PollCommand{
		ID: "cmd_fixture", Kind: sdkclient.CommandUpdateRoutes,
		Payload: json.RawMessage(`{"deployment_id":"` + fenceDeployment + `"}`),
	})
	if result.Status != "failed" || !strings.Contains(result.Error, "cannot be verified") {
		t.Fatalf("corrupt fence failed open: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatal("path escape wrote outside the fence directory")
	}
}

func TestFailoverFenceRejectsUnreviewedIdentity(t *testing.T) {
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandHostFailoverFence,
		Payload: json.RawMessage(`{"deployment_id":"` + fenceDeployment + `","hostname":"https://site-abcdef.imprezaapps.com/path","epoch":2,"cutover_id":"` + fenceCutover + `"}`)}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "Invalid host failover") {
		t.Fatalf("invalid fence accepted: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "failover-fences")); !os.IsNotExist(err) {
		t.Fatal("invalid fence wrote state")
	}
}

func TestTransportFencesProtectRealApplications(t *testing.T) {
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	other := "dpl_bbbbbbbbbbbbbbbb"
	if err := d.writeFailoverFence(failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 2, fenceCutover, ""}); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"bkpjob", "rstjob", "tskjob", "rdjob", "clijob", "pitrjob"} {
		id := prefix + "_aaaaaaaaaaaaaaaa"
		// Ordinary housekeeping on an unfenced application remains authorized.
		if err := d.checkCommandFences(sdkclient.CommandDeploy, id, []string{other}); err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		for _, deps := range [][]string{nil, {}, {fenceDeployment}, {other, fenceDeployment}, {"../escape"}, {id}} {
			raw, _ := json.Marshal(map[string]any{"deployment_id": id, "fence_dependencies": deps})
			result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, Payload: raw})
			if result.Status != "failed" || (!strings.Contains(result.Error, "fenced") && !strings.Contains(result.Error, "cannot be verified")) {
				t.Fatalf("%s dependencies %v escaped the real dispatcher: %+v", prefix, deps, result)
			}
		}
	}
	// A normal application cannot use metadata to hide its own tombstone.
	if err := d.checkCommandFences(sdkclient.CommandDeploy, fenceDeployment, []string{other}); err == nil {
		t.Fatal("application substituted its fence identity")
	}
	// A malformed dependency is denied even if all preceding apps are healthy.
	if err := d.checkCommandFences(sdkclient.CommandDeploy, "bkpjob_aaaaaaaaaaaaaaaa", []string{other, "dpl_bad"}); err == nil {
		t.Fatal("malformed dependency accepted")
	}
	path, _ := d.failoverFencePath(other)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.checkCommandFences(sdkclient.CommandDeploy, "bkpjob_aaaaaaaaaaaaaaaa", []string{other}); err == nil {
		t.Fatal("corrupt dependency fence accepted")
	}
}

func TestStartupFenceReconciliationRejectsCorruptLedgerBeforeDocker(t *testing.T) {
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := d.ReconcileFailoverFences(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(d.StateDir, "failover-fences")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fenceDeployment+".json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileFailoverFences(context.Background()); err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("corrupt tombstone was ignored: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, fenceDeployment+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrecognized"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileFailoverFences(context.Background()); err == nil || !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("ambiguous fence storage was ignored: %v", err)
	}
}

func TestFailoverFenceRejectsIncompleteOnionTransfer(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	recipient := strings.Repeat("A", 43) + "="
	base := `"deployment_id":"` + fenceDeployment + `","hostname":"site-abcdef.imprezaapps.com","epoch":2,"cutover_id":"` + fenceCutover + `"`
	for name, extra := range map[string]string{
		"onion without recipient": `,"onion":"` + onion + `"`,
		"recipient without onion": `,"onion_recipient":"` + recipient + `"`,
		"short recipient":         `,"onion":"` + onion + `","onion_recipient":"AAAA"`,
		"non-canonical recipient": `,"onion":"` + onion + `","onion_recipient":"` + strings.Repeat("A", 42) + "B=" + `"`,
		"invalid onion":           `,"onion":"x.onion","onion_recipient":"` + recipient + `"`,
	} {
		root := t.TempDir()
		d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
		result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture",
			Kind: sdkclient.CommandHostFailoverFence, Payload: json.RawMessage("{" + base + extra + "}")})
		if result.Status != "failed" || !strings.Contains(result.Error, "Invalid onion transfer") {
			t.Fatalf("%s: incomplete onion transfer accepted: %+v", name, result)
		}
		if _, err := os.Stat(filepath.Join(root, "failover-fences")); !os.IsNotExist(err) {
			t.Fatalf("%s: rejected fence wrote state", name)
		}
	}
}

func TestOnionTransferRecipientIsBlockedWhenFencedAndNeedsTheApplication(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	payload := json.RawMessage(`{"deployment_id":"` + fenceDeployment + `","cutover_id":"` + fenceCutover + `","onion":"` + onion + `"}`)
	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandOnionTransferRecipient, Payload: payload})
	if result.Status != "failed" || !strings.Contains(result.Error, "standby application") {
		t.Fatalf("recipient prepared without the standby application: %+v", result)
	}
	if err := d.writeFailoverFence(failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 2, fenceCutover, onion}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.appDir(fenceDeployment), 0o700); err != nil {
		t.Fatal(err)
	}
	result = d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandOnionTransferRecipient, Payload: payload})
	if result.Status != "failed" || !strings.Contains(result.Error, "fenced") {
		t.Fatalf("a fenced deployment accepted a new onion identity: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "proxy", "tor", "transfer")); !os.IsNotExist(err) {
		t.Fatal("blocked recipient wrote key material")
	}
}

func TestFailoverTombstoneWithInvalidOnionFailsClosed(t *testing.T) {
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := d.writeFailoverFence(failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 2, fenceCutover, "not-an-onion"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.readFailoverFence(fenceDeployment); err == nil {
		t.Fatal("tombstone with an invalid onion was trusted")
	}
}

func TestFailoverReleaseLiftsOnlyTheExactFence(t *testing.T) {
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	release := func(hostname string, epoch int, cutover string) sdkclient.DeployResult {
		payload, _ := json.Marshal(map[string]any{"deployment_id": fenceDeployment, "hostname": hostname, "epoch": epoch, "cutover_id": cutover})
		return d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandHostFailoverRelease, Payload: payload})
	}
	// Failures name the application: the control plane acknowledges them
	// instead of leaving the agent to resend an unmatched result.
	if result := release("https://site-abcdef.imprezaapps.com/", 2, fenceCutover); result.Status != "failed" ||
		!strings.Contains(result.Error, "Invalid host failover release") || result.DeploymentID != fenceDeployment {
		t.Fatalf("unreviewed release accepted: %+v", result)
	}
	// Nothing fenced: the end state already holds and the result says so.
	result := release("site-abcdef.imprezaapps.com", 2, fenceCutover)
	if result.Status != "success" || result.HostFailoverRelease == nil || !result.HostFailoverRelease.Released || result.HostFailoverRelease.WasFenced {
		t.Fatalf("idempotent release was not reported: %+v", result)
	}
	if err := d.writeFailoverFence(failoverFence{1, fenceDeployment, "site-abcdef.imprezaapps.com", 3, fenceCutover, ""}); err != nil {
		t.Fatal(err)
	}
	for name, attempt := range map[string]func() sdkclient.DeployResult{
		"older epoch": func() sdkclient.DeployResult { return release("site-abcdef.imprezaapps.com", 2, fenceCutover) },
		"other cutover": func() sdkclient.DeployResult {
			return release("site-abcdef.imprezaapps.com", 3, "fov_bbbbbbbbbbbbbbbbbbbbbbbb")
		},
		"other hostname": func() sdkclient.DeployResult { return release("other-abcdef.imprezaapps.com", 3, fenceCutover) },
	} {
		// The release itself is not blocked by the fence; its own check refuses.
		if result := attempt(); result.Status != "failed" || !strings.Contains(result.Error, "different failover fence") ||
			result.DeploymentID != fenceDeployment {
			t.Fatalf("%s: mismatched release accepted: %+v", name, result)
		}
		if fence, err := d.readFailoverFence(fenceDeployment); err != nil || fence == nil {
			t.Fatalf("%s: refused release removed the fence", name)
		}
	}
}
