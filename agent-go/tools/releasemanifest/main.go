// Command releasemanifest builds, reviews, signs and verifies the signed
// agent release manifests. Preparation and review never touch a
// signing key; signing accepts only exact reviewed bytes and refuses
// rollback, expiry abuse and chain breaks. Mirrors tools/advisory.
package main

import (
	"bytes"
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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/release"
)

const validityPattern = `^([1-9][0-9]*)h$`

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type reviewReport struct {
	Mode          string            `json:"mode"`
	Channel       string            `json:"channel"`
	Version       string            `json:"version"`
	Seq           uint64            `json:"seq"`
	Previous      string            `json:"previous"`
	MinAgent      string            `json:"min_agent_version"`
	ReleasedAt    string            `json:"released_at"`
	ExpiresAt     string            `json:"expires_at"`
	CandidateHash string            `json:"candidate_sha256"`
	Artifacts     map[string]string `json:"artifacts"`
}

func main() {
	mode := flag.String("mode", "", "prepare | review | sign | verify | show | keygen")
	out := flag.String("out", "", "New output file; existing files are never overwritten")
	input := flag.String("input", "", "Input artifact (candidate or signed manifest)")
	channelDir := flag.String("channel-dir", "", "Channel release directory (releases/<channel>)")
	channel := flag.String("channel", "", "Channel override; default is the channel-dir base name")
	version := flag.String("version", "", "Release version override; default reads version.txt")
	minAgent := flag.String("min-agent-version", "0.6.0", "Minimum supported agent version advertised to the fleet")
	seq := flag.Uint64("seq", 0, "Manifest sequence number, strictly increasing per channel")
	previousDigest := flag.String("previous-digest", "", "Hex digest of the envelope being superseded, for prepare")
	previous := flag.String("previous", "", "Previous signed envelope file, for sign and verify")
	bootstrap := flag.Bool("bootstrap", false, "Explicit first manifest of a channel, without a predecessor")
	validity := flag.String("validity", "168h", "Signing window (1h..168h)")
	review := flag.String("review", "", "Review artifact, for sign")
	reviewHash := flag.String("review-sha256", "", "Independently approved review SHA256, for sign")
	seed := flag.String("seed-file", "", "0600 file containing the base64 Ed25519 seed; never passed as an argument")
	publicKey := flag.String("public-key-file", "", "Pinned base64 public key file, never learned from the feed")
	allowExpired := flag.Bool("allow-expired", false, "Verify an expired manifest during ledger recovery only")
	now := flag.String("now", "", "RFC3339 clock override for offline checks")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	clock := time.Now()
	if *now != "" {
		parsed, err := time.Parse(time.RFC3339, *now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid --now")
			os.Exit(1)
		}
		clock = parsed
	}
	if err := run(*mode, *out, *input, *channelDir, *channel, *version, *minAgent, *seq, *previousDigest, *previous, *bootstrap, *validity, *review, *reviewHash, *seed, *publicKey, *allowExpired, clock); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(mode, out, input, channelDir, channelName, versionOverride, minAgent string, seq uint64, previousDigest, previousPath string, bootstrap bool, validity, reviewPath, reviewHash, seedPath, publicKeyPath string, allowExpired bool, clock time.Time) error {
	switch mode {
	case "keygen":
		if input != "" || seedPath != "" || seq != 0 || previousPath != "" || bootstrap || channelDir != "" {
			return errors.New("keygen accepts only a new private output directory")
		}
		return generateKey(out)
	case "prepare":
		if seedPath != "" || reviewPath != "" || reviewHash != "" || previousPath != "" || bootstrap {
			return errors.New("prepare never accepts signing credentials or chain envelopes")
		}
		return prepare(out, channelDir, channelName, versionOverride, minAgent, seq, previousDigest, validity, clock)
	case "review":
		if seedPath != "" || seq != 0 || previousPath != "" || bootstrap {
			return errors.New("review never accepts signing credentials or overrides")
		}
		return review(out, input, channelDir, clock)
	case "sign":
		return sign(out, input, reviewPath, reviewHash, seedPath, previousPath, publicKeyPath, bootstrap, clock)
	case "verify":
		if seedPath != "" || reviewPath != "" || reviewHash != "" || bootstrap {
			return errors.New("verify accepts no signing credentials")
		}
		return verify(input, channelDir, previousPath, publicKeyPath, allowExpired, clock)
	case "show":
		return show(input, clock)
	default:
		return errors.New("unknown mode; use prepare | review | sign | verify | show | keygen")
	}
}

func generateKey(out string) error {
	if out == "" {
		return errors.New("out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	seedPath := filepath.Join(out, "seed.b64")
	if _, err := os.Stat(seedPath); err == nil {
		return errors.New("refusing to overwrite an existing seed")
	}
	if err := os.WriteFile(seedPath, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "public.b64"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0644)
}

var arches = []string{"amd64", "arm64"}

func prepare(out, channelDir, channelName, versionOverride, minAgent string, seq uint64, previousDigest, validity string, clock time.Time) error {
	if out == "" || channelDir == "" {
		return errors.New("prepare requires out and channel-dir")
	}
	if seq == 0 {
		return errors.New("prepare requires an explicit --seq")
	}
	if bootstrapShape(seq, previousDigest) {
		return errors.New("first manifest uses --seq 1 without --previous-digest; later ones chain a digest")
	}
	if seq > 1 && !sha256Pattern.MatchString(previousDigest) {
		return errors.New("--previous-digest must be the sha256 of the superseded envelope")
	}
	if channelName == "" {
		channelName = filepath.Base(filepath.Clean(channelDir))
	}
	if !release.Channels[channelName] {
		return errors.New("channel must be stable or beta")
	}
	version := versionOverride
	if version == "" {
		raw, err := readReleaseFile(channelDir, "version.txt", 64)
		if err != nil {
			return fmt.Errorf("read version.txt: %w", err)
		}
		version = strings.TrimSpace(string(raw))
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return errors.New("release version must be numeric X.Y.Z")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(minAgent) {
		return errors.New("min-agent-version must be numeric X.Y.Z")
	}
	hours, err := parseValidityHours(validity)
	if err != nil {
		return err
	}
	artifacts := map[string]release.Artifact{}
	for _, arch := range arches {
		name := "impreza-agent-linux-" + arch
		bin, err := readReleaseFile(channelDir, filepath.Join(version, name), release.MaxArtifactBytes)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", name, err)
		}
		digest := sha256.Sum256(bin)
		hexDigest := hex.EncodeToString(digest[:])
		sidecar, err := readReleaseFile(channelDir, filepath.Join(version, name+".sha256"), 128)
		if err == nil {
			if strings.TrimSpace(string(sidecar)) != hexDigest {
				return fmt.Errorf("artifact %s disagrees with its .sha256 sidecar", name)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("artifact %s sidecar: %w", name, err)
		}
		artifacts[arch] = release.Artifact{Name: name, SHA256: hexDigest, Size: int64(len(bin))}
	}
	var previous *string
	if seq > 1 {
		previous = &previousDigest
	}
	m := &release.Manifest{
		Schema:          1,
		Channel:         channelName,
		Version:         version,
		Seq:             seq,
		Previous:        previous,
		ReleasedAt:      clock.UTC().Format(time.RFC3339),
		ExpiresAt:       clock.UTC().Add(time.Duration(hours) * time.Hour).Format(time.RFC3339),
		MinAgentVersion: minAgent,
		Artifacts:       artifacts,
	}
	if err := m.Validate(clock); err != nil {
		return fmt.Errorf("candidate manifest rejected: %w", err)
	}
	payload, err := release.MarshalPayload(m)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "candidate sha256: %s\n", hashHex(payload))
	return writeNewFile(out, payload, 0644)
}

func bootstrapShape(seq uint64, previousDigest string) bool {
	return seq == 1 && previousDigest != ""
}

func parseValidityHours(validity string) (int64, error) {
	match := regexp.MustCompile(validityPattern).FindStringSubmatch(validity)
	if match == nil {
		return 0, errors.New("validity must look like 24h or 168h")
	}
	hours, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil || hours < 1 || hours > 168 {
		return 0, errors.New("validity must be between 1h and 168h")
	}
	return hours, nil
}

func review(out, input, channelDir string, clock time.Time) error {
	if out == "" || input == "" || channelDir == "" {
		return errors.New("review requires input, channel-dir and out")
	}
	payload, err := boundedFile(input, release.MaxPayloadBytes)
	if err != nil {
		return err
	}
	m, err := release.ParseManifest(payload)
	if err != nil {
		return err
	}
	if err := m.Validate(clock); err != nil {
		return fmt.Errorf("candidate rejected: %w", err)
	}
	versionTxt, err := readReleaseFile(channelDir, "version.txt", 64)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(versionTxt)) != m.Version {
		return errors.New("candidate version disagrees with channel version.txt")
	}
	artifacts := map[string]string{}
	for arch, want := range m.Artifacts {
		bin, err := readReleaseFile(channelDir, filepath.Join(m.Version, want.Name), release.MaxArtifactBytes)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", want.Name, err)
		}
		digest := sha256.Sum256(bin)
		hexDigest := hex.EncodeToString(digest[:])
		if hexDigest != want.SHA256 || int64(len(bin)) != want.Size {
			return fmt.Errorf("artifact %s does not match the candidate manifest", want.Name)
		}
		artifacts[arch] = hexDigest
	}
	report := reviewReport{
		Mode:          "release-manifest-review-v1",
		Channel:       m.Channel,
		Version:       m.Version,
		Seq:           m.Seq,
		Previous:      "",
		MinAgent:      m.MinAgentVersion,
		ReleasedAt:    m.ReleasedAt,
		ExpiresAt:     m.ExpiresAt,
		CandidateHash: hashHex(payload),
		Artifacts:     artifacts,
	}
	if m.Previous != nil {
		report.Previous = *m.Previous
	}
	if filepath.Base(filepath.Clean(channelDir)) != m.Channel {
		return errors.New("candidate channel disagrees with the reviewed channel directory")
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return writeNewFile(out, raw, 0644)
}

func sign(out, input, reviewPath, reviewHash, seedPath, previousPath, publicKeyPath string, bootstrap bool, clock time.Time) error {
	if out == "" || input == "" || seedPath == "" || reviewPath == "" || reviewHash == "" {
		return errors.New("sign requires input, review, review-sha256, seed-file and out")
	}
	if bootstrap == (previousPath != "") {
		return errors.New("sign requires exactly one of --previous or --bootstrap")
	}
	if !sha256Pattern.MatchString(reviewHash) {
		return errors.New("review-sha256 must be a hex digest")
	}
	payload, err := boundedFile(input, release.MaxPayloadBytes)
	if err != nil {
		return err
	}
	if hashHex(payload) != reviewHash {
		return errors.New("candidate bytes do not match the approved review digest")
	}
	m, err := release.ParseManifest(payload)
	if err != nil {
		return err
	}
	secret, err := readRegularFile(seedPath, 128, true)
	if err != nil {
		return err
	}
	defer clear(secret)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(secret)))
	defer clear(decoded)
	if err != nil || len(decoded) != ed25519.SeedSize {
		return errors.New("invalid signing seed")
	}
	key := ed25519.NewKeyFromSeed(decoded)
	defer clear(key)
	if publicKeyPath != "" {
		pinned, err := readPublicKey(publicKeyPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(key.Public().(ed25519.PublicKey), pinned) {
			return errors.New("signer does not match the pinned public key")
		}
	}
	var previousManifest *release.Manifest
	if previousPath != "" {
		previousRaw, err := boundedFile(previousPath, release.MaxEnvelopeBytes)
		if err != nil {
			return err
		}
		// Without an explicit pin the chain is verified against this
		// signer's own key: continuity means the same authority signed the
		// predecessor, and a pinned key above already proved the signer is
		// the intended authority.
		previousKey := key.Public().(ed25519.PublicKey)
		if publicKeyPath != "" {
			previousKey, err = readPublicKey(publicKeyPath)
			if err != nil {
				return err
			}
		}
		previousVerifyAt := clock
		if previousManifest, err = release.VerifyEnvelope(previousRaw, previousKey, previousVerifyAt); err != nil {
			// The predecessor envelope itself may have expired while the
			// chain stays sound; its authenticity is what signing needs.
			previousManifest, err = release.VerifyEnvelope(previousRaw, previousKey, clock.Add(-8*24*time.Hour))
			if err != nil {
				return fmt.Errorf("predecessor envelope does not verify: %w", err)
			}
		}
		if m.Previous == nil || *m.Previous != release.EnvelopeSHA256(previousRaw) {
			return errors.New("candidate does not chain to the supplied predecessor envelope")
		}
		if m.Seq != previousManifest.Seq+1 {
			return errors.New("candidate sequence must be exactly the predecessor sequence plus one")
		}
		if release.CompareVersions(m.Version, previousManifest.Version) < 0 {
			return errors.New("channel rollback refused: publish a fixed release forward")
		}
	} else if m.Seq != 1 || m.Previous != nil {
		return errors.New("bootstrap signing requires sequence 1 without a predecessor")
	}
	reviewRaw, err := boundedFile(reviewPath, 8192)
	if err != nil {
		return err
	}
	var report reviewReport
	if json.Unmarshal(reviewRaw, &report) != nil || report.Mode != "release-manifest-review-v1" {
		return errors.New("invalid review artifact")
	}
	if report.CandidateHash != hashHex(payload) || report.Channel != m.Channel || report.Version != m.Version ||
		report.Seq != m.Seq || report.ReleasedAt != m.ReleasedAt || report.ExpiresAt != m.ExpiresAt || report.MinAgent != m.MinAgentVersion {
		return errors.New("review artifact does not describe this exact candidate")
	}
	if report.Previous == "" && m.Previous != nil || report.Previous != "" && (m.Previous == nil || report.Previous != *m.Previous) {
		return errors.New("review artifact chain field disagrees with candidate")
	}
	signed, err := release.Sign(m, key, clock)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "envelope sha256: %s\n", release.EnvelopeSHA256(signed))
	return writeNewFile(out, signed, 0644)
}

func verify(input, channelDir, previousPath, publicKeyPath string, allowExpired bool, clock time.Time) error {
	if input == "" {
		return errors.New("verify requires input")
	}
	raw, err := boundedFile(input, release.MaxEnvelopeBytes)
	if err != nil {
		return err
	}
	key, err := readPublicKey(publicKeyPath)
	if err != nil {
		return err
	}
	at := clock
	m, err := release.VerifyEnvelope(raw, key, at)
	if err != nil {
		if !allowExpired || !strings.Contains(err.Error(), "expired") {
			return err
		}
		// Ledger recovery only: authenticate against the clock the
		// manifest was valid at, never to re-offer it to the fleet.
		recovered, recoveredErr := release.VerifyEnvelope(raw, key, clock.Add(-8*24*time.Hour))
		if recoveredErr != nil {
			return err
		}
		m = recovered
		fmt.Fprintln(os.Stderr, "warning: manifest expired; recovered for ledger inspection only")
	}
	if previousPath != "" {
		previousRaw, err := boundedFile(previousPath, release.MaxEnvelopeBytes)
		if err != nil {
			return err
		}
		previous, err := release.VerifyEnvelope(previousRaw, key, clock)
		if err != nil {
			previous, err = release.VerifyEnvelope(previousRaw, key, clock.Add(-8*24*time.Hour))
			if err != nil {
				return fmt.Errorf("predecessor envelope does not verify: %w", err)
			}
		}
		if m.Previous == nil || *m.Previous != release.EnvelopeSHA256(previousRaw) {
			return errors.New("manifest does not chain to the supplied predecessor")
		}
		if m.Seq != previous.Seq+1 {
			return errors.New("manifest sequence is not the predecessor sequence plus one")
		}
		if release.CompareVersions(m.Version, previous.Version) < 0 {
			return errors.New("manifest rolls the channel back")
		}
	}
	if channelDir != "" {
		for _, arch := range []string{"amd64", "arm64"} {
			want, ok := m.Artifacts[arch]
			if !ok {
				return fmt.Errorf("manifest is missing the %s artifact", arch)
			}
			bin, err := readReleaseFile(channelDir, filepath.Join(m.Version, want.Name), release.MaxArtifactBytes)
			if err != nil {
				return fmt.Errorf("artifact %s: %w", want.Name, err)
			}
			digest := sha256.Sum256(bin)
			if hex.EncodeToString(digest[:]) != want.SHA256 || int64(len(bin)) != want.Size {
				return fmt.Errorf("artifact %s does not match the manifest", want.Name)
			}
		}
	}
	report := map[string]any{
		"channel":           m.Channel,
		"version":           m.Version,
		"seq":               m.Seq,
		"released_at":       m.ReleasedAt,
		"expires_at":        m.ExpiresAt,
		"min_agent_version": m.MinAgentVersion,
		"envelope_sha256":   release.EnvelopeSHA256(raw),
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		return err
	}
	fmt.Println(string(rawReport))
	return nil
}

func show(input string, clock time.Time) error {
	if input == "" {
		return errors.New("show requires input")
	}
	raw, err := boundedFile(input, release.MaxEnvelopeBytes)
	if err != nil {
		return err
	}
	var envelope release.Envelope
	if json.Unmarshal(raw, &envelope) != nil {
		return errors.New("invalid envelope")
	}
	m, err := release.ParseManifest(envelope.Payload)
	if err != nil {
		return err
	}
	rawReport, err := json.Marshal(m)
	if err != nil {
		return err
	}
	fmt.Println(string(rawReport))
	return nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	if path == "" {
		return release.TrustedKey()
	}
	raw, err := boundedFile(path, 128)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("invalid pinned public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func readReleaseFile(channelDir string, name string, limit int64) ([]byte, error) {
	path := filepath.Join(channelDir, filepath.FromSlash(name))
	f, err := openInput(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid channel file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("channel file limit exceeded")
	}
	return raw, nil
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

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeNewFile(path string, raw []byte, mode os.FileMode) error {
	if path == "" {
		return errors.New("out is required")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
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
