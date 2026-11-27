package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMutationAcknowledgementRequiresTransportSuccess(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent, http.StatusFound, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				if code != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"success":true,"data":{"accepted":true}}`))
				}
			}))
			defer server.Close()
			client := &Client{BaseURL: server.URL, HTTP: server.Client()}
			want := code != http.StatusOK && code != http.StatusNoContent
			for _, tc := range []struct {
				name string
				call func() error
			}{
				{"agent_result", func() error { return client.AgentDeployResult(context.Background(), DeployResult{}) }},
				{"artifact_receipt", func() error {
					return client.PostRaw(context.Background(), "/fixture/upload", "application/octet-stream", nil, nil)
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := tc.call(); (err != nil) != want {
						t.Fatalf("transport status %d: got error %v, want rejection %t", code, err, want)
					}
				})
			}
		})
	}
}
