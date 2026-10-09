package packaging

// The embedded updater and the published one are the same script: the
// managed update executes these bytes and the customer may run the raw
// file, so a divergence ships two different behaviors (the embedded one
// lacked the RFC 8032 verifier for OpenSSL 1.1.1 for a whole release).
// The structural pins run everywhere; the byte comparison runs whenever a
// checkout of agent-public is reachable (release checks always set
// AGENT_PUBLIC_UPDATE_SH).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedUpdateCarriesThePublishedGuards(t *testing.T) {
	for _, pin := range []struct{ needle, what string }{
		{"def ed25519_verify_files(", "the RFC 8032 Ed25519 verifier for OpenSSL 1.1.1"},
		{">>> manifest-verify", "the manifest verifier block markers"},
		{"impreza-agent-ingress.service", "the ingress boot unit installation"},
		{"systemctl enable impreza-agent-ingress.service", "enabling the ingress boot unit"},
		// The post-update health watch must stay the 30 s window that fails
		// on any restart: shrinking the window or watching only MainPID
		// readopts a crash-looping candidate (the 15 s probe).
		{"RESTARTS=$(systemctl show impreza-agent.service -p NRestarts --value)", "the NRestarts baseline of the health watch"},
		{"while [ \"$count\" -lt 30 ]; do", "the 30-second health watch window"},
	} {
		if !bytes.Contains(UpdateScript, []byte(pin.needle)) {
			t.Errorf("the embedded update.sh lost %s (%q is absent)", pin.what, pin.needle)
		}
	}
	if bytes.Contains(UpdateScript, []byte("\r\n")) {
		t.Error("the embedded update.sh carries CRLF line endings")
	}
}

func TestEmbeddedUpdateMatchesPublished(t *testing.T) {
	candidates := []string{os.Getenv("AGENT_PUBLIC_UPDATE_SH")}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../agent-public", "../../agent-public", "../../../agent-public"} {
		candidates = append(candidates, filepath.Join(dir, rel, "update.sh"))
	}
	var published []byte
	for _, path := range candidates {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			published = raw
			break
		}
	}
	if published == nil {
		t.Skip("no agent-public checkout reachable; set AGENT_PUBLIC_UPDATE_SH — release checks always compare")
	}
	if !bytes.Equal(published, UpdateScript) {
		embedded := strings.Split(string(UpdateScript), "\n")
		tracked := strings.Split(string(published), "\n")
		for i := 0; i < len(embedded) || i < len(tracked); i++ {
			a, b := "<missing>", "<missing>"
			if i < len(embedded) {
				a = embedded[i]
			}
			if i < len(tracked) {
				b = tracked[i]
			}
			if a != b {
				t.Fatalf("the embedded update.sh diverges from the published one at line %d:\n embedded: %s\n published: %s", i+1, a, b)
			}
		}
		t.Fatal(fmt.Sprintf("the embedded update.sh (%d bytes) differs from the published one (%d bytes)", len(UpdateScript), len(published)))
	}
}
