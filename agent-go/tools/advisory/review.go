package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/scanner"
)

type releaseOptions struct {
	Review, ReviewSHA256, Previous, PublicKey    string
	Bootstrap, AllowCoverageChange, AllowExpired bool
}

type coverage struct {
	Entries int `json:"entries"`
	Records int `json:"records"`
}

// No timestamp: a review must be reproducible in the isolated signer. Hashes
// bind exact input bytes, predecessor and pinned public key to operator approval.
type releaseReview struct {
	Schema          int                 `json:"schema"`
	PayloadSHA256   string              `json:"payload_sha256"`
	CandidateSHA256 string              `json:"candidate_sha256"`
	PreviousSHA256  string              `json:"previous_sha256"`
	PublicKeySHA256 string              `json:"public_key_sha256"`
	Bootstrap       bool                `json:"bootstrap"`
	Revision        uint64              `json:"revision"`
	GeneratedAt     string              `json:"generated_at"`
	ExpiresAt       string              `json:"expires_at"`
	Incomplete      bool                `json:"incomplete"`
	Current         map[string]coverage `json:"current"`
	Previous        map[string]coverage `json:"previous"`
	Added           int                 `json:"added_records"`
	Removed         int                 `json:"removed_records"`
	Changed         int                 `json:"changed_records"`
	Warnings        []string            `json:"warnings"`
}

func digest(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := boundedFile(path, 128)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid pinned public key")
	}
	return ed25519.PublicKey(key), nil
}

// Bucket both ecosystem and source: an intact Go GHSA source must not hide a
// vanished official Go source. Preserve duplicate entries in record digests.
func summarize(b *scanner.AdvisoryBase) (map[string]coverage, map[string]string, error) {
	counts := map[string]coverage{}
	records := map[string][]string{}
	for _, e := range b.Entries {
		bucket := e.Ecosystem + "/" + e.Source
		if bucket != "npm/github-reviewed" && bucket != "Packagist/github-reviewed" && bucket != "Go/github-reviewed" && bucket != "Go/go-vulndb" {
			return nil, nil, errors.New("unexpected advisory source/ecosystem")
		}
		c := counts[bucket]
		c.Entries++
		counts[bucket] = c
		raw, _ := json.Marshal(e)
		id := bucket + "/" + e.ID
		records[id] = append(records[id], digest(raw))
	}
	fingerprints := map[string]string{}
	for id, parts := range records {
		sort.Strings(parts)
		fingerprints[id] = digest([]byte(strings.Join(parts, "\n")))
		bucket := id[:strings.LastIndexByte(id, '/')]
		c := counts[bucket]
		c.Records++
		counts[bucket] = c
	}
	for _, bucket := range []string{"npm/github-reviewed", "Packagist/github-reviewed", "Go/github-reviewed", "Go/go-vulndb"} {
		if counts[bucket].Records == 0 {
			return nil, nil, errors.New("required advisory source absent")
		}
	}
	return counts, fingerprints, nil
}

func makeReview(raw, previous []byte, key ed25519.PublicKey, bootstrap bool, now time.Time) (*releaseReview, error) {
	if len(key) != ed25519.PublicKeySize || bootstrap == (len(previous) != 0) {
		return nil, errors.New("choose explicit bootstrap or an authenticated predecessor")
	}
	b, err := scanner.ValidateAdvisoryCandidate(raw, now)
	if err != nil {
		return nil, err
	}
	current, records, err := summarize(b)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(b)
	r := &releaseReview{Schema: 1, PayloadSHA256: digest(payload), CandidateSHA256: digest(raw), PublicKeySHA256: digest(key), Bootstrap: bootstrap,
		Revision: b.Revision, GeneratedAt: b.GeneratedAt, ExpiresAt: b.ExpiresAt, Incomplete: b.Incomplete,
		Current: current, Previous: map[string]coverage{}, Warnings: []string{}}
	if bootstrap {
		r.Added = len(records)
		return r, nil
	}
	p, err := scanner.VerifyAdvisoryBase(previous, key, now)
	if err != nil {
		return nil, errors.New("predecessor signature/schema invalid")
	}
	oldTime, _ := time.Parse(time.RFC3339, p.GeneratedAt)
	newTime, _ := time.Parse(time.RFC3339, b.GeneratedAt)
	if p.Revision >= b.Revision || newTime.Before(oldTime) {
		return nil, errors.New("release revision/time must advance without rollback")
	}
	oldCounts, oldRecords, err := summarize(p)
	if err != nil {
		return nil, err
	}
	r.PreviousSHA256, r.Previous = digest(previous), oldCounts
	for id, hash := range records {
		old, ok := oldRecords[id]
		if !ok {
			r.Added++
		} else if old != hash {
			r.Changed++
		}
	}
	for id := range oldRecords {
		if _, ok := records[id]; !ok {
			r.Removed++
		}
	}
	for bucket, count := range current {
		old := oldCounts[bucket]
		if count.Records*100 < old.Records*95 || count.Entries*100 < old.Entries*95 {
			r.Warnings = append(r.Warnings, bucket+": coverage decreased by more than 5%")
		}
		if count.Records*100 > old.Records*125 || count.Entries*100 > old.Entries*125 {
			r.Warnings = append(r.Warnings, bucket+": coverage increased by more than 25%")
		}
		// Net counts alone can hide replacement of the whole database.
		removed, changed := 0, 0
		for id := range oldRecords {
			if strings.HasPrefix(id, bucket+"/") {
				if hash, ok := records[id]; !ok {
					removed++
				} else if hash != oldRecords[id] {
					changed++
				}
			}
		}
		if removed*100 > old.Records*5 {
			r.Warnings = append(r.Warnings, bucket+": more than 5% of prior records removed")
		}
		if changed*100 > old.Records*25 {
			r.Warnings = append(r.Warnings, bucket+": more than 25% of prior records changed")
		}
	}
	sort.Strings(r.Warnings)
	return r, nil
}

func prepareReview(input string, opt releaseOptions) ([]byte, *releaseReview, error) {
	raw, err := boundedFile(input, scanner.MaxBaseBytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := readPublicKey(opt.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	var previous []byte
	if opt.Previous != "" {
		previous, err = boundedFile(opt.Previous, scanner.MaxSignedBytes)
		if err != nil {
			return nil, nil, err
		}
	}
	review, err := makeReview(raw, previous, key, opt.Bootstrap, time.Now())
	return raw, review, err
}

func approvedCandidate(input string, opt releaseOptions, allowIncomplete bool) (*scanner.AdvisoryBase, ed25519.PublicKey, error) {
	if len(opt.ReviewSHA256) != 64 || opt.Review == "" {
		return nil, nil, errors.New("sign requires review and its independently approved SHA256")
	}
	approved, err := boundedFile(opt.Review, 64<<10)
	if err != nil || digest(approved) != opt.ReviewSHA256 {
		return nil, nil, errors.New("review hash mismatch")
	}
	raw, computed, err := prepareReview(input, opt)
	if err != nil {
		return nil, nil, err
	}
	want, _ := json.Marshal(computed)
	if !bytes.Equal(approved, want) {
		return nil, nil, errors.New("review no longer matches candidate, predecessor or key")
	}
	if computed.Incomplete && !allowIncomplete {
		return nil, nil, errors.New("review partial coverage before allowing incomplete data")
	}
	if len(computed.Warnings) != 0 && !opt.AllowCoverageChange {
		return nil, nil, fmt.Errorf("coverage review required: %s", strings.Join(computed.Warnings, "; "))
	}
	base, err := scanner.ValidateAdvisoryCandidate(raw, time.Now())
	if err != nil {
		return nil, nil, err
	}
	// Return the key bound to the review; do not reread a mutable key file later.
	key, err := readPublicKey(opt.PublicKey)
	if err != nil || digest(key) != computed.PublicKeySHA256 {
		return nil, nil, errors.New("reviewed public key changed")
	}
	return base, key, nil
}
