package packaging

// The installer downloads the agent before any trust anchor exists on
// the box, so it must not lean on same-origin checksums or moving "latest"
// paths. These tests pin the structural contract; the verifier itself is
// the exact block update.sh ships (TestInstallVerifierMatchesUpdater), so
// the manifest tests in agent-public cover its behavior once extracted
// from either script.

import (
	"bytes"
	"strings"
	"testing"
)

func manifestBlock(script []byte) string {
	start := bytes.Index(script, []byte(">>> manifest-verify"))
	end := bytes.Index(script, []byte("<<< manifest-verify"))
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return string(script[start : end+len("<<< manifest-verify")])
}

func TestInstallCarriesTheManifestContract(t *testing.T) {
	for _, pin := range []struct{ needle, what string }{
		{">>> manifest-verify", "the manifest verifier block markers"},
		{"def ed25519_verify_files(", "the RFC 8032 Ed25519 verifier for OpenSSL 1.1.1"},
		{"verify_manifest \"$TMP/manifest.signed.json\" \"$STATE_FILE\" \"$TMP/verify\"", "verifying the channel manifest before any download"},
		{"an unpinned install refuses unverified downloads", "refusing an unpinned install without the signed manifest"},
		{"checksum differs from the signed manifest; nothing was installed", "checking the binary against the manifest digest"},
		{"release size differs from the signed manifest; nothing was installed", "checking the binary size against the manifest"},
		{"MANIFEST_MODE=1", "the manifest mode switch"},
		{"VERSION=$MANIFEST_VERSION", "resolving the version through the signed manifest"},
		{"record_state", "anchoring the update chain from the install"},
		{"update.state.$CHANNEL", "sharing update.sh's state file"},
	} {
		if !bytes.Contains(InstallScript, []byte(pin.needle)) {
			t.Errorf("install.sh lost %s (%q is absent)", pin.what, pin.needle)
		}
	}
	if bytes.Contains(InstallScript, []byte("curl -fsSL https://get.docker.com | sh")) {
		t.Error("install.sh still pipes a remote script into a shell")
	}
	if bytes.Contains(InstallScript, []byte("\r\n")) {
		t.Error("install.sh carries CRLF line endings")
	}
	// The unpinned path must not fall back to the sibling .sha256: only the
	// explicitly pinned path may (update.sh's documented legacy trade).
	unpinned := string(InstallScript[strings.Index(string(InstallScript), "MANIFEST_MODE=0"):])
	if i := strings.Index(unpinned, "chmod +x"); i >= 0 {
		unpinned = unpinned[:i]
	}
	if strings.Count(unpinned, "release checksum download failed") != 1 {
		t.Errorf("the checksum fallback must exist exactly once (the pinned path); found %d", strings.Count(unpinned, "release checksum download failed"))
	}
}

func TestInstallVerifierMatchesUpdater(t *testing.T) {
	installBlock := manifestBlock(InstallScript)
	updateBlock := manifestBlock(UpdateScript)
	if installBlock == "" || updateBlock == "" {
		t.Fatal("the manifest verifier block markers are absent from a script")
	}
	if installBlock != updateBlock {
		t.Errorf("install.sh's manifest verifier diverges from update.sh's (%d vs %d bytes) — the verifier must stay the exact same block", len(installBlock), len(updateBlock))
	}
}
