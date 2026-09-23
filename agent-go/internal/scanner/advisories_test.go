package scanner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdvisoryEcosystemRanges(t *testing.T) {
	for _, test := range []struct {
		eco, version string
		interval     Interval
		want         bool
	}{
		{"npm", "1.2.3", Interval{Introduced: "1.0.0", Fixed: "1.2.4"}, true},
		{"npm", "1.2.4", Interval{Introduced: "1.0.0", Fixed: "1.2.4"}, false},
		{"npm", "0.9.9", Interval{Introduced: "1.0.0", Fixed: "1.2.4"}, false},
		{"npm", "2.0.0-beta.1", Interval{Introduced: "0", Fixed: "2.0.0"}, true},
		{"npm", "1.2.3+build.2", Interval{Introduced: "0", Fixed: "1.2.3"}, false},
		{"npm", "1.2.3", Interval{Introduced: "0", LastAffected: "1.2.3"}, true},
		{"npm", "1.2.3", Interval{Introduced: "0", Limit: "1.2.3"}, false},
		{"Go", "v0.0.0-20200101000000-0123456789ab", Interval{Introduced: "0", Fixed: "0.0.0-20200201000000-0123456789ab"}, true},
		{"Go", "v1.2.0", Interval{Introduced: "1.2.0"}, true},
		{"Packagist", "v1.2.3", Interval{Introduced: "1.0.0", Fixed: "1.2.4"}, true},
		{"Packagist", "1.2.3-RC1", Interval{Introduced: "0", Fixed: "1.2.3"}, true},
		{"Packagist", "4.3alpha1", Interval{Introduced: "4.3alpha1", Fixed: "4.3beta2"}, true},
		{"Packagist", "1.2.3.4", Interval{Introduced: "1.2.3.3", Fixed: "1.2.3.5"}, true},
	} {
		t.Run(test.eco+test.version+test.interval.Fixed+test.interval.Limit, func(t *testing.T) {
			if !validInterval(test.eco, test.interval) {
				t.Fatal("interval rejected")
			}
			got, known := affected(AdvisoryEntry{Ecosystem: test.eco, Ranges: []Interval{test.interval}}, test.version)
			if got != test.want || !known {
				t.Fatalf("got %v/%v want %v", got, known, test.want)
			}
		})
	}
	for _, r := range []Interval{{Introduced: "bad"}, {Introduced: "2.0.0", Fixed: "1.0.0"}, {Introduced: "1.0.0", Fixed: "1.0.0"}, {Introduced: "0", Fixed: "1.0.0", Limit: "2.0.0"}} {
		if validInterval("npm", r) {
			t.Fatal("invalid interval accepted")
		}
	}
}

func fixtureBase(t *testing.T, revision uint64, key ed25519.PrivateKey) []byte {
	t.Helper()
	b := NewAdvisoryBase([]AdvisoryEntry{{Ecosystem: "npm", Name: "fixture-dependency", ID: "GHSA-fixture", Sev: "high", Source: "github-reviewed", Ranges: []Interval{{Introduced: "0", Fixed: "1.2.0"}}}}, revision, time.Now().Truncate(time.Second), false)
	raw, err := SignAdvisoryBase(b, key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSignedAdvisoryTrustAndRollback(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("IMPREZA_ADVISORY_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	dir := t.TempDir()
	now := time.Now()
	raw := fixtureBase(t, 2, key)
	if err := InstallAdvisoryBase(dir, raw, pub, now); err != nil {
		t.Fatal(err)
	}
	if err := InstallAdvisoryBase(dir, raw, pub, now); err != nil {
		t.Fatal("idempotent install", err)
	}
	if b := LoadAdvisoryBase(dir); b == nil || b.Revision != 2 {
		t.Fatal("signed load failed")
	}
	bad := append([]byte(nil), raw...)
	bad[len(bad)/2] ^= 1
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, candidate := range map[string][]byte{"tampered": bad, "rollback": fixtureBase(t, 1, key), "unsigned": []byte(`{"entries":[]}`), "oversize": bytes.Repeat([]byte(" "), MaxSignedBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if InstallAdvisoryBase(dir, candidate, pub, now) == nil {
				t.Fatal("accepted")
			}
		})
	}
	if InstallAdvisoryBase(dir, fixtureBase(t, 3, key), other, now) == nil {
		t.Fatal("wrong signer")
	}
	var b AdvisoryBase
	verified, _ := VerifyAdvisoryBase(raw, pub, now)
	b = *verified
	b.Entries[0].Sev = "critical"
	conflict, _ := SignAdvisoryBase(&b, key)
	if InstallAdvisoryBase(dir, conflict, pub, now) == nil {
		t.Fatal("revision equivocation")
	}
	if InstallAdvisoryBase(dir, fixtureBase(t, 3, key), pub, now.Add(8*24*time.Hour)) == nil {
		t.Fatal("expired update")
	}
	saved, _ := os.ReadFile(filepath.Join(dir, SignedBaseName))
	if !bytes.Equal(saved, raw) {
		t.Fatal("last good database replaced")
	}
	b.Entries[0].Sev = "high"
	b.Revision = 3
	b.GeneratedAt = now.Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	b.ExpiresAt = now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	stale, _ := SignAdvisoryBase(&b, key)
	if _, err := VerifyAdvisoryBase(stale, pub, now); err != nil {
		t.Fatal("stale cache should still verify", err)
	}
	r, e := ScanDir(t.TempDir(), &b)
	if e != nil || r.DependencyStatus != "stale" {
		t.Fatal("stale cache not explicit")
	}
	write(t, dir, SignedBaseName, "corrupt")
	if InstallAdvisoryBase(dir, fixtureBase(t, 4, key), pub, now) == nil {
		t.Fatal("corrupt revision silently reset")
	}
	if LoadAdvisoryBase(dir) != nil {
		t.Fatal("corrupt cache trusted")
	}
}

func TestSignedAdvisoryHTTPAndAtomicReader(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := fixtureBase(t, 10, key)
	dir := t.TempDir()
	t.Setenv("IMPREZA_ADVISORY_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	mode := "ok"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.ContentLength > 0 || r.URL.RawQuery != "" || r.Header.Get("X-Agent-Secret") != "" || r.Header.Get("Authorization") != "" {
			t.Error("private data sent")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, "https://example.invalid/", 302)
		case "failure":
			w.WriteHeader(503)
		case "truncated":
			w.Header().Set("Content-Length", fmt.Sprint(len(raw)+100))
			w.Write(raw[:100])
		default:
			w.Write(raw)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if err := updateAdvisories(t.Context(), dir, server.URL, pub, client); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"redirect", "failure", "truncated"} {
		mode = m
		if updateAdvisories(t.Context(), dir, server.URL, pub, client) == nil {
			t.Fatal("accepted", m)
		}
	}
	for _, address := range []string{strings.Replace(server.URL, "https:", "http:", 1), server.URL + "?inventory=secret", server.URL + "#fragment", "https://user:pass@example.com"} {
		if updateAdvisories(t.Context(), dir, address, pub, client) == nil {
			t.Fatal("unsafe URL")
		}
	}
	if requests != 4 {
		t.Fatalf("unexpected requests %d", requests)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if updateAdvisories(ctx, dir, server.URL, pub, client) == nil {
		t.Fatal("cancelled download")
	}
	if got := LoadAdvisoryBase(dir); got == nil || got.Revision != 10 {
		t.Fatal("lost cache")
	}
	// Readers never observe a half-written signed envelope during replacement.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := uint64(11); i < 20; i++ {
			if err := InstallAdvisoryBase(dir, fixtureBase(t, i, key), pub, time.Now()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		if LoadAdvisoryBase(dir) == nil {
			t.Fatal("partial atomic read")
		}
	}
	<-done
}

func TestOSVSelectionAndCoverage(t *testing.T) {
	raw := `{"id":"GHSA-35jh-r3h4-6jhm","modified":"2026-09-20T00:00:00Z","database_specific":{"github_reviewed":true,"severity":"MODERATE"},"aliases":["CVE-2026-12345"],"affected":[{"package":{"ecosystem":"npm","name":"fixture-dependency"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"1.2.0"},{"introduced":"2.0.0"},{"last_affected":"2.0.1"}]}]}]}`
	entries, partial, err := NormalizeOSV([]byte(raw), "npm")
	if err != nil || partial || len(entries) != 1 || len(entries[0].Ranges) != 2 {
		t.Fatal(entries, partial, err)
	}
	for _, edit := range []string{strings.Replace(raw, `"github_reviewed":true`, `"github_reviewed":false`, 1), strings.Replace(raw, `"modified":`, `"withdrawn":"2026-09-21T00:00:00Z","modified":`, 1)} {
		e, _, err := NormalizeOSV([]byte(edit), "npm")
		if err != nil || len(e) != 0 {
			t.Fatal("excluded upstream accepted")
		}
	}
	broken := strings.Replace(raw, `"SEMVER"`, `"GIT"`, 1)
	e, partial, err := NormalizeOSV([]byte(broken), "npm")
	if err != nil || !partial || len(e) != 0 {
		t.Fatal("unsupported range hidden")
	}
	root := t.TempDir()
	write(t, root, "package-lock.json", `{"packages":{"node_modules/fixture-dependency":{"version":"1.1.9"}}}`)
	duplicate := entries[0]
	duplicate.ID = "GHSA-alias"
	duplicate.Aliases = append(duplicate.Aliases, entries[0].ID)
	base := NewAdvisoryBase(append(entries, duplicate), 1, time.Now(), false)
	report, err := ScanDir(root, base)
	if err != nil || len(report.Findings) != 1 || report.Findings[0].Severity != "moderate" {
		t.Fatal("range/alias integration", report, err)
	}
}

func TestSignedAdvisoryFilesystemBoundary(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := fixtureBase(t, 1, key)
	for _, name := range []string{SignedBaseName, "advisories.lock"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "sentinel")
			os.WriteFile(outside, []byte("unchanged"), 0600)
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Skip("symlink unavailable")
			}
			if InstallAdvisoryBase(dir, raw, pub, time.Now()) == nil {
				t.Fatal("symlink accepted")
			}
			got, _ := os.ReadFile(outside)
			if string(got) != "unchanged" {
				t.Fatal("outside file changed")
			}
		})
	}
	dir := t.TempDir()
	write(t, dir, "advisory-base.json", `{"generated_at":"2026-09-22T00:00:00Z","entries":[]}`)
	t.Setenv("IMPREZA_ADVISORY_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	if LoadAdvisoryBase(dir) != nil {
		t.Fatal("unsigned downgrade accepted")
	}
}

func TestCollectedSnapshotAcceptanceLive(t *testing.T) {
	path := os.Getenv("IMPREZA_F10_SNAPSHOT")
	if path == "" {
		t.Skip("reviewed OSV snapshot required")
	}
	key, err := TrustedAdvisoryKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readBaseFile(filepath.Dir(path), filepath.Base(path), MaxSignedBytes)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = InstallAdvisoryBase(dir, raw, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	base := LoadAdvisoryBase(dir)
	if base == nil || len(base.Entries) < 1000 {
		t.Fatal("snapshot not loaded")
	}
	root := t.TempDir()
	write(t, root, "package-lock.json", `{"packages":{"node_modules/lodash":{"version":"4.17.19"}}}`)
	write(t, root, "composer.lock", `{"packages":[{"name":"symfony/http-foundation","version":"v2.0.0"}]}`)
	write(t, root, "go.sum", "golang.org/x/crypto v0.17.0 h1:synthetic\n")
	report, err := ScanDir(root, base)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, finding := range report.Findings {
		seen[finding.Name] = true
	}
	for _, name := range []string{"lodash", "symfony/http-foundation", "golang.org/x/crypto"} {
		if !seen[name] {
			t.Fatalf("real snapshot failed to find %s", name)
		}
	}
	if report.DependencyStatus != "available" || report.Truncated != base.Incomplete {
		t.Fatal("coverage status missing")
	}
	t.Logf("real OSV snapshot: %d entries, revision %d, three ecosystems matched, incomplete=%v", len(base.Entries), base.Revision, base.Incomplete)
}

func TestPublicUpdaterUsesTrustTransportAndNoFallback(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := fixtureBase(t, 1, key)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Agent-Secret") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credential leaked")
		}
		w.Write(raw)
	}))
	defer server.Close()
	previous := http.DefaultTransport
	transport := server.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	t.Setenv("IMPREZA_ADVISORY_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("IMPREZA_ADVISORY_URL", server.URL)
	dir := t.TempDir()
	if err := UpdateAdvisories(t.Context(), dir, false, ""); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	if UpdateAdvisories(t.Context(), dir, true, "socks5h://"+address) == nil {
		t.Fatal("unreachable proxy accepted")
	}
	if UpdateAdvisories(t.Context(), dir, true, "http://"+address) == nil {
		t.Fatal("invalid proxy accepted")
	}
	t.Setenv("IMPREZA_ADVISORY_PUBLIC_KEY", "invalid")
	if UpdateAdvisories(t.Context(), dir, false, "") == nil {
		t.Fatal("missing trust accepted")
	}
	if requests != 1 {
		t.Fatal("proxy failure fell back to direct transport")
	}
}
