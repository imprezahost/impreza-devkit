package packaging

// The installer and the updater must read the SAME distributor
// override for the release origin. A rewrite of the installer spelled it
// IMREZA_AGENT_RELEASE_BASE, so a distributor or mirror that sets the
// documented IMPREZA_AGENT_RELEASE_BASE (the one update.sh and the agent's
// upgrade path read) was silently ignored and the installer fetched from
// the default origin instead.

import (
	"regexp"
	"testing"
)

var releaseBaseOverride = regexp.MustCompile(`\$\{([A-Z_]+_RELEASE_BASE):-`)

func TestInstallAndUpdateReadTheSameReleaseBaseOverride(t *testing.T) {
	inst := releaseBaseOverride.FindAllSubmatch(InstallScript, -1)
	upd := releaseBaseOverride.FindAllSubmatch(UpdateScript, -1)
	if len(inst) == 0 || len(upd) == 0 {
		t.Fatalf("release base override not found (install.sh %d, update.sh %d)", len(inst), len(upd))
	}
	want := string(upd[0][1])
	if want != "IMPREZA_AGENT_RELEASE_BASE" {
		t.Fatalf("update.sh reads %s, expected IMPREZA_AGENT_RELEASE_BASE", want)
	}
	for _, m := range inst {
		if got := string(m[1]); got != want {
			t.Errorf("install.sh reads %s for the release origin; update.sh and the agent read %s — the documented override would be ignored", got, want)
		}
	}
}
