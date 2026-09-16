package executor

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedWorkerInactivityRequiresCompleteProof(t *testing.T) {
	base := "ActiveState=inactive\nSubState=dead\nMainPID=0\nControlPID=0\nLoadState=not-found\n"
	for _, kind := range []string{"valid", "active", "pid", "control", "substate", "missing", "duplicate", "load"} {
		t.Run(kind, func(t *testing.T) {
			raw := base
			switch kind {
			case "active":
				raw = strings.Replace(raw, "inactive", "active", 1)
			case "pid":
				raw = strings.Replace(raw, "MainPID=0", "MainPID=10", 1)
			case "control":
				raw = strings.Replace(raw, "ControlPID=0", "ControlPID=12", 1)
			case "substate":
				raw = strings.Replace(raw, "dead", "stop-sigterm", 1)
			case "missing":
				raw = strings.Replace(raw, "ControlPID=0\n", "", 1)
			case "duplicate":
				raw += "MainPID=0\n"
			case "load":
				raw = strings.Replace(raw, "not-found", "error", 1)
			}
			ok, err := parseOwnedWorkerInactive(raw)
			if ok != (kind == "valid") {
				t.Fatal(kind, ok, err)
			}
		})
	}
}
func TestOwnedRecoveryRequiresDeadWorkerAndAuthenticatedPreparing(t *testing.T) {
	for _, kind := range []string{"dead", "reboot", "active", "wrong phase", "outage", "missing token", "foreign container", "drift", "corrupt receipt", "unbound", "no start", "lost removal", "receipt storage"} {
		t.Run(kind, func(t *testing.T) {
			script := `case "$1 $2" in
"context inspect") echo unix:///var/run/docker.sock;;
"buildx ls") echo '{"Current":true,"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default","Status":"running"}]}';;
"info --format") echo test-daemon;;
"container inspect") cat "$FIXTURE_DIR/inspect.json";;
"container ls") if [ -f "$FIXTURE_DIR/present" ]; then cat "$FIXTURE_DIR/present"; fi;;
"container rm") echo removed >> "$FIXTURE_DIR/removals"; rm "$FIXTURE_DIR/present"; if [ -f "$FIXTURE_DIR/lose" ]; then exit 1; fi; if [ -f "$FIXTURE_DIR/block-receipt" ]; then mkdir "$WORK_DIR/result.json"; fi;;
*) exit 9;; esac`
			d, recovery, bin := workFixture(t, "build", script)
			dir, req, err := d.loadPreparationWork(recovery.Work, recovery.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			req.OwnedBuilder = true
			req.LocalBootRecovery = true
			req.BootID = currentBootID()
			req.Env = append(req.Env, "FIXTURE_DIR="+bin, "WORK_DIR="+dir)
			if kind == "reboot" {
				req.BootID = "11111111-1111-1111-1111-111111111111"
			}
			raw, _ := json.Marshal(req)
			recovery.Work.RequestSHA256 = workHash(raw)
			if err = writeWorkJSON(dir, "request.json", req); err != nil {
				t.Fatal(err)
			}
			b, _, row := ownedBuilderFixture()
			b.WorkID = recovery.Work.ID
			b.CommandID = recovery.Work.CommandID
			b.DeploymentID = recovery.DeploymentID
			b.RequestSHA256 = recovery.Work.RequestSHA256
			row["Name"] = "/impreza-builder-" + b.WorkID
			row["Config"].(map[string]any)["Labels"] = map[string]string{"impreza.builder.work": b.WorkID, "impreza.builder.command": b.CommandID, "impreza.builder.deployment": b.DeploymentID, "impreza.builder.request": b.RequestSHA256}
			row["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"seccomp=unconfined", "apparmor=impreza-builder-" + b.WorkID}
			if kind == "foreign container" {
				row["Name"] = "/foreign"
			}
			raw, _ = json.Marshal([]any{row})
			os.WriteFile(filepath.Join(bin, "inspect.json"), raw, 0600)
			os.WriteFile(filepath.Join(bin, "present"), []byte(b.ContainerID), 0600)
			record := &ownedBuilderRecord{Version: 1, Phase: "intent", Work: *recovery.Work, DeploymentID: b.DeploymentID, BootID: req.BootID, DaemonID: "test-daemon", ImageID: b.ImageID}
			if err = createOwnedBuilderIntent(dir, record); err != nil {
				t.Fatal(err)
			}
			if kind != "unbound" {
				if err = transitionOwnedBuilder(dir, recovery.Work, b.DeploymentID, req.BootID, "test-daemon", "intent", "bound", b); err != nil {
					t.Fatal(err)
				}
			}
			state := "ActiveState=inactive\nSubState=dead\nMainPID=0\nControlPID=0\nLoadState=not-found\n"
			if kind == "active" {
				state = strings.Replace(state, "inactive", "active", 1)
			}
			os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf '"+state+"'\n"), 0700)
			if kind != "no start" {
				os.WriteFile(filepath.Join(dir, "started"), nil, 0600)
			}
			if kind == "drift" {
				os.WriteFile(filepath.Join(d.appDir(b.DeploymentID), "compose.yaml"), []byte("changed"), 0600)
			}
			if kind == "corrupt receipt" {
				os.WriteFile(filepath.Join(dir, "result.json"), []byte("{"), 0600)
			}
			if kind == "lost removal" {
				os.WriteFile(filepath.Join(bin, "lose"), nil, 0600)
			}
			if kind == "receipt storage" {
				os.WriteFile(filepath.Join(bin, "block-receipt"), nil, 0600)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input sdkclient.DeploymentControl
				json.NewDecoder(r.Body).Decode(&input)
				if input.CommandID != b.CommandID || input.ControlToken != "recovery-token" || input.Phase != "preparing" {
					t.Error("incorrect recovery authorization")
				}
				w.Header().Set("Content-Type", "application/json")
				if kind == "outage" {
					w.WriteHeader(503)
					return
				}
				phase := "preparing"
				if kind == "wrong phase" {
					phase = "replacing"
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sdkclient.DeploymentControlResponse{CommandID: b.CommandID, Phase: phase}})
			}))
			defer server.Close()
			d.Client, err = sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			cmd := &sdkclient.PollCommand{ID: b.CommandID, ControlToken: "recovery-token"}
			if kind == "missing token" {
				cmd.ControlToken = ""
			}
			recovered, err := d.RecoverOwnedPreparation(context.Background(), cmd, recovery.Work, b.DeploymentID)
			if kind == "lost removal" {
				if err == nil || recovered {
					t.Fatal("lost response counted as completion")
				}
				recovered, err = d.RecoverOwnedPreparation(context.Background(), cmd, recovery.Work, b.DeploymentID)
			}
			if kind == "receipt storage" {
				if recovered || err == nil {
					t.Fatal("storage failure counted as completion")
				}
				if err = os.Remove(filepath.Join(dir, "result.json")); err != nil {
					t.Fatal(err)
				}
				recovered, err = d.RecoverOwnedPreparation(context.Background(), cmd, recovery.Work, b.DeploymentID)
			}
			want := kind == "dead" || kind == "reboot" || kind == "lost removal" || kind == "receipt storage"
			if recovered != want || (want && err != nil) {
				t.Fatal(kind, recovered, err)
			}
			mutations, _ := os.ReadFile(filepath.Join(bin, "removals"))
			if (strings.Count(string(mutations), "removed") == 1) != want {
				t.Fatal("unexpected removal", kind, string(mutations))
			}
			if want {
				next, err := d.CompletedPreparationWork(recovery, cmd.ID)
				if err != nil || next.Phase != "aborted" {
					t.Fatal(next, err)
				}
				result, err := d.preparationWorkResult(recovery.Work, b.DeploymentID)
				if err != nil || result.Success || result.Interrupted || result.RecoveryBootID == "" {
					t.Fatal("recovery invented success/cancellation", err)
				}
				if again, err := d.RecoverOwnedPreparation(context.Background(), cmd, recovery.Work, b.DeploymentID); err != nil || again {
					t.Fatal("recovered work replayed", err)
				}
				if err = d.ForgetPreparationWork(recovery.Work); err != nil {
					t.Fatal(err)
				}
			} else if kind != "corrupt receipt" {
				if _, err = d.preparationWorkResult(recovery.Work, b.DeploymentID); !errors.Is(err, ErrPreparationPending) {
					t.Fatal("invalid recovery wrote receipt", err)
				}
			}
		})
	}
}
