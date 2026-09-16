package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestOwnedBuildReceiptCannotInventInterruption(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			d, recovery, _ := workFixture(t, "build", `case "$1 $2" in "context inspect") echo unix:///var/run/docker.sock;; "buildx ls") echo '{"Current":true,"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default","Status":"running"}]}';; "info --format") echo test-daemon;; esac`)
			dir, request, err := d.loadPreparationWork(recovery.Work, recovery.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			request.OwnedBuilder = owned
			request.LocalBootRecovery = true
			request.BootID = currentBootID()
			raw, _ := json.Marshal(request)
			recovery.Work.RequestSHA256 = workHash(raw)
			if err = writeWorkJSON(dir, "request.json", request); err != nil {
				t.Fatal(err)
			}
			result := preparationWorkResult{Version: 1, ID: recovery.Work.ID, RequestSHA256: recovery.Work.RequestSHA256, Completed: true, Interrupted: true}
			if err = writeWorkJSON(dir, "result.json", result); err != nil {
				t.Fatal(err)
			}
			if _, err = d.preparationWorkResult(recovery.Work, recovery.DeploymentID); err == nil {
				t.Fatal("forged interruption accepted without a stop journal")
			}
		})
	}
}

func TestOwnedBuildControlRequiresAuthenticatedExactOperation(t *testing.T) {
	for _, scenario := range []string{"cancel", "no cancel", "wrong command", "wrong phase", "outage", "redirect", "missing token", "foreign work", "lost remove response", "corrupt journal"} {
		t.Run(scenario, func(t *testing.T) {
			// Script fixtures exercise the real SDK, request/journal validation and entry
			// point. The opt-in VPS test separately exercises actual Docker/Compose.
			script := `case "$1 $2" in
"context inspect") echo unix:///var/run/docker.sock;;
"buildx ls") echo '{"Current":true,"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default","Status":"running"}]}';;
"info --format") echo test-daemon;;
"container inspect") cat "$FIXTURE_DIR/inspect.json";;
"container ls") if [ -f "$FIXTURE_DIR/present" ]; then cat "$FIXTURE_DIR/present"; fi;;
"container rm") echo removed >> "$FIXTURE_DIR/removals"; rm "$FIXTURE_DIR/present"; if [ -f "$FIXTURE_DIR/lose-response" ]; then exit 1; fi;;
*) exit 9;;
esac`
			d, recovery, bin := workFixture(t, "build", script)
			dir, request, err := d.loadPreparationWork(recovery.Work, recovery.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			request.OwnedBuilder = true
			request.LocalBootRecovery = true
			request.BootID = currentBootID()
			request.Env = append(request.Env, "FIXTURE_DIR="+bin)
			raw, _ := json.Marshal(request)
			recovery.Work.RequestSHA256 = workHash(raw)
			if err = writeWorkJSON(dir, "request.json", request); err != nil {
				t.Fatal(err)
			}
			b, _, row := ownedBuilderFixture()
			b.WorkID = recovery.Work.ID
			b.CommandID = recovery.Work.CommandID
			b.RequestSHA256 = recovery.Work.RequestSHA256
			b.DeploymentID = recovery.DeploymentID
			row["Name"] = "/impreza-builder-" + b.WorkID
			row["Config"].(map[string]any)["Labels"] = map[string]string{"impreza.builder.work": b.WorkID, "impreza.builder.command": b.CommandID, "impreza.builder.deployment": b.DeploymentID, "impreza.builder.request": b.RequestSHA256}
			row["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"seccomp=unconfined", "apparmor=impreza-builder-" + b.WorkID}
			inspection, _ := json.Marshal([]any{row})
			if err = os.WriteFile(filepath.Join(bin, "inspect.json"), inspection, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(bin, "present"), []byte(b.ContainerID+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			record := &ownedBuilderRecord{Version: 1, Phase: "intent", Work: *recovery.Work, DeploymentID: b.DeploymentID, BootID: request.BootID, DaemonID: "test-daemon", ImageID: b.ImageID}
			if err = createOwnedBuilderIntent(dir, record); err != nil {
				t.Fatal(err)
			}
			if err = transitionOwnedBuilder(dir, recovery.Work, b.DeploymentID, record.BootID, record.DaemonID, "intent", "bound", b); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				var input sdkclient.DeploymentControl
				if json.NewDecoder(r.Body).Decode(&input) != nil || input.CommandID != b.CommandID || input.ControlToken != "private-control-token" || input.Phase != "preparing" || r.Header.Get("X-Agent-Id") != "agt_fixture" {
					t.Error("wrong authenticated control request")
				}
				if scenario == "redirect" {
					w.Header().Set("Location", "/foreign")
					w.WriteHeader(307)
					return
				}
				if scenario == "outage" {
					w.WriteHeader(503)
					return
				}
				id, phase := b.CommandID, "preparing"
				if scenario == "wrong command" {
					id = "cmd_foreign"
				}
				if scenario == "wrong phase" {
					phase = "replacing"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sdkclient.DeploymentControlResponse{CommandID: id, Phase: phase, CancelRequested: scenario != "no cancel"}})
			}))
			defer server.Close()
			d.Client, err = sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture-secret", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			cmd := &sdkclient.PollCommand{ID: b.CommandID, ControlToken: "private-control-token"}
			if scenario == "missing token" {
				cmd.ControlToken = ""
			}
			if scenario == "foreign work" {
				cmd.ID = "cmd_foreign"
			}
			if scenario == "lost remove response" {
				if err = os.WriteFile(filepath.Join(bin, "lose-response"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt journal" {
				if err = os.WriteFile(filepath.Join(dir, "builder.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stopped, err := d.InterruptPreparationWork(context.Background(), cmd, recovery.Work, b.DeploymentID)
			if scenario == "cancel" {
				if err != nil || !stopped {
					t.Fatal(stopped, err)
				}
			} else if scenario == "no cancel" {
				if err != nil || stopped {
					t.Fatal(stopped, err)
				}
			} else if err == nil || stopped {
				t.Fatal("invalid response authorized a stop", stopped, err)
			}
			if scenario == "lost remove response" {
				current, readErr := loadOwnedBuilderRecord(dir, recovery.Work, b.DeploymentID, record.BootID, record.DaemonID)
				if readErr != nil || current.Phase != "stopping" {
					t.Fatal(current, readErr)
				}
				stopped, err = d.InterruptPreparationWork(context.Background(), cmd, recovery.Work, b.DeploymentID)
				if err != nil || !stopped {
					t.Fatal("could not reconcile lost response", err)
				}
			}
			mutations, _ := os.ReadFile(filepath.Join(bin, "removals"))
			wantMutation := scenario == "cancel" || scenario == "lost remove response"
			if (strings.Count(string(mutations), "removed") != 0) != wantMutation || strings.Count(string(mutations), "removed") > 1 {
				t.Fatal("wrong mutation count", string(mutations))
			}
			if _, err = d.preparationWorkResult(recovery.Work, b.DeploymentID); !errors.Is(err, ErrPreparationPending) {
				t.Fatal("executor stop falsely became a worker receipt", err)
			}
			if err = d.ForgetPreparationWork(recovery.Work); err == nil {
				t.Fatal("unreceipted work was forgotten")
			}
			if (scenario == "missing token" || scenario == "foreign work" || scenario == "corrupt journal") && calls.Load() != 0 {
				t.Fatal("invalid local state reached control plane")
			}
			journal, _ := os.ReadFile(filepath.Join(dir, "builder.json"))
			if strings.Contains(string(journal), "private-control-token") || strings.Contains(string(journal), "fixture-secret") {
				t.Fatal("credentials persisted")
			}
			if wantMutation {
				if scenario == "cancel" {
					// Deterministically let cancellation win between the worker's
					// journal read and its initial Docker inspection.
					if err = os.WriteFile(filepath.Join(bin, "stopped.json"), journal, 0600); err != nil {
						t.Fatal(err)
					}
					var earlier ownedBuilderRecord
					if err = json.Unmarshal(journal, &earlier); err != nil {
						t.Fatal(err)
					}
					earlier.Phase = "bound"
					if err = writeWorkJSON(dir, "builder.json", &earlier); err != nil {
						t.Fatal(err)
					}
					body := strings.Replace(script, `cat "$FIXTURE_DIR/inspect.json"`, `cp "$FIXTURE_DIR/stopped.json" "`+filepath.Join(dir, "builder.json")+`"; exit 1`, 1)
					if err = os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err = RunPreparationWorker(d.StateDir, recovery.Work.ID); err != nil {
					t.Fatal("early stop lost worker receipt", err)
				}
				result, err := d.preparationWorkResult(recovery.Work, b.DeploymentID)
				if err != nil || !result.Completed || !result.Interrupted || result.Success {
					t.Fatal("invalid early cancellation receipt", err)
				}
				if err = d.ForgetPreparationWork(recovery.Work); err != nil {
					t.Fatal("early stopped work cannot be acknowledged", err)
				}
				after, _ := os.ReadFile(filepath.Join(bin, "removals"))
				if string(after) != string(mutations) {
					t.Fatal("worker repeated removal after verified stop")
				}
			}
		})
	}
}
