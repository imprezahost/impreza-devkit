package client

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPlatformSetOnionProfilePostsTier(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/profile" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || body["profile"] != "hardened" {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"command_id": "cmd_prof", "profile": "hardened", "note": "applies on the next torrc render"},
		})
	}))
	out, err := c.PlatformSetOnionProfile(context.Background(), "dpl_test", "hardened")
	if err != nil {
		t.Fatal(err)
	}
	if out.CommandID != "cmd_prof" || out.Profile != "hardened" || out.Note == "" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestPlatformSetOnionProfileRejectsBadInputBeforeHTTP(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call expected for invalid input")
	}))
	for _, tier := range []string{"", "ultra", "MAX", "Standard"} {
		if _, err := c.PlatformSetOnionProfile(context.Background(), "dpl_test", tier); err == nil {
			t.Errorf("tier %q must be refused", tier)
		}
	}
	if _, err := c.PlatformSetOnionProfile(context.Background(), "", "max"); err == nil {
		t.Error("empty deployment id must be refused")
	}
}

func TestDeployRequestsCarryOnionProfile(t *testing.T) {
	if !ValidOnionProfileTier("standard") || !ValidOnionProfileTier("hardened") || !ValidOnionProfileTier("max") {
		t.Fatal("all three tiers must validate")
	}
	if ValidOnionProfileTier("ultra") || ValidOnionProfileTier("") {
		t.Fatal("unknown tiers must not validate")
	}
	cat, err := json.Marshal(DeploymentCreateRequest{AppName: "vaultwarden", AgentID: "agt_1", Onion: true, OnionProfile: "max"})
	if err != nil {
		t.Fatal(err)
	}
	var catBody map[string]any
	if err := json.Unmarshal(cat, &catBody); err != nil {
		t.Fatal(err)
	}
	if catBody["onion_profile"] != "max" {
		t.Errorf("catalog request lost the tier: %s", cat)
	}
	custom, err := json.Marshal(CustomDeployRequest{Name: "app", AgentID: "agt_1"})
	if err != nil {
		t.Fatal(err)
	}
	var customBody map[string]any
	if err := json.Unmarshal(custom, &customBody); err != nil {
		t.Fatal(err)
	}
	if _, present := customBody["onion_profile"]; present {
		t.Error("empty tier must stay out of the body (omitempty)")
	}
}
