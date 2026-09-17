package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentServiceBindingsUsesAgentRealmAndOperationBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/agent/service-bindings/dpl_aaaaaaaaaaaaaaaaaaaaaaaa" || r.Header.Get("X-Agent-Id") != "agt_fixture" || r.Header.Get("X-Agent-Secret") != "fixture" || r.Header.Get("X-API-Key") != "" {
			t.Error("incorrect credential request")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["command_id"] != "cmd_aabb" || body["control_token"] != "operation-token" || len(body) != 2 {
			t.Error("operation identity missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"protocol":"postgres-service-binding-v1","bindings":[]}}`))
	}))
	defer server.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.AgentServiceBindings(context.Background(), "dpl_aaaaaaaaaaaaaaaaaaaaaaaa", "cmd_aabb", "operation-token")
	if err != nil {
		t.Fatal(err)
	}
	if result.Protocol != "postgres-service-binding-v1" || len(result.Bindings) != 0 {
		t.Fatal("credential response mismatch")
	}
}
