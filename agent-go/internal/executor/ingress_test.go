package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// The agent validates the allowlist again, whatever the server sent: a
// hostile payload is refused through the real dispatch before any Docker or
// firewall command, and nothing is stored.
func TestIngressUpdateRefusesHostilePayloadBeforeAnyCommand(t *testing.T) {
	stateDir := t.TempDir()
	d := &Docker{StateDir: stateDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, payload := range []sdkclient.IngressUpdatePayload{
		{DeploymentID: "dpl_pg1", Revision: 2, Rules: []sdkclient.IngressRule{{Port: 5432, Protocol: "tcp", Sources: []string{"0.0.0.0/0"}}}},
		{DeploymentID: "dpl_pg1", Revision: 2, Rules: []sdkclient.IngressRule{{Port: 5432, Protocol: "tcp", Sources: []string{"192.0.2.0/24 -j ACCEPT"}}}},
		{DeploymentID: "dpl_pg1", Revision: 2, Rules: []sdkclient.IngressRule{{Port: 22, Protocol: "tcp", Sources: []string{"192.0.2.0/24"}}}},
		{DeploymentID: "dpl_pg1", Revision: 2, Rules: []sdkclient.IngressRule{{Port: 5432, Protocol: "tcp", Sources: []string{"192.0.2.9/24"}}}},
		{DeploymentID: "dpl_../../etc", Revision: 2},
	} {
		raw, _ := json.Marshal(payload)
		res := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "c1", Kind: sdkclient.CommandIngressUpdate, Payload: raw})
		if res.Status != "failed" || res.Ingress == nil || res.Ingress.Enforced || res.Ingress.Reason != "invalid_policy" {
			t.Fatalf("hostile payload %+v: %+v", payload, res)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ingress.json")); !os.IsNotExist(err) {
		t.Fatal("a refused payload was stored")
	}
}
