package executor

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeploymentCheckpointRequiresExactGrant(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		status                        int
		id, phase                     string
		cancel, wantCancel, wantError bool
	}{
		{"grant", 200, "cmd_test", "replacing", false, false, false},
		{"cancel", 200, "cmd_test", "preparing", true, true, true},
		{"cancel after replacement", 200, "cmd_test", "replacing", true, false, true},
		{"cancel missing phase", 200, "cmd_test", "", true, false, true},
		{"cancel terminal", 200, "cmd_test", "finished", true, false, true},
		{"wrong command", 200, "cmd_other", "replacing", false, false, true},
		{"wrong phase", 200, "cmd_test", "preparing", false, false, true},
		{"outage", 503, "", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input sdkclient.DeploymentControl
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/agent/command-control" || input.CommandID != "cmd_test" || input.ControlToken != "private-token" || input.Phase != "replacing" || r.Header.Get("X-Agent-Id") != "agt_test" {
					t.Error("incorrect authenticated checkpoint")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": tc.status == 200, "data": sdkclient.DeploymentControlResponse{CommandID: tc.id, Phase: tc.phase, CancelRequested: tc.cancel}})
			}))
			defer server.Close()
			client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_test", AgentSecret: "fixture-secret", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			d := &Docker{Client: client}
			err = d.deploymentCheckpoint(context.Background(), &sdkclient.PollCommand{ID: "cmd_test", ControlToken: "private-token"}, "replacing")
			if (err != nil) != tc.wantError || errors.Is(err, errDeployCancelled) != tc.wantCancel {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
func TestLegacyCheckpointDoesNotNeedControlPlane(t *testing.T) {
	if err := (&Docker{}).deploymentCheckpoint(context.Background(), &sdkclient.PollCommand{}, "replacing"); err != nil {
		t.Fatal(err)
	}
}
