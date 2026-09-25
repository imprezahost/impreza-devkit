package executor

import (
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDomainHandoverEntryRefusesMissingAuthority(t *testing.T) {
	for _, variant := range []string{"token", "protocol", "proxy", "hostname", "url", "provision"} {
		t.Run(variant, func(t *testing.T) {
			d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			id := "dpl_" + strings.Repeat("a", 16)
			app := d.appDir(id)
			if err := os.MkdirAll(app, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			env := filepath.Join(app, ".env")
			if err := os.WriteFile(env, []byte("KEEP=previous\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p := sdkclient.UpdateRoutesPayload{DeploymentID: id, Vars: map[string]any{"DOMAIN_URL": "https://after.example.test"},
				Routes:         []sdkclient.Route{{Hostname: "after.example.test", Upstream: id + "-app:80"}},
				DomainHandover: &sdkclient.DomainHandover{Protocol: sdkclient.DomainHandoverProtocol, Before: "before.example.test", After: "after.example.test"}}
			cmd := &sdkclient.PollCommand{ID: "cmd_" + strings.Repeat("a", 16), Kind: sdkclient.CommandUpdateRoutes, ControlToken: "fixture", ProgressProtocol: sdkclient.DeploymentProgressProtocol}
			switch variant {
			case "token":
				cmd.ControlToken = ""
			case "protocol":
				p.DomainHandover.Protocol = "legacy"
			case "proxy":
				d.Proxy = nil
			case "hostname":
				p.DomainHandover.After = "bad\n.example.test"
			case "url":
				p.Vars["DOMAIN_URL"] = "https://other.example.test"
			case "provision":
				p.ProvisionOnion = true
			}
			raw, _ := json.Marshal(p)
			cmd.Payload = raw
			if result := d.Execute(t.Context(), cmd); result.Status != "failed" {
				t.Fatal("invalid handover accepted")
			}
			got, _ := os.ReadFile(env)
			if string(got) != "KEEP=previous\n" {
				t.Fatal("invalid handover rewrote environment")
			}
			if _, err := os.Stat(filepath.Join(d.StateDir, "proxy", "routing-switch.json")); !os.IsNotExist(err) {
				t.Fatal("invalid handover wrote a routing journal")
			}
		})
	}
}
func TestDomainHandoverReplacementRequiresLiveControl(t *testing.T) {
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := "dpl_" + strings.Repeat("a", 16)
	command := "cmd_" + strings.Repeat("b", 16)
	app := d.appDir(id)
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(app, ".env")
	if err := os.WriteFile(env, []byte("KEEP=previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	phases := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		phase, _ := body["phase"].(string)
		phases = append(phases, phase)
		w.Header().Set("Content-Type", "application/json")
		if phase == "replacing" {
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"success":false,"error":{"code":"CONFLICT","message":"fixture refusal"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"command_id": command, "phase": phase, "cancel_requested": false}})
	}))
	defer srv.Close()
	d.Client = &sdkclient.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	p := sdkclient.UpdateRoutesPayload{DeploymentID: id, Vars: map[string]any{"DOMAIN_URL": "https://after.example.test"}, Routes: []sdkclient.Route{{Hostname: "after.example.test", Upstream: id + "-app:80"}}, DomainHandover: &sdkclient.DomainHandover{Protocol: sdkclient.DomainHandoverProtocol, Before: "before.example.test", After: "after.example.test"}}
	cmd := &sdkclient.PollCommand{ID: command, Kind: sdkclient.CommandUpdateRoutes, ControlToken: "fixture", ProgressProtocol: sdkclient.DeploymentProgressProtocol}
	cmd.Payload, _ = json.Marshal(p)
	r := d.Execute(t.Context(), cmd)
	if r.Status != "failed" || r.DomainHandover == nil || r.DomainHandover.Status != "unchanged" {
		t.Fatalf("replacement refusal lost safe outcome: %#v", r)
	}
	if strings.Join(phases, ",") != "preparing,replacing" {
		t.Fatalf("unexpected checkpoint flow: %v", phases)
	}
	got, _ := os.ReadFile(env)
	if string(got) != "KEEP=previous\n" {
		t.Fatal("unapproved replacement rewrote environment")
	}
}

func TestDomainHandoverRecoveryIdentityAndSavedReceipt(t *testing.T) {
	d := NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := DomainHandoverIdentity{DeploymentID: "dpl_" + strings.Repeat("a", 16), Before: "before.example.test", After: "after.example.test"}
	command := "cmd_" + strings.Repeat("a", 16)
	r, err := d.RecoverDomainHandover(t.Context(), command, &id)
	if err != nil || r.DomainHandover.Status != "unchanged" {
		t.Fatal("unstarted operation should leave old state untouched", err)
	}
	result := sdkclient.DeployResult{CommandID: command, DeploymentID: id.DeploymentID, Status: "success", Domain: "https://" + id.After,
		DomainHandover: &sdkclient.DomainHandoverResult{Before: id.Before, After: id.After, Status: "switched"}}
	record := &domainHandoverRecord{Version: 1, Command: command, Identity: id, Environment: []byte("KEEP=old-secret\n"), Result: &result}
	if err := d.saveDomainHandover(record); err != nil {
		t.Fatal(err)
	}
	wrong := id
	wrong.DeploymentID = "dpl_" + strings.Repeat("b", 16)
	if _, err := d.RecoverDomainHandover(t.Context(), command, &wrong); err == nil {
		t.Fatal("another app consumed a saved domain receipt")
	}
	r, err = d.RecoverDomainHandover(t.Context(), command, &id)
	if err != nil || r.Status != "success" || r.DomainHandover.Status != "switched" {
		t.Fatal("verified completion was not recovered", err)
	}
	if err := d.ForgetDomainHandover(command); err != nil {
		t.Fatal(err)
	}
	path, _ := d.handoverPath(command)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("acknowledged domain recovery material remains")
	}
	if _, err := d.RecoverDomainHandover(t.Context(), "../escape", &id); err == nil {
		t.Fatal("unsafe command accepted")
	}
}
