package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/release"
)

func writeChannel(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	channel := filepath.Join(dir, "stable")
	if err := os.MkdirAll(filepath.Join(channel, version), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(channel, "version.txt"), []byte(version+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "impreza-agent-linux-" + arch
		fake := []byte("fake agent " + version + " " + arch)
		if err := os.WriteFile(filepath.Join(channel, version, name), fake, 0644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(fake)
		if err := os.WriteFile(filepath.Join(channel, version, name+".sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return channel
}

func args(t *testing.T, params map[string]string) {
	t.Helper()
	saved := map[string]string{}
	for k, v := range params {
		saved[k] = os.Getenv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

func runOrFail(t *testing.T, params map[string]string) {
	t.Helper()
	execute(t, params, true)
}

func runExpectError(t *testing.T, params map[string]string) error {
	t.Helper()
	return execute(t, params, false)
}

func execute(t *testing.T, params map[string]string, mustPass bool) error {
	t.Helper()
	minAgent := params["min-agent-version"]
	if minAgent == "" {
		minAgent = "0.6.0"
	}
	validity := params["validity"]
	if validity == "" {
		validity = "168h"
	}
	err := run(params["mode"], params["out"], params["input"], params["channel-dir"], params["channel"], params["version"],
		minAgent, parseUint(params, "seq"), params["previous-digest"], params["previous"],
		params["bootstrap"] == "true", validity, params["review"], params["review-sha256"], params["seed-file"],
		params["public-key-file"], params["allow-expired"] == "true", time.Now())
	if mustPass && err != nil {
		t.Fatalf("%s failed: %v", params["mode"], err)
	}
	return err
}

func parseUint(params map[string]string, key string) uint64 {
	if params[key] == "" {
		return 0
	}
	var v uint64
	for _, c := range params[key] {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + uint64(c-'0')
	}
	return v
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TestPipelineBootAndChain exercises the full offline ceremony with a
// synthetic key: bootstrap manifest, then a chained re-sign of the same
// head (freshness touch), then a forward release.
func TestPipelineBootAndChain(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	if _, err := os.Stat(filepath.Join(keys, "public.b64")); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(keys, "seed.b64")

	channel := writeChannel(t, "0.6.20")

	candidate := filepath.Join(work, "candidate.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": candidate, "channel-dir": channel, "seq": "1", "validity": "24h"})

	reviewPath := filepath.Join(work, "review.json")
	runOrFail(t, map[string]string{"mode": "review", "out": reviewPath, "input": candidate, "channel-dir": channel})
	approved := hashFile(t, candidate)

	signed := filepath.Join(work, "manifest.signed.json")
	runOrFail(t, map[string]string{"mode": "sign", "out": signed, "input": candidate, "review": reviewPath,
		"review-sha256": approved, "seed-file": seedPath, "bootstrap": "true"})

	runOrFail(t, map[string]string{"mode": "verify", "input": signed, "public-key-file": filepath.Join(keys, "public.b64"), "channel-dir": channel})
	envelopeHash := release.EnvelopeSHA256(mustRead(t, signed))

	// Chained freshness touch of the same head version.
	candidate2 := filepath.Join(work, "candidate2.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": candidate2, "channel-dir": channel, "seq": "2", "previous-digest": envelopeHash, "validity": "24h"})
	review2 := filepath.Join(work, "review2.json")
	runOrFail(t, map[string]string{"mode": "review", "out": review2, "input": candidate2, "channel-dir": channel})
	signed2 := filepath.Join(work, "manifest2.signed.json")
	runOrFail(t, map[string]string{"mode": "sign", "out": signed2, "input": candidate2, "review": review2,
		"review-sha256": hashFile(t, candidate2), "seed-file": seedPath, "previous": signed,
		"public-key-file": filepath.Join(keys, "public.b64")})
	runOrFail(t, map[string]string{"mode": "verify", "input": signed2, "public-key-file": filepath.Join(keys, "public.b64"),
		"previous": signed, "channel-dir": channel})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSignRefusesUnapprovedCandidateBytes(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	channel := writeChannel(t, "0.6.20")
	candidate := filepath.Join(work, "candidate.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": candidate, "channel-dir": channel, "seq": "1"})
	reviewPath := filepath.Join(work, "review.json")
	runOrFail(t, map[string]string{"mode": "review", "out": reviewPath, "input": candidate, "channel-dir": channel})
	err := runExpectError(t, map[string]string{"mode": "sign", "out": filepath.Join(work, "signed.json"), "input": candidate,
		"review": reviewPath, "review-sha256": strings.Repeat("0", 64), "seed-file": filepath.Join(keys, "seed.b64"), "bootstrap": "true"})
	if err == nil || !strings.Contains(err.Error(), "approved review digest") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestSignRefusesChainBreakAndRollback(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	seedPath := filepath.Join(keys, "seed.b64")
	pubPath := filepath.Join(keys, "public.b64")

	channel1 := writeChannel(t, "0.6.20")
	c1 := filepath.Join(work, "c1.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": c1, "channel-dir": channel1, "seq": "1"})
	r1 := filepath.Join(work, "r1.json")
	runOrFail(t, map[string]string{"mode": "review", "out": r1, "input": c1, "channel-dir": channel1})
	s1 := filepath.Join(work, "s1.json")
	runOrFail(t, map[string]string{"mode": "sign", "out": s1, "input": c1, "review": r1, "review-sha256": hashFile(t, c1),
		"seed-file": seedPath, "bootstrap": "true"})
	goodDigest := release.EnvelopeSHA256(mustRead(t, s1))

	// Broken predecessor digest in the candidate.
	channel2 := writeChannel(t, "0.6.21")
	c2 := filepath.Join(work, "c2.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": c2, "channel-dir": channel2, "seq": "2", "previous-digest": strings.Repeat("e", 64)})
	r2 := filepath.Join(work, "r2.json")
	runOrFail(t, map[string]string{"mode": "review", "out": r2, "input": c2, "channel-dir": channel2})
	err := runExpectError(t, map[string]string{"mode": "sign", "out": filepath.Join(work, "s2.json"), "input": c2, "review": r2,
		"review-sha256": hashFile(t, c2), "seed-file": seedPath, "previous": s1, "public-key-file": pubPath})
	if err == nil || !strings.Contains(err.Error(), "chain") {
		t.Fatalf("chain break accepted: %v", err)
	}

	// Rollback: channel head moves to an older version.
	channelOld := writeChannel(t, "0.6.19")
	c3 := filepath.Join(work, "c3.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": c3, "channel-dir": channelOld, "seq": "2", "previous-digest": goodDigest})
	r3 := filepath.Join(work, "r3.json")
	runOrFail(t, map[string]string{"mode": "review", "out": r3, "input": c3, "channel-dir": channelOld})
	err = runExpectError(t, map[string]string{"mode": "sign", "out": filepath.Join(work, "s3.json"), "input": c3, "review": r3,
		"review-sha256": hashFile(t, c3), "seed-file": seedPath, "previous": s1, "public-key-file": pubPath})
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("rollback accepted: %v", err)
	}
}

func TestVerifyDetectsTamperedEnvelope(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	channel := writeChannel(t, "0.6.20")
	candidate := filepath.Join(work, "candidate.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": candidate, "channel-dir": channel, "seq": "1"})
	reviewPath := filepath.Join(work, "review.json")
	runOrFail(t, map[string]string{"mode": "review", "out": reviewPath, "input": candidate, "channel-dir": channel})
	signed := filepath.Join(work, "signed.json")
	runOrFail(t, map[string]string{"mode": "sign", "out": signed, "input": candidate, "review": reviewPath,
		"review-sha256": hashFile(t, candidate), "seed-file": filepath.Join(keys, "seed.b64"), "bootstrap": "true"})

	raw := mustRead(t, signed)
	var envelope release.Envelope
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("decode envelope")
	}
	tamperedPayload := strings.Replace(string(envelope.Payload), "0.6.20", "9.9.9", 1)
	tampered, _ := json.Marshal(release.Envelope{Format: 1, Payload: []byte(tamperedPayload), Signature: envelope.Signature})
	tamperedPath := filepath.Join(work, "tampered.json")
	if os.WriteFile(tamperedPath, tampered, 0644) != nil {
		t.Fatal("write tampered")
	}
	err := runExpectError(t, map[string]string{"mode": "verify", "input": tamperedPath, "public-key-file": filepath.Join(keys, "public.b64")})
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered envelope accepted: %v", err)
	}
}

func TestVerifyArtifactMismatchDetected(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	channel := writeChannel(t, "0.6.20")
	candidate := filepath.Join(work, "candidate.json")
	runOrFail(t, map[string]string{"mode": "prepare", "out": candidate, "channel-dir": channel, "seq": "1"})
	reviewPath := filepath.Join(work, "review.json")
	runOrFail(t, map[string]string{"mode": "review", "out": reviewPath, "input": candidate, "channel-dir": channel})
	signed := filepath.Join(work, "signed.json")
	runOrFail(t, map[string]string{"mode": "sign", "out": signed, "input": candidate, "review": reviewPath,
		"review-sha256": hashFile(t, candidate), "seed-file": filepath.Join(keys, "seed.b64"), "bootstrap": "true"})

	// Swap the on-disk binary after signing.
	if err := os.WriteFile(filepath.Join(channel, "0.6.20", "impreza-agent-linux-amd64"), []byte("replaced"), 0644); err != nil {
		t.Fatal(err)
	}
	err := runExpectError(t, map[string]string{"mode": "verify", "input": signed, "public-key-file": filepath.Join(keys, "public.b64"), "channel-dir": channel})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("artifact mismatch accepted: %v", err)
	}
}

func TestPrepareRefusesBadInput(t *testing.T) {
	work := t.TempDir()
	channel := writeChannel(t, "0.6.20")
	if err := runExpectError(t, map[string]string{"mode": "prepare", "out": filepath.Join(work, "c.json"), "channel-dir": channel, "seq": "2", "previous-digest": strings.Repeat("e", 64), "validity": "999h"}); err == nil {
		t.Fatal("oversized validity accepted")
	}
	if err := runExpectError(t, map[string]string{"mode": "prepare", "out": filepath.Join(work, "c.json"), "channel-dir": channel, "seq": "0"}); err == nil {
		t.Fatal("missing seq accepted")
	}
	if err := run("prepare", filepath.Join(work, "c.json"), "", "", channel, "0.6.20.1", "0.6.0", 1, "", "", false, "24h", "", "", "", "", false, time.Now()); err == nil {
		t.Fatal("four-part version accepted")
	}
}

func TestKeygenSeedIsPrivate(t *testing.T) {
	work := t.TempDir()
	keys := filepath.Join(work, "keys")
	runOrFail(t, map[string]string{"mode": "keygen", "out": keys})
	info, err := os.Stat(filepath.Join(keys, "seed.b64"))
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '\\' {
		return // Windows has no mode bits to assert.
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("seed mode too open: %v", info.Mode())
	}
	// The seed file must actually work as an Ed25519 seed.
	raw, err := os.ReadFile(filepath.Join(keys, "seed.b64"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatal("malformed generated seed")
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if bytes := key.Public(); pub.Equal(bytes.(ed25519.PublicKey)) {
		t.Fatal("unreachable")
	}
}
