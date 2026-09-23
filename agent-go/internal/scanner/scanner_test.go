package scanner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A fixture tree with a known-vulnerable lockfile entry and committed
// secrets produces findings that are redacted (no content) and carry a fix.
func TestScanFindsVulnAndSecrets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package-lock.json", `{"packages":{"node_modules/lodash":{"version":"4.17.20"}}}`)
	write(t, dir, ".env", "DATABASE_URL=postgres://user:secret@localhost/db")
	write(t, dir, "src/config.js", "const key = `-----BEGIN RSA PRIVATE KEY-----`\nmore")
	write(t, dir, "src/ok.txt", "nothing to see")

	base := &AdvisoryBase{
		GeneratedAt: "2026-09-22T00:00:00Z",
		Entries: []AdvisoryEntry{
			{Ecosystem: "npm", Name: "lodash", Version: "4.17.20", ID: "GHSA-test-001", Sev: "high", Summary: "test"},
		},
	}
	rp, err := ScanDir(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if !rp.Scanned {
		t.Fatal("report not marked scanned")
	}
	if rp.AdvisoryAge == "" {
		t.Fatal("advisory age not carried")
	}
	var hasVuln, hasEnv, hasKey bool
	for _, f := range rp.Findings {
		switch f.Kind {
		case "vulnerability":
			if f.Name == "lodash" && f.Version == "4.17.20" && f.Severity == "high" {
				hasVuln = true
			}
		case "secret":
			switch f.Name {
			case ".env_file":
				hasEnv = true
			case "private_key":
				hasKey = true
			}
		}
		if f.Fix == "" {
			t.Fatalf("finding without fix: %+v", f)
		}
	}
	if !hasVuln {
		t.Fatal("vulnerability finding missing")
	}
	if !hasEnv {
		t.Fatal(".env finding missing")
	}
	if !hasKey {
		t.Fatal("private key finding missing")
	}
	// Redaction: the report must not carry the file content or the secret.
	out, _ := os.ReadFile(filepath.Join(dir, ".env"))
	rpJSON, _ := json.Marshal(rp)
	if string(out) != "" && len(rpJSON) > 0 {
		// The scan reads it but the report must not echo it.
		if string(rpJSON) != "" && strings.Contains(string(rpJSON), "secret@localhost") {
			t.Fatal("report leaks .env content")
		}
		if strings.Contains(string(rpJSON), "BEGIN RSA PRIVATE") {
			t.Fatal("report leaks private key content")
		}
	}
}

func TestScanNilBaseSkipsVulns(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package-lock.json", `{"packages":{"node_modules/lodash":{"version":"4.17.20"}}}`)
	rp, err := ScanDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rp.Findings {
		if f.Kind == "vulnerability" {
			t.Fatal("vulnerability found with nil base")
		}
	}
}

func TestScanByteBudgetSkipsLarge(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxFileBytes+1)
	write(t, dir, "big.bin", string(big))
	rp, err := ScanDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.SkippedFiles) != 1 || rp.SkippedFiles[0] != "big.bin" {
		t.Fatalf("expected big.bin in skipped: %v", rp.SkippedFiles)
	}
}

func TestScanSkipsGitAndVendor(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".git/config", "AKIAABCDEFGHIJKLMNOP")
	write(t, dir, "vendor/pkg/key.txt", "-----BEGIN RSA PRIVATE KEY-----")
	rp, err := ScanDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Findings) != 0 {
		t.Fatalf("findings in skipped dirs: %+v", rp.Findings)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportCapsPreventDataDump(t *testing.T) {
	dir := t.TempDir()
	// A lockfile with a plausible count of "vulnerable" entries.
	lock := `{"packages":{`
	for i := 0; i < MaxFindings+50; i++ {
		if i > 0 {
			lock += ","
		}
		lock += `"node_modules/pkg` + itoa(i) + `":{"version":"1.0.0"}`
	}
	lock += `}}`
	write(t, dir, "package-lock.json", lock)
	base := &AdvisoryBase{
		GeneratedAt: "2026-09-22T00:00:00Z",
		Entries: []AdvisoryEntry{
			{Ecosystem: "npm", Name: "pkg0", Version: "1.0.0", ID: "GHSA-cap", Sev: "low", Summary: "cap"},
		},
	}
	// We only have one matching entry, so this tests the skipped-files cap
	// indirectly; the finding cap needs a broader base. For now verify the
	// constants are enforced by checking that a tree with many .env files
	// produces at most MaxFindings findings.
	for i := 0; i < MaxFindings+30; i++ {
		os.MkdirAll(filepath.Join(dir, "d"+strconv.Itoa(i/10)), 0o755)
		write(t, dir, "d"+strconv.Itoa(i/10)+"/secret"+strconv.Itoa(i%10)+".js", "-----BEGIN RSA PRIVATE KEY-----")
	}
	rp, err := ScanDir(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Findings) > MaxFindings {
		t.Fatalf("findings exceed cap: %d > %d", len(rp.Findings), MaxFindings)
	}
	if !rp.Truncated {
		t.Fatal("truncation not reported when caps hit")
	}
}

func TestSafeDepNameFilters(t *testing.T) {
	if got := safeDepName("lodash"); got != "lodash" {
		t.Fatalf("valid name filtered: %q", got)
	}
	if got := safeDepName("@scope/pkg"); got != "@scope/pkg" {
		t.Fatalf("scoped name filtered: %q", got)
	}
	if got := safeDepName("github.com/foo/bar"); got != "github.com/foo/bar" {
		t.Fatalf("go module filtered: %q", got)
	}
	if got := safeDepName("<script>alert(1)</script>"); got != "[filtered]" {
		t.Fatalf("XSS payload not filtered: %q", got)
	}
	if got := safeDepName("pkg\nwith\nnewlines"); got != "[filtered]" {
		t.Fatalf("newlines not filtered: %q", got)
	}
	long := strings.Repeat("a", 200)
	if got := safeDepName(long); got != strings.Repeat("a", 120) {
		t.Fatalf("long name not truncated to 120: %d chars", len(got))
	}
	if got := safeDepName(""); got != "[filtered]" {
		t.Fatalf("empty name not filtered: %q", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
