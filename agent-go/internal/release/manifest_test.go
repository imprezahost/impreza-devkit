package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testManifest(now time.Time) *Manifest {
	return &Manifest{
		Schema:          1,
		Channel:         "stable",
		Version:         "0.6.20",
		Seq:             2,
		Previous:        strPtr(strings.Repeat("a", 64)),
		ReleasedAt:      now.UTC().Format(time.RFC3339),
		ExpiresAt:       now.UTC().Add(24 * time.Hour).Format(time.RFC3339),
		MinAgentVersion: "0.6.0",
		Artifacts: map[string]Artifact{
			"amd64": {Name: "impreza-agent-linux-amd64", SHA256: strings.Repeat("b", 64), Size: 12345},
			"arm64": {Name: "impreza-agent-linux-arm64", SHA256: strings.Repeat("c", 64), Size: 12346},
		},
	}
}

func strPtr(s string) *string { return &s }

func signManifest(t *testing.T, m *Manifest, key ed25519.PrivateKey, now time.Time) []byte {
	t.Helper()
	raw, err := Sign(m, key, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return raw
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	raw := signManifest(t, testManifest(now), priv, now)
	m, err := VerifyEnvelope(raw, pub, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if m.Version != "0.6.20" || m.Channel != "stable" || m.Seq != 2 {
		t.Fatalf("decoded manifest mismatch: %+v", m)
	}
	if m.Artifacts["amd64"].SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("artifact digest mismatch")
	}
}

func TestTamperedPayloadRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	raw := signManifest(t, testManifest(now), priv, now)
	var envelope Envelope
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("decode envelope")
	}
	payload := strings.Replace(string(envelope.Payload), "0.6.20", "9.9.99", 1)
	if payload == string(envelope.Payload) {
		t.Fatal("tamper did not apply")
	}
	tampered, _ := json.Marshal(Envelope{Format: 1, Payload: []byte(payload), Signature: envelope.Signature})
	if _, err := VerifyEnvelope(tampered, pub, now); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestWrongKeyRefused(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	raw := signManifest(t, testManifest(now), priv, now)
	if _, err := VerifyEnvelope(raw, otherPub, now); err == nil {
		t.Fatal("signature of another key accepted")
	}
}

func TestExpiredManifestRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	released := time.Now().Add(-8 * 24 * time.Hour)
	m := testManifest(released)
	m.ReleasedAt = released.UTC().Format(time.RFC3339)
	m.ExpiresAt = released.UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
	raw, err := Sign(m, priv, released)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := VerifyEnvelope(raw, pub, time.Now()); err == nil {
		t.Fatal("expired manifest accepted")
	}
}

func TestFutureReleaseRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	m := testManifest(now)
	m.ReleasedAt = now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	m.ExpiresAt = now.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	// Sign refuses future-dated input outright; build the envelope directly
	// so the verifier's own clock check is what gets exercised.
	payload, err := MarshalPayload(m)
	if err != nil {
		t.Fatal("marshal")
	}
	sig := ed25519.Sign(priv, append([]byte(SignatureDomain), payload...))
	raw, _ := json.Marshal(Envelope{Format: 1, Payload: payload, Signature: sig})
	if _, err := VerifyEnvelope(raw, pub, now); err == nil {
		t.Fatal("future-dated manifest accepted")
	}
}

func TestOversizedValidityRefused(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	m := testManifest(now)
	m.ExpiresAt = now.Add(8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("validity beyond the freeze window accepted at signing")
	}
}

func TestChainShapeEnforced(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()

	m := testManifest(now)
	m.Seq = 1
	m.Previous = nil
	if _, err := Sign(m, priv, now); err != nil {
		t.Fatalf("bootstrap manifest rejected: %v", err)
	}

	m = testManifest(now)
	m.Seq = 1
	m.Previous = strPtr(strings.Repeat("a", 64))
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("bootstrap with predecessor accepted")
	}

	m = testManifest(now)
	m.Seq = 3
	m.Previous = nil
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("chained manifest without predecessor accepted")
	}

	m = testManifest(now)
	m.Previous = strPtr("not-a-digest")
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("malformed predecessor digest accepted")
	}
}

func TestArtifactShapeEnforced(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()

	m := testManifest(now)
	m.Artifacts["amd64"] = Artifact{Name: "mismatched-name", SHA256: strings.Repeat("b", 64), Size: 10}
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("artifact name/arch mismatch accepted")
	}

	m = testManifest(now)
	art := m.Artifacts["amd64"]
	art.Size = 0
	m.Artifacts["amd64"] = art
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("empty artifact accepted")
	}

	m = testManifest(now)
	art = m.Artifacts["amd64"]
	art.SHA256 = strings.ToUpper(strings.Repeat("b", 64))
	m.Artifacts["amd64"] = art
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("uppercase digest accepted")
	}

	m = testManifest(now)
	m.Channel = " nightly "
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("unknown channel accepted")
	}

	m = testManifest(now)
	m.Version = "0.6"
	if _, err := Sign(m, priv, now); err == nil {
		t.Fatal("malformed version accepted")
	}
}

func TestUnknownFieldsRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	m := testManifest(now)
	payload, err := MarshalPayload(m)
	if err != nil {
		t.Fatal("marshal")
	}
	extended := bytes.Replace(payload, []byte(`{`), []byte(`{"surprise":1,`), 1)
	sig := ed25519.Sign(priv, append([]byte(SignatureDomain), extended...))
	raw, _ := json.Marshal(Envelope{Format: 1, Payload: extended, Signature: sig})
	if _, err := VerifyEnvelope(raw, pub, now); err == nil {
		t.Fatal("unknown payload field accepted")
	}
}

func TestTrailingDataRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	raw := signManifest(t, testManifest(now), priv, now)
	if _, err := VerifyEnvelope(append(raw, []byte("\n{}\n")...), pub, now); err == nil {
		t.Fatal("trailing envelope data accepted")
	}
}

func TestEnvelopeDigestStable(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	raw := signManifest(t, testManifest(now), priv, now)
	d1 := EnvelopeSHA256(raw)
	d2 := EnvelopeSHA256(raw)
	if d1 != d2 || len(d1) != 64 {
		t.Fatal("envelope digest unstable")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.6.9", "0.6.19", -1},
		{"0.6.19", "0.6.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.6.20", "0.6.20", 0},
		{"0.7.0", "0.10.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Fatalf("CompareVersions(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestTrustedKeyOverride(t *testing.T) {
	if _, err := TrustedKey(); err != nil {
		t.Fatalf("pinned key unusable: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	t.Setenv(TrustedKeyEnv, base64.StdEncoding.EncodeToString(pub))
	got, err := TrustedKey()
	if err != nil || !bytes.Equal(got, pub) {
		t.Fatal("environment override ignored")
	}
	t.Setenv(TrustedKeyEnv, "not-base64!!")
	if _, err := TrustedKey(); err == nil {
		t.Fatal("malformed override accepted")
	}
}
