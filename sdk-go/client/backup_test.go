package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupDatabaseSpecTransport(t *testing.T) {
	spec := BackupDatabaseSpec{
		ServiceBindingRef:    ServiceBindingRef{BindingID: "bnd_" + strings.Repeat("a", 24), ProviderDeploymentID: "dpl_" + strings.Repeat("b", 16), Variable: "DATABASE_URL", Revision: strings.Repeat("1", 64)},
		Protocol:             ServiceBindingBackupProtocol,
		ConsumerDeploymentID: "dpl_" + strings.Repeat("c", 16),
		VerifyDatabase:       "imp_verify_" + strings.Repeat("7", 16),
	}
	raw, err := json.Marshal(ManifestRuntime{Type: "docker-compose", BackupDatabase: &spec})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ManifestRuntime
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.BackupDatabase == nil || *decoded.BackupDatabase != spec {
		t.Fatal("backup database stage lost in transport")
	}
	plain, err := json.Marshal(ManifestRuntime{Type: "docker-compose"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "backup_database") {
		t.Fatal("absent database stage serialized")
	}
}

func TestAgentServiceBindingBackupTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/agent/service-binding-backup/bkpjob_aaaaaaaaaaaaaaaa" || r.Header.Get("X-Agent-Id") != "agt_fixture" {
			t.Error("incorrect backup credential request")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["command_id"] != "cmd_aabb" || body["control_token"] != "operation-token" || len(body) != 2 {
			t.Error("operation identity missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"protocol":"postgres-service-binding-backup-v1","bindings":[]}}`))
	}))
	defer server.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.AgentServiceBindingBackup(context.Background(), "bkpjob_aaaaaaaaaaaaaaaa", "cmd_aabb", "operation-token")
	if err != nil || result.Protocol != ServiceBindingBackupProtocol {
		t.Fatal("backup credential response mismatch")
	}
	if _, err := c.AgentServiceBindingBackup(context.Background(), "dpl_aaaaaaaaaaaaaaaa", "cmd_aabb", "operation-token"); err == nil {
		t.Fatal("application identity accepted as a backup job")
	}
}
