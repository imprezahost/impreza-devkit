package cmd

// The boot-unit check speaks through one journal line per condition and,
// for a disabled unit, a single enable attempt. These tests stub systemctl
// and the unit path, the way the poll tests stub the docker CLI: removing
// the is-enabled check must fail the disabled case here.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubSystemctl writes a systemctl that answers is-enabled with the given
// state (exit 0 only for "enabled") and records every enable call.
func stubSystemctl(t *testing.T, state string, enableFails bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		"if [ \"$1\" = \"is-enabled\" ]; then printf '%s\\n' " + state + "; [ " + state + " = enabled ] && exit 0; exit 1; fi\n" +
		"if [ \"$1\" = \"enable\" ]; then " + map[bool]string{true: "exit 1", false: "exit 0"}[enableFails] + "; fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func withBootUnitFixture(t *testing.T, unitFileExists bool, state string, enableFails bool) (*bytes.Buffer, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("systemctl stub runs on Linux")
	}
	originalFile, originalCtl := ingressUnitFile, ingressSystemctl
	unit := filepath.Join(t.TempDir(), "impreza-agent-ingress.service")
	ingressUnitFile = unit
	ingressSystemctl = stubSystemctl(t, state, enableFails)
	t.Cleanup(func() { ingressUnitFile, ingressSystemctl = originalFile, originalCtl })
	if unitFileExists {
		if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	return &buf, filepath.Join(filepath.Dir(ingressSystemctl), "calls")
}

func TestBootUnitCheckMissingFileWarns(t *testing.T) {
	buf, _ := withBootUnitFixture(t, false, "enabled", false)
	checkIngressBootUnit(slog.New(slog.NewTextHandler(buf, nil)))
	if !strings.Contains(buf.String(), "not installed") {
		t.Fatalf("a missing unit file must warn: %s", buf.String())
	}
}

func TestBootUnitCheckEnabledStaysQuiet(t *testing.T) {
	buf, calls := withBootUnitFixture(t, true, "enabled", false)
	checkIngressBootUnit(slog.New(slog.NewTextHandler(buf, nil)))
	if buf.Len() != 0 {
		t.Fatalf("an installed and enabled unit must not warn: %s", buf.String())
	}
	if raw, err := os.ReadFile(calls); err == nil && strings.Contains(string(raw), "enable impreza-agent-ingress.service") {
		t.Fatal("an enabled unit must not be touched")
	}
}

func TestBootUnitCheckDisabledWarnsAndEnablesOnce(t *testing.T) {
	buf, calls := withBootUnitFixture(t, true, "disabled", false)
	checkIngressBootUnit(slog.New(slog.NewTextHandler(buf, nil)))
	if !strings.Contains(buf.String(), "not enabled") {
		t.Fatalf("a disabled unit must warn: %s", buf.String())
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "enable impreza-agent-ingress.service") != 1 {
		t.Fatalf("the disabled unit must get exactly one enable attempt: %s", raw)
	}
}

func TestBootUnitCheckEnableFailureWarnsTheRepair(t *testing.T) {
	buf, calls := withBootUnitFixture(t, true, "disabled", true)
	checkIngressBootUnit(slog.New(slog.NewTextHandler(buf, nil)))
	if !strings.Contains(buf.String(), "not enabled") || !strings.Contains(buf.String(), "repair it with systemctl enable impreza-agent-ingress.service") {
		t.Fatalf("a failed enable must name the repair command: %s", buf.String())
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "enable impreza-agent-ingress.service") != 1 {
		t.Fatalf("only one enable attempt, even when it fails: %s", raw)
	}
}
