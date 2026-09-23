package scanner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingBaseIsExplicitAndExtensionlessSecretsDetected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----")
	write(t, dir, "long.txt", strings.Repeat("a", 70000)+"postgres://user:sensitive@database/app")
	r, e := ScanDir(dir, nil)
	if e != nil || r.DependencyStatus != "unavailable" || len(r.Findings) != 2 {
		t.Fatalf("%+v %v", r, e)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "sensitive") {
		t.Fatal("secret leaked")
	}
}
func TestCancelledScanStopsAndReportsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, e := ScanDirContext(ctx, t.TempDir(), nil)
	if e != nil || !r.Truncated {
		t.Fatal(r, e)
	}
}
func TestSourceSymlinkNeverReadsOutside(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "private.pem", "-----BEGIN PRIVATE KEY-----")
	root := t.TempDir()
	if e := os.Symlink(filepath.Join(outside, "private.pem"), filepath.Join(root, "key.pem")); e != nil {
		t.Skip("symlink unavailable")
	}
	r, e := ScanDir(root, nil)
	if e != nil || len(r.Findings) != 0 || len(r.SkippedFiles) != 1 {
		t.Fatal(r, e)
	}
}
func TestNestedDependencyAndEcosystemIsolation(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/package-lock.json", `{"packages":{"node_modules/parent/node_modules/pkg":{"version":"1.0.0"}}}`)
	b := &AdvisoryBase{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Entries: []AdvisoryEntry{{Ecosystem: "npm", Name: "pkg", Version: "1.0.0", ID: "GHSA-test", Sev: "high"}, {Ecosystem: "Go", Name: "pkg", Version: "1.0.0", ID: "GO-other", Sev: "high"}}}
	r, e := ScanDir(root, b)
	if e != nil || len(r.Findings) != 1 || r.Findings[0].ID != "GHSA-test" {
		t.Fatal(r, e)
	}
}
func TestReportBoundedBeforeReturnAndPerFileDeduplicated(t *testing.T) {
	root := t.TempDir()
	write(t, root, "many.pem", strings.Repeat("-----BEGIN PRIVATE KEY-----\n", 40000))
	r, e := ScanDir(root, nil)
	if e != nil || len(r.Findings) != 1 {
		t.Fatal(r, e)
	}
	s := scan{report: &Report{}}
	for i := 0; i < 100000; i++ {
		s.add(Finding{File: "<script>"})
		s.skip("\nunsafe")
	}
	if len(s.report.Findings) != MaxFindings || len(s.report.SkippedFiles) != MaxSkipped || !s.report.Truncated {
		t.Fatal("limits not enforced during collection")
	}
	if s.report.Findings[0].File != "[filtered-path]" {
		t.Fatal("unsafe metadata echoed")
	}
}
func TestInvalidAdvisoryBaseNeverEntersReport(t *testing.T) {
	root := t.TempDir()
	write(t, root, "advisory-base.json", `{"generated_at":"2026-09-22T00:00:00Z","entries":[{"ecosystem":"npm","name":"pkg","version":"1","id":"<script>","severity":"high"}]}`)
	if LoadAdvisoryBase(root) != nil {
		t.Fatal("malformed advisory accepted")
	}
	write(t, root, "advisory-base.json", strings.Repeat(" ", MaxBaseBytes+1))
	if LoadAdvisoryBase(root) != nil {
		t.Fatal("oversized base accepted")
	}
}
