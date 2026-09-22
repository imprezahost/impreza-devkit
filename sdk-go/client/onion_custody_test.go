package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

var testRecipientPub = base64.StdEncoding.EncodeToString(bytesOf(7, 32))

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestPlatformExportOnionKeyPostsSealedRequest(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/export" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["recipient_pubkey"] != testRecipientPub || body["confirm"] != true || len(body) != 2 {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"command_id": "cmd_exp1", "note": "Export queued."},
		})
	}))
	out, err := c.PlatformExportOnionKey(context.Background(), "dpl_test", testRecipientPub)
	if err != nil {
		t.Fatal(err)
	}
	if out.CommandID != "cmd_exp1" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestPlatformExportOnionKeyRejectsBadPubkeyBeforeHTTP(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call expected for invalid input")
	}))
	for _, pk := range []string{"", "not-base64!!!", base64.StdEncoding.EncodeToString(bytesOf(1, 16)), base64.RawStdEncoding.EncodeToString(bytesOf(2, 32))} {
		if _, err := c.PlatformExportOnionKey(context.Background(), "dpl_test", pk); err == nil {
			t.Errorf("pubkey %q must be refused", pk)
		}
	}
}

func TestPlatformFetchOnionKeyExportReadOnce(t *testing.T) {
	burned := false
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/export/cmd_exp1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if burned {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   map[string]string{"code": "NOT_FOUND", "message": "The sealed blob was already retrieved (read-once)."},
			})
			return
		}
		burned = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"command_id": "cmd_exp1", "onion": "abc.onion", "sealed": "SEALED", "note": "Shown once."},
		})
	}))
	out, err := c.PlatformFetchOnionKeyExport(context.Background(), "dpl_test", "cmd_exp1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Sealed != "SEALED" || out.Onion != "abc.onion" {
		t.Fatalf("unexpected payload: %+v", out)
	}
	if _, err := c.PlatformFetchOnionKeyExport(context.Background(), "dpl_test", "cmd_exp1"); err == nil {
		t.Fatal("second read must fail — the blob is burned")
	}
}

func TestPlatformRotateOnionKeyPostsConfirmAddress(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/rotate" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["confirm"] != true || body["confirm_address"] != onion || len(body) != 2 {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"command_id": "cmd_rot1", "note": "Rotation queued."},
		})
	}))
	out, err := c.PlatformRotateOnionKey(context.Background(), "dpl_test", onion)
	if err != nil {
		t.Fatal(err)
	}
	if out.CommandID != "cmd_rot1" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestPlatformRotateOnionKeyRejectsBadAddressBeforeHTTP(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call expected for invalid input")
	}))
	for _, addr := range []string{"", "not-an-onion", strings.Repeat("A", 56) + ".onion", strings.Repeat("a", 55) + ".onion", strings.Repeat("a", 56)} {
		if _, err := c.PlatformRotateOnionKey(context.Background(), "dpl_test", addr); err == nil {
			t.Errorf("address %q must be refused", addr)
		}
	}
}

func TestDeployRequestsCarryOnionImport(t *testing.T) {
	keys := &OnionImportKeys{SecretKeyB64: "SECRET96", PublicKeyB64: "PUB64"}
	custom, err := json.Marshal(CustomDeployRequest{Name: "app", AgentID: "agt_1", Onion: true, OnionImport: keys})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(custom), `"onion_import":{"secret_key_b64":"SECRET96","public_key_b64":"PUB64"}`) {
		t.Errorf("custom request lost the key pair: %s", custom)
	}
	cat, err := json.Marshal(DeploymentCreateRequest{AppName: "vaultwarden", AgentID: "agt_1", Onion: true, OnionImport: keys})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cat), `"onion_import":{"secret_key_b64":"SECRET96","public_key_b64":"PUB64"}`) {
		t.Errorf("catalog request lost the key pair: %s", cat)
	}
	empty, err := json.Marshal(CustomDeployRequest{Name: "app", AgentID: "agt_1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "onion_import") {
		t.Error("nil onion_import must stay out of the body (omitempty)")
	}
}
