package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootstrapRequiresConfirmedCredentials(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		accepted bool
	}{
		{"rejected_status", http.StatusForbidden, `{"success":true,"data":{"agent_id":"fixture","agent_secret":"synthetic"}}`, false},
		{"missing_id", http.StatusOK, `{"success":true,"data":{"agent_secret":"synthetic"}}`, false},
		{"missing_secret", http.StatusOK, `{"success":true,"data":{"agent_id":"fixture"}}`, false},
		{"empty_receipt", http.StatusOK, `{"success":true,"data":null}`, false},
		{"confirmed", http.StatusOK, `{"success":true,"data":{"agent_id":"fixture","agent_secret":"synthetic"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			result, err := Bootstrap(context.Background(), "fixture-token", BootstrapRequest{}, BootstrapOptions{BaseURL: server.URL})
			if tc.accepted {
				if err != nil || result == nil || result.AgentID != "fixture" || result.AgentSecret != "synthetic" {
					t.Fatal("confirmed bootstrap was rejected")
				}
			} else if err == nil || result != nil {
				t.Fatal("unconfirmed bootstrap returned credentials")
			}
		})
	}
}
