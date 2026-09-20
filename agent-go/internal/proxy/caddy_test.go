package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture bcrypt hash of "fixture-preview-password" at cost 12.
const testBCryptHash = "$2y$12$Cphk3.WNMyxxiDP79ERfl.tZp2arv5.uqgqfGxxt11IS0JgHrDkiK"

func TestRenderFragmentBasicAuth(t *testing.T) {
	frag := renderFragment("dpl_x", []Route{{
		Hostname:  "pr-a1b2c3.fixture.test",
		Upstream:  "dpl_x-app:8080",
		TLSMode:   "internal",
		BasicAuth: &BasicAuth{Username: "preview", BCryptHash: testBCryptHash},
	}})
	if !strings.Contains(frag, "basic_auth {\n    preview "+testBCryptHash+"\n  }") {
		t.Fatalf("fragment missing the basic_auth gate:\n%s", frag)
	}
	// The gate precedes the proxy pass so no request reaches the app first.
	if strings.Index(frag, "basic_auth") > strings.Index(frag, "reverse_proxy") {
		t.Fatal("basic_auth must precede reverse_proxy")
	}
	if !strings.Contains(frag, "tls internal") {
		t.Fatal("internal TLS mode lost")
	}
}

func TestRenderFragmentBasicAuthOnionToo(t *testing.T) {
	frag := renderFragment("dpl_x", []Route{{
		OnionAddr: "abcdef.onion",
		Upstream:  "dpl_x-app:8080",
		BasicAuth: &BasicAuth{Username: "preview", BCryptHash: testBCryptHash},
	}})
	if !strings.Contains(frag, "http://abcdef.onion {") || !strings.Contains(frag, "basic_auth") {
		t.Fatalf("onion block must carry the gate too:\n%s", frag)
	}
}

func TestRenderFragmentWithoutBasicAuthUnchanged(t *testing.T) {
	frag := renderFragment("dpl_x", []Route{{
		Hostname: "app.example.test",
		Upstream: "dpl_x-app:8080",
		TLSMode:  "internal",
	}})
	if strings.Contains(frag, "basic_auth") {
		t.Fatalf("unprotected route grew a gate:\n%s", frag)
	}
}

func TestValidateBasicAuth(t *testing.T) {
	good := &BasicAuth{Username: "preview", BCryptHash: testBCryptHash}
	if err := validateBasicAuth(good); err != nil {
		t.Fatalf("valid gate refused: %v", err)
	}
	if err := validateBasicAuth(nil); err != nil {
		t.Fatalf("absent gate refused: %v", err)
	}
	bad := []*BasicAuth{
		{Username: "has space", BCryptHash: testBCryptHash},
		{Username: "injec}tion", BCryptHash: testBCryptHash},
		{Username: "", BCryptHash: testBCryptHash},
		{Username: "preview", BCryptHash: strings.Replace(testBCryptHash, "$2y$12$", "$2y$04$", 1)},   // cost below 10
		{Username: "preview", BCryptHash: strings.Replace(testBCryptHash, "$2y$12$", "$2y$99$", 1)},   // impossible cost
		{Username: "preview", BCryptHash: testBCryptHash[:len(testBCryptHash)-1]},                     // truncated
		{Username: "preview", BCryptHash: "not-a-hash"},
		{Username: "preview", BCryptHash: testBCryptHash + "\n"},
	}
	for i, b := range bad {
		if err := validateBasicAuth(b); err == nil {
			t.Fatalf("bad gate %d accepted", i)
		}
	}
}

func TestApplyDeploymentRoutesRefusesBadGateBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	c := &Caddy{StateDir: dir}
	err := c.ApplyDeploymentRoutes(t.Context(), "dpl_bad", []Route{{
		Hostname:  "pr-a1b2c3.fixture.test",
		Upstream:  "dpl_bad-app:8080",
		BasicAuth: &BasicAuth{Username: "preview", BCryptHash: "garbage"},
	}})
	if err == nil {
		t.Fatal("a malformed gate must refuse, not deploy unprotected")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "deployments", "dpl_bad.caddy")); !os.IsNotExist(statErr) {
		t.Fatal("refused route must not leave a fragment behind")
	}
}
