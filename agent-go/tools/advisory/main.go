// Command advisory collects a bounded OSV snapshot, or signs a reviewed snapshot.
// Collection and signing are separate operations so the signing key can stay offline.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/scanner"
)

type snapshot struct {
	URL           string   `json:"url"`
	SHA256        string   `json:"sha256"`
	Records       int      `json:"records"`
	Entries       int      `json:"entries"`
	Incomplete    int      `json:"incomplete_records"`
	IncompleteIDs []string `json:"incomplete_ids,omitempty"`
}

func main() {
	mode := flag.String("mode", "collect", "collect | review | sign | verify | keygen")
	out := flag.String("out", "", "New output file; existing files are never overwritten")
	input := flag.String("input", "", "Reviewed base JSON, for sign")
	seed := flag.String("seed-file", "", "0600 file containing the base64 Ed25519 seed; never passed as an argument")
	revision := flag.Uint64("revision", 0, "Strictly increasing release revision, for collect")
	allowIncomplete := flag.Bool("allow-incomplete", false, "Acknowledge unsupported records reported by collect before signing")
	review := flag.String("review", "", "Review artifact, for sign")
	reviewHash := flag.String("review-sha256", "", "Independently approved review SHA256, for sign")
	previous := flag.String("previous", "", "Previous signed release, required except explicit bootstrap")
	publicKey := flag.String("public-key-file", "", "Pinned base64 public key file, never learned from the feed")
	bootstrap := flag.Bool("bootstrap", false, "Explicit first release only, without a predecessor")
	allowCoverage := flag.Bool("allow-coverage-change", false, "Acknowledge anomalous coverage after reviewing the report")
	allowExpired := flag.Bool("allow-expired", false, "Verify an expired artifact during ledger recovery only")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	if err := run(*mode, *out, *input, *seed, *revision, *allowIncomplete, releaseOptions{Review: *review, ReviewSHA256: *reviewHash, Previous: *previous, PublicKey: *publicKey, Bootstrap: *bootstrap, AllowCoverageChange: *allowCoverage, AllowExpired: *allowExpired}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(mode, out, input, seed string, revision uint64, allowIncomplete bool, options ...releaseOptions) error {
	opt := releaseOptions{}
	if len(options) == 1 {
		opt = options[0]
	}
	if out == "" {
		return errors.New("out is required")
	}
	var raw []byte
	var err error
	if mode == "keygen" {
		if input != "" || seed != "" || revision != 0 || allowIncomplete || opt != (releaseOptions{}) {
			return errors.New("keygen accepts only a new private output directory")
		}
		return generateKey(out)
	} else if mode == "collect" {
		if revision == 0 || input != "" || seed != "" || opt != (releaseOptions{}) || allowIncomplete {
			return errors.New("collect requires a revision and never accepts a signing key")
		}
		base, evidence, e := collect()
		if e != nil {
			return e
		}
		base.Revision = revision
		raw, err = json.Marshal(base)
		if err != nil || len(raw) > scanner.MaxBaseBytes {
			return errors.New("compiled base exceeds size limit")
		}
		for _, s := range evidence {
			if err := json.NewEncoder(os.Stdout).Encode(s); err != nil {
				return err
			}
		}
	} else if mode == "review" {
		if seed != "" || revision != 0 || allowIncomplete || opt.AllowExpired || opt.AllowCoverageChange || opt.Review != "" || opt.ReviewSHA256 != "" {
			return errors.New("review never accepts signing credentials or overrides")
		}
		_, report, e := prepareReview(input, opt)
		if e != nil {
			return e
		}
		raw, err = json.Marshal(report)
		if err != nil {
			return err
		}
	} else if mode == "verify" {
		if seed != "" || revision != 0 || allowIncomplete || opt.Bootstrap || opt.Previous != "" || opt.Review != "" || opt.ReviewSHA256 != "" || opt.AllowCoverageChange {
			return errors.New("verify accepts only input, out and pinned public key")
		}
		key, e := readPublicKey(opt.PublicKey)
		if e != nil {
			return e
		}
		envelope, e := boundedFile(input, scanner.MaxSignedBytes)
		if e != nil {
			return e
		}
		base, e := scanner.VerifyAdvisoryBase(envelope, key, time.Now())
		if e != nil {
			return e
		}
		expires, _ := time.Parse(time.RFC3339, base.ExpiresAt)
		if !expires.After(time.Now()) && !opt.AllowExpired {
			return errors.New("signed release expired")
		}
		counts, _, e := summarize(base)
		if e != nil {
			return e
		}
		raw, err = json.Marshal(struct {
			GeneratedAt string              `json:"generated_at"`
			SHA256      string              `json:"sha256"`
			Revision    uint64              `json:"revision"`
			ExpiresAt   string              `json:"expires_at"`
			Incomplete  bool                `json:"incomplete"`
			Coverage    map[string]coverage `json:"coverage"`
		}{base.GeneratedAt, digest(envelope), base.Revision, base.ExpiresAt, base.Incomplete, counts})
		if err != nil {
			return err
		}
	} else if mode == "sign" {
		if seed == "" || input == "" || revision != 0 || opt.AllowExpired {
			return errors.New("sign requires input and seed-file without a revision override")
		}
		base, publicKey, e := approvedCandidate(input, opt, allowIncomplete)
		if e != nil {
			return e
		}
		secret, e := readRegularFile(seed, 128, true)
		if e != nil {
			return e
		}
		defer clear(secret)
		decoded, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(secret)))
		defer clear(decoded)
		if e != nil || len(decoded) != ed25519.SeedSize {
			return errors.New("invalid signing seed")
		}
		key := ed25519.NewKeyFromSeed(decoded)
		defer clear(key)
		if !bytes.Equal(key.Public().(ed25519.PublicKey), publicKey) {
			return errors.New("signer does not match reviewed public key")
		}
		raw, err = scanner.SignAdvisoryBase(base, key)
		if err != nil {
			return err
		}
	} else {
		return errors.New("unknown mode")
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	return closed
}

func boundedFile(path string, limit int64) ([]byte, error) {
	return readRegularFile(path, limit, false)
}

func readRegularFile(path string, limit int64, private bool) ([]byte, error) {
	f, err := openInput(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || (private && os.PathSeparator != '\\' && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("invalid input file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("input limit exceeded")
	}
	return raw, nil
}

func collect() (*scanner.AdvisoryBase, []snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 8 * time.Minute, Transport: &http.Transport{TLSHandshakeTimeout: 20 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OSV redirect refused") }}
	defer client.CloseIdleConnections()
	var entries []scanner.AdvisoryEntry
	var evidence []snapshot
	incomplete := false
	for _, ecosystem := range []string{"npm", "Packagist", "Go"} {
		part, report, err := collectEcosystem(ctx, client, ecosystem)
		if err != nil {
			return nil, nil, fmt.Errorf("%s collection failed: %w", ecosystem, err)
		}
		if len(part) == 0 {
			return nil, nil, errors.New("source selection empty; refusing incomplete snapshot")
		}
		entries = append(entries, part...)
		evidence = append(evidence, report)
		incomplete = incomplete || report.Incomplete > 0
		if len(entries) > scanner.MaxEntries {
			return nil, nil, errors.New("too many compiled entries")
		}
	}
	return scanner.NewAdvisoryBase(entries, 1, time.Now(), incomplete), evidence, nil
}

func collectEcosystem(ctx context.Context, client *http.Client, ecosystem string) ([]scanner.AdvisoryEntry, snapshot, error) {
	const maxArchive = 512 << 20
	report := snapshot{URL: "https://storage.googleapis.com/osv-vulnerabilities/" + ecosystem + "/all.zip"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, report.URL, nil)
	if err != nil {
		return nil, report, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, report, errors.New("download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength > maxArchive {
		return nil, report, errors.New("download refused")
	}
	f, err := os.CreateTemp("", "impreza-osv-*.zip")
	if err != nil {
		return nil, report, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, maxArchive+1))
	if err != nil || n > maxArchive {
		return nil, report, errors.New("archive exceeds limit or incomplete")
	}
	report.SHA256 = hex.EncodeToString(hash.Sum(nil))
	z, err := zip.NewReader(f, n)
	if err != nil || len(z.File) > 500000 {
		return nil, report, errors.New("invalid archive")
	}
	var entries []scanner.AdvisoryEntry
	total := int64(0)
	seen := map[string]bool{}
	sources := map[string]int{}
	for _, member := range z.File {
		if ctx.Err() != nil {
			return nil, report, ctx.Err()
		}
		name := member.Name
		if !strings.HasPrefix(name, "GHSA-") && !(ecosystem == "Go" && strings.HasPrefix(name, "GO-")) {
			continue
		}
		if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".json") || !member.Mode().IsRegular() || member.UncompressedSize64 > 2<<20 || seen[name] {
			return nil, report, errors.New("invalid selected archive member")
		}
		seen[name] = true
		reader, e := member.Open()
		if e != nil {
			return nil, report, e
		}
		raw, e := io.ReadAll(io.LimitReader(reader, (2<<20)+1))
		reader.Close()
		total += int64(len(raw))
		if e != nil || len(raw) > 2<<20 || total > 256<<20 {
			return nil, report, errors.New("expanded records exceed limit")
		}
		var identity struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &identity) != nil || identity.ID+".json" != name {
			return nil, report, errors.New("record identity mismatch")
		}
		part, partial, e := scanner.NormalizeOSV(raw, ecosystem)
		if e != nil {
			return nil, report, fmt.Errorf("record %s: %w", name, e)
		}
		if partial {
			report.Incomplete++
			if len(report.IncompleteIDs) < 100 {
				report.IncompleteIDs = append(report.IncompleteIDs, identity.ID)
			}
		}
		if len(part) > 0 {
			report.Records++
			sources[part[0].Source]++
		}
		entries = append(entries, part...)
		if len(entries) > scanner.MaxEntries {
			return nil, report, errors.New("too many entries")
		}
	}
	if sources["github-reviewed"] == 0 || ecosystem == "Go" && sources["go-vulndb"] == 0 {
		return nil, report, errors.New("required upstream source absent")
	}
	report.Entries = len(entries)
	return entries, report, nil
}

// A new directory is mandatory. Never replace an existing key, including a
// partially completed enrollment: investigate and preserve it first.
func generateKey(directory string) error {
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer clear(key)
	seed := key.Seed()
	defer clear(seed)
	for _, item := range []struct {
		name string
		data []byte
	}{
		{"seed", []byte(base64.StdEncoding.EncodeToString(seed))},
		{"public-key.txt", []byte(base64.StdEncoding.EncodeToString(pub))},
		{"fingerprint.txt", []byte(digest(pub) + "\n")},
	} {
		f, e := os.OpenFile(filepath.Join(directory, item.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(item.data)
		clear(item.data)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	if os.PathSeparator != '\\' {
		for _, path := range []string{directory, filepath.Dir(directory)} {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = f.Sync()
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
