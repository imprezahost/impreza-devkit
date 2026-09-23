package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/scanner"
)

func testBase(now time.Time, revision uint64) *scanner.AdvisoryBase {
	var entries []scanner.AdvisoryEntry
	for _, ecosystem := range []string{"npm", "Packagist", "Go"} {
		for i := 0; i < 40; i++ {
			entries = append(entries, scanner.AdvisoryEntry{Ecosystem: ecosystem, Name: "example.org/fixture", Version: "1.0.0", ID: fmt.Sprintf("GHSA-fixture-%04d", i), Source: "github-reviewed", Sev: "high"})
		}
	}
	for i := 0; i < 40; i++ {
		entries = append(entries, scanner.AdvisoryEntry{Ecosystem: "Go", Name: "example.org/fixture", Version: "1.0.0", ID: fmt.Sprintf("GO-2026-%04d", i), Source: "go-vulndb", Sev: "unknown"})
	}
	return scanner.NewAdvisoryBase(entries, revision, now, false)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func put(t *testing.T, path string, raw []byte) {
	t.Helper()
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}

func TestReviewCoverageAndPredecessor(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	previous, err := scanner.SignAdvisoryBase(testBase(now.Add(-time.Hour), 1), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "bootstrap", "no-predecessor", "bootstrap-with-predecessor", "bad-signature", "rollback", "time-rollback", "expired", "missing-source", "drop", "churn", "growth", "rewritten", "changed", "reordered", "unknown-field"} {
		t.Run(name, func(t *testing.T) {
			base := testBase(now, 2)
			prior := append([]byte(nil), previous...)
			bootstrap := false
			switch name {
			case "bootstrap":
				prior = nil
				bootstrap = true
			case "no-predecessor":
				prior = nil
			case "bootstrap-with-predecessor":
				bootstrap = true
			case "bad-signature":
				prior[len(prior)/2] ^= 1
			case "rollback":
				base.Revision = 1
			case "time-rollback":
				base.GeneratedAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
				base.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
			case "expired":
				base.GeneratedAt = now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)
				base.ExpiresAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
			case "missing-source":
				base.Entries = base.Entries[:120]
			case "drop":
				base.Entries = base.Entries[3:]
			case "churn":
				for i := 0; i < 3; i++ {
					base.Entries[i].ID += "-new"
				}
			case "growth":
				for i := 0; i < 11; i++ {
					e := base.Entries[i]
					e.ID += "-new"
					base.Entries = append(base.Entries, e)
				}
			case "rewritten":
				for i := 0; i < 11; i++ {
					base.Entries[i].Sev = "critical"
				}
			case "changed":
				base.Entries[0].Sev = "critical"
			case "reordered":
				for i, j := 0, len(base.Entries)-1; i < j; i, j = i+1, j-1 {
					base.Entries[i], base.Entries[j] = base.Entries[j], base.Entries[i]
				}
			}
			raw := mustJSON(t, base)
			if name == "unknown-field" {
				raw = append([]byte(`{"ignored":true,`), raw[1:]...)
			}
			r, err := makeReview(raw, prior, pub, bootstrap, now)
			bad := map[string]bool{"no-predecessor": true, "bootstrap-with-predecessor": true, "bad-signature": true, "rollback": true, "time-rollback": true, "expired": true, "missing-source": true, "unknown-field": true}[name]
			if bad {
				if err == nil {
					t.Fatal("invalid review accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.CandidateSHA256 != digest(raw) || r.Revision != 2 || len(r.Current) != 4 {
				t.Fatal(r)
			}
			warning := name == "drop" || name == "churn" || name == "growth" || name == "rewritten"
			if (len(r.Warnings) > 0) != warning {
				t.Fatal(r.Warnings)
			}
			if name == "churn" && (r.Removed != 3 || r.Added != 3) {
				t.Fatal(r)
			}
			if name == "changed" && r.Changed != 1 {
				t.Fatal(r)
			}
			if name == "reordered" && (r.Changed != 0 || r.Added != 0 || r.Removed != 0) {
				t.Fatal(r)
			}
		})
	}
}

// Exercise the actual command dispatch, not only the comparison helper.
func TestReleaseCommandApprovalBoundary(t *testing.T) {
	for _, name := range []string{"valid", "missing-review", "bad-review-hash", "candidate-replaced", "predecessor-replaced", "key-replaced", "wrong-signer", "partial", "partial-approved", "coverage", "coverage-approved", "output-exists", "bootstrap", "expired-after-review"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now().UTC().Truncate(time.Second)
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			input := filepath.Join(dir, "base.json")
			seed := filepath.Join(dir, "seed")
			output := filepath.Join(dir, "signed.json")
			opt := releaseOptions{Previous: filepath.Join(dir, "previous.json"), PublicKey: filepath.Join(dir, "public"), Review: filepath.Join(dir, "review.json")}
			base := testBase(now, 2)
			if strings.HasPrefix(name, "partial") {
				base.Incomplete = true
			}
			if strings.HasPrefix(name, "coverage") {
				base.Entries = base.Entries[3:]
			}
			put(t, input, mustJSON(t, base))
			put(t, seed, []byte(base64.StdEncoding.EncodeToString(key.Seed())))
			put(t, opt.PublicKey, []byte(base64.StdEncoding.EncodeToString(pub)))
			prior, e := scanner.SignAdvisoryBase(testBase(now.Add(-time.Hour), 1), key)
			if e != nil {
				t.Fatal(e)
			}
			put(t, opt.Previous, prior)
			if name == "bootstrap" {
				opt.Previous = ""
				opt.Bootstrap = true
			}
			reviewOpt := opt
			reviewOpt.Review = ""
			if e := run("review", opt.Review, input, "", 0, false, reviewOpt); e != nil {
				t.Fatal(e)
			}
			reviewBytes, e := os.ReadFile(opt.Review)
			if e != nil {
				t.Fatal(e)
			}
			opt.ReviewSHA256 = digest(reviewBytes)
			switch name {
			case "missing-review":
				opt.Review = ""
			case "bad-review-hash":
				opt.ReviewSHA256 = strings.Repeat("0", 64)
			case "candidate-replaced":
				base.Entries[0].Sev = "low"
				put(t, input, mustJSON(t, base))
			case "predecessor-replaced":
				prior, e = scanner.SignAdvisoryBase(testBase(now.Add(-2*time.Hour), 1), key)
				if e != nil {
					t.Fatal(e)
				}
				put(t, opt.Previous, prior)
			case "key-replaced":
				other, _, _ := ed25519.GenerateKey(rand.Reader)
				put(t, opt.PublicKey, []byte(base64.StdEncoding.EncodeToString(other)))
			case "wrong-signer":
				_, other, _ := ed25519.GenerateKey(rand.Reader)
				put(t, seed, []byte(base64.StdEncoding.EncodeToString(other.Seed())))
			case "coverage-approved":
				opt.AllowCoverageChange = true
			case "output-exists":
				put(t, output, []byte("preserve"))
			case "expired-after-review":
				base.GeneratedAt = now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)
				base.ExpiresAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
				put(t, input, mustJSON(t, base))
			}
			err := run("sign", output, input, seed, 0, name == "partial-approved", opt)
			good := name == "valid" || name == "partial-approved" || name == "coverage-approved" || name == "bootstrap"
			if !good {
				if err == nil {
					t.Fatal("unapproved signing accepted")
				}
				raw, e := os.ReadFile(output)
				if name == "output-exists" {
					if e != nil || string(raw) != "preserve" {
						t.Fatal("overwritten")
					}
				} else if !os.IsNotExist(e) {
					t.Fatal("failed signing created output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			verification := filepath.Join(dir, "verified.json")
			if e := run("verify", verification, output, "", 0, false, releaseOptions{PublicKey: opt.PublicKey}); e != nil {
				t.Fatal(e)
			}
			b, _ := os.ReadFile(output)
			signed, e := scanner.VerifyAdvisoryBase(b, pub, time.Now())
			if e != nil || signed.Revision != 2 {
				t.Fatal(e)
			}
			b[len(b)/2] ^= 1
			put(t, output, b)
			if run("verify", filepath.Join(dir, "bad.json"), output, "", 0, false, releaseOptions{PublicKey: opt.PublicKey}) == nil {
				t.Fatal("tampered signature verified")
			}
		})
	}
}

func TestReleaseActualSnapshot(t *testing.T) {
	input := os.Getenv("IMPREZA_F10_RELEASE_SNAPSHOT")
	if input == "" {
		t.Skip("actual collector snapshot not supplied")
	}
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	seed := filepath.Join(dir, "ephemeral-seed")
	put(t, seed, []byte(base64.StdEncoding.EncodeToString(key.Seed())))
	opt := releaseOptions{Bootstrap: true, PublicKey: filepath.Join(dir, "public"), Review: filepath.Join(dir, "review.json")}
	put(t, opt.PublicKey, []byte(base64.StdEncoding.EncodeToString(pub)))
	reviewOpt := opt
	reviewOpt.Review = ""
	if e := run("review", opt.Review, input, "", 0, false, reviewOpt); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(opt.Review)
	if e != nil {
		t.Fatal(e)
	}
	opt.ReviewSHA256 = digest(raw)
	var report releaseReview
	if json.Unmarshal(raw, &report) != nil || len(report.Current) != 4 {
		t.Fatal("missing actual source coverage")
	}
	output := filepath.Join(dir, "signed.json")
	if e := run("sign", output, input, seed, 0, true, opt); e != nil {
		t.Fatal(e)
	}
	if e := run("verify", filepath.Join(dir, "verified.json"), output, "", 0, false, releaseOptions{PublicKey: opt.PublicKey}); e != nil {
		t.Fatal(e)
	}
	envelope, e := os.ReadFile(output)
	if e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(dir, "agent-state")
	if e := os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	if e := scanner.InstallAdvisoryBase(state, envelope, pub, time.Now()); e != nil {
		t.Fatal(e)
	}
	t.Logf("actual release accepted: revision=%d entries=%v incomplete=%t", report.Revision, report.Current, report.Incomplete)
	// A second release uses the authenticated predecessor, not bootstrap.
	baseRaw, e := boundedFile(input, scanner.MaxBaseBytes)
	if e != nil {
		t.Fatal(e)
	}
	base, e := scanner.ValidateAdvisoryCandidate(baseRaw, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	base.Revision++
	next := filepath.Join(dir, "next.json")
	put(t, next, mustJSON(t, base))
	opt.Bootstrap = false
	opt.Previous = output
	opt.Review = filepath.Join(dir, "review-next.json")
	opt.ReviewSHA256 = ""
	reviewOpt = opt
	reviewOpt.Review = ""
	if e := run("review", opt.Review, next, "", 0, false, reviewOpt); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile(opt.Review)
	if e != nil {
		t.Fatal(e)
	}
	opt.ReviewSHA256 = digest(raw)
	nextSigned := filepath.Join(dir, "signed-next.json")
	if e := run("sign", nextSigned, next, seed, 0, true, opt); e != nil {
		t.Fatal(e)
	}
	nextEnvelope, e := os.ReadFile(nextSigned)
	if e != nil {
		t.Fatal(e)
	}
	if e := scanner.InstallAdvisoryBase(state, nextEnvelope, pub, time.Now()); e != nil {
		t.Fatal(e)
	}
	if scanner.InstallAdvisoryBase(state, envelope, pub, time.Now()) == nil {
		t.Fatal("agent accepted release rollback")
	}
}

func TestKeyEnrollmentNeverReplacesExistingSeed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := run("keygen", dir, "", "", 0, false); err != nil {
		t.Fatal(err)
	}
	pub, err := readPublicKey(filepath.Join(dir, "public-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readRegularFile(filepath.Join(dir, "seed"), 128, true)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	seed, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(seed)
	key := ed25519.NewKeyFromSeed(seed)
	defer clear(key)
	if digest(key.Public().(ed25519.PublicKey)) != digest(pub) {
		t.Fatal("enrolled key mismatch")
	}
	if run("keygen", dir, "", "", 0, false) == nil {
		t.Fatal("replaced existing key")
	}
	after, err := readRegularFile(filepath.Join(dir, "seed"), 128, true)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(after)
	if digest(after) != digest(raw) {
		t.Fatal("existing seed changed")
	}
	if run("keygen", filepath.Join(t.TempDir(), "refused"), "", "unexpected", 0, false) == nil {
		t.Fatal("keygen accepted external secret")
	}
}
