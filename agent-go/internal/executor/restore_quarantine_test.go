package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// quarantineFixture puts the fixture in a quarantined state, through the real
// startup path: a restore journal that cannot be verified, whose terminal
// result names quiesceTestTarget (or names nothing, for the global case).
func quarantineFixture(t *testing.T, named bool) *Docker {
	t.Helper()
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if named {
		d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_quarantine", Status: "failed",
			DeploymentID: quiesceTestJob, RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: quiesceTestTarget}})
	}
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("a corrupt journal took the agent down: %v", err)
	}
	return d
}

func quarantineCmd(t *testing.T, kind sdkclient.CommandKind, payload map[string]any) *sdkclient.PollCommand {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &sdkclient.PollCommand{ID: "cmd_quarantine_" + string(kind), Kind: kind, Payload: raw}
}

const quarantineOther = "dpl_bbbbbbbbbbbbbbbb"

// The commands that change a quarantined application's identity, or
// put visitors or a failover onto it, are refused with the reason; the
// commands that only read, stop, fence or move traffic away still run.
func TestQuarantinedApplicationRefusesIdentityAndInboundMoves(t *testing.T) {
	d := quarantineFixture(t, true)
	onion := strings.Repeat("a", 56) + ".onion"
	refused := map[string]*sdkclient.PollCommand{
		"onion_rotate": quarantineCmd(t, sdkclient.CommandOnionRotate, map[string]any{"deployment_id": quiesceTestTarget, "expected_onion": onion}),
		"onion_purge":  quarantineCmd(t, sdkclient.CommandOnionPurge, map[string]any{"deployment_id": quiesceTestTarget, "onion": onion}),
		"onion_transfer_recipient": quarantineCmd(t, sdkclient.CommandOnionTransferRecipient, map[string]any{
			"deployment_id": quiesceTestTarget, "cutover_id": "cut_0123456789abcdef", "onion": onion}),
		"traffic_switch onto the quarantined target": quarantineCmd(t, sdkclient.CommandTrafficSwitch, map[string]any{
			"deployment_id": quarantineOther, "target_deployment_id": quiesceTestTarget, "hostname": "shop.example.test"}),
	}
	for name, cmd := range refused {
		result := d.Execute(context.Background(), cmd)
		if result.Status != "failed" || !strings.Contains(result.Error, "cannot be verified") {
			t.Errorf("%s was not refused with the quarantine reason: %+v", name, result)
		}
	}
	allowed := map[string]*sdkclient.PollCommand{
		"onion_key_export": quarantineCmd(t, sdkclient.CommandOnionKeyExport, map[string]any{"deployment_id": quiesceTestTarget, "recipient_pubkey": "AAAA"}),
		"host_failover_fence": quarantineCmd(t, sdkclient.CommandHostFailoverFence, map[string]any{
			"deployment_id": quiesceTestTarget, "hostname": "shop.example.test", "epoch": 2, "cutover_id": "cut_0123456789abcdef"}),
		"host_failover_release": quarantineCmd(t, sdkclient.CommandHostFailoverRelease, map[string]any{
			"deployment_id": quiesceTestTarget, "hostname": "shop.example.test", "epoch": 2, "cutover_id": "cut_0123456789abcdef"}),
		"health_check": quarantineCmd(t, sdkclient.CommandHealthCheck, map[string]any{"deployment_id": quiesceTestTarget}),
		"traffic_switch off the quarantined source": quarantineCmd(t, sdkclient.CommandTrafficSwitch, map[string]any{
			"deployment_id": quiesceTestTarget, "target_deployment_id": quarantineOther, "hostname": "shop.example.test"}),
		"onion_rotate of another application": quarantineCmd(t, sdkclient.CommandOnionRotate, map[string]any{"deployment_id": quarantineOther}),
	}
	for name, cmd := range allowed {
		// They fail here for their own reasons (no Tor, no proxy, a bare
		// payload), never for the quarantine.
		result := d.Execute(context.Background(), cmd)
		if strings.Contains(result.Error, "cannot be verified") || strings.Contains(result.Error, "names no application") {
			t.Errorf("%s was refused by the quarantine: %+v", name, result)
		}
	}
}

// With no readable target every command outside the allowed set is
// refused for every application, traffic switches included (any target
// could be the one mid-exchange); the protective ones still run.
func TestGlobalQuarantineRefusesForEveryApplication(t *testing.T) {
	d := quarantineFixture(t, false)
	for _, app := range []string{quiesceTestTarget, quarantineOther} {
		for _, cmd := range []*sdkclient.PollCommand{
			quarantineCmd(t, sdkclient.CommandOnionRotate, map[string]any{"deployment_id": app}),
			quarantineCmd(t, sdkclient.CommandOnionPurge, map[string]any{"deployment_id": app}),
			quarantineCmd(t, sdkclient.CommandOnionTransferRecipient, map[string]any{"deployment_id": app}),
			quarantineCmd(t, sdkclient.CommandTrafficSwitch, map[string]any{"deployment_id": app, "target_deployment_id": "dpl_cccccccccccccccc"}),
		} {
			result := d.Execute(context.Background(), cmd)
			if result.Status != "failed" || !strings.Contains(result.Error, "names no application") {
				t.Errorf("%s on %s was not refused globally: %+v", cmd.Kind, app, result)
			}
		}
		for _, cmd := range []*sdkclient.PollCommand{
			quarantineCmd(t, sdkclient.CommandOnionKeyExport, map[string]any{"deployment_id": app}),
			quarantineCmd(t, sdkclient.CommandHostFailoverFence, map[string]any{"deployment_id": app}),
		} {
			if result := d.Execute(context.Background(), cmd); strings.Contains(result.Error, "names no application") {
				t.Errorf("%s on %s was refused by the global quarantine: %+v", cmd.Kind, app, result)
			}
		}
	}
}

// The heartbeat view: what is quarantined reaches the control plane on its
// own, bounded and ordered; nothing quarantined sends nothing.
func TestRestoreQuarantineReport(t *testing.T) {
	d := quarantineFixture(t, true)
	got := d.CollectRestoreQuarantine()
	if len(got) != 1 || got[0].DeploymentID != quiesceTestTarget || got[0].Scope != "application" ||
		!strings.Contains(got[0].Reason, quiesceTestJob) {
		t.Fatalf("the quarantined application is not reported: %+v", got)
	}
	global := quarantineFixture(t, false).CollectRestoreQuarantine()
	if len(global) != 1 || global[0].DeploymentID != "" || global[0].Scope != "all" {
		t.Fatalf("the global quarantine is not reported as scope all: %+v", global)
	}
	if clean := quiesceFixture(t).CollectRestoreQuarantine(); clean != nil {
		t.Fatalf("a clean agent reports a quarantine: %+v", clean)
	}
	many := &Docker{quiescePoisoned: map[string]string{}}
	for i := 0; i < 100; i++ {
		many.quiescePoisoned[strings.Repeat("x", 3)+string(rune('a'+i%26))+strings.Repeat("y", i)] = strings.Repeat("é", 400)
	}
	reports := many.CollectRestoreQuarantine()
	if len(reports) != maxQuarantineReports {
		t.Fatalf("the report is not bounded: %d entries", len(reports))
	}
	for _, r := range reports {
		if len(r.Reason) > 512 || !json.Valid([]byte(`"`+r.Reason+`"`)) || strings.ContainsRune(r.Reason, '�') {
			t.Fatalf("a reason was not cut to a valid bounded string: %d bytes", len(r.Reason))
		}
	}
	raw, _ := json.Marshal(sdkclient.AgentReport{RestoreQuarantine: got})
	if !strings.Contains(string(raw), `"restore_quarantine":[{"deployment_id":"`+quiesceTestTarget+`","scope":"application"`) {
		t.Fatalf("the wire shape changed: %s", raw)
	}
	if raw, _ := json.Marshal(sdkclient.AgentReport{}); strings.Contains(string(raw), "restore_quarantine") {
		t.Fatalf("an empty quarantine is on the wire: %s", raw)
	}
}
