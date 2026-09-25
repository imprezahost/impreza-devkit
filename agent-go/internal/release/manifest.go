// Package release implements the signed agent release manifest.
//
// A channel directory publishes manifest.signed.json: an Ed25519 envelope
// whose payload names the current release, its artifact digests, a strictly
// increasing sequence number chained to the previous envelope, a short
// expiry and the minimum supported agent version. The format mirrors the
// signed advisory envelope (same envelope shape, different signature domain)
// so both feeds can share custody without cross-protocol forgeries.
//
// Verification is fail-closed: an absent, malformed, expired, rolled-back
// or equivocating manifest must never degrade into trusting unsigned
// metadata for the channels that publish one.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SignatureDomain separates release signatures from every other use of the
// same key, including the advisory feed.
const SignatureDomain = "impreza-agent-release-v1\x00"

const (
	// MaxPayloadBytes bounds the unsigned manifest payload.
	MaxPayloadBytes = 4096
	// MaxEnvelopeBytes bounds the signed envelope (base64 expansion + margin).
	MaxEnvelopeBytes = 8192
	// MaxArtifactBytes bounds a single release binary.
	MaxArtifactBytes = 64 << 20
	// MaxValidity is the longest signing window; expiry is the freeze
	// signal — a channel that stops publishing goes stale on purpose.
	MaxValidity = 7 * 24 * time.Hour
)

// Channels that may publish a manifest. Pinned servers never resolve to a
// channel automatically; it is a control-plane policy state, not a feed.
var Channels = map[string]bool{"stable": true, "beta": true}

// TrustedKeyEnv lets an operator or test override the pinned release key
// without rebuilding, mirroring the advisory trust anchor. No trust is
// ever learned from the feed itself.
const TrustedKeyEnv = "IMPREZA_RELEASE_PUBLIC_KEY"

// ReleaseAdvisoryKey is the production advisory signing key. Agent
// releases are signed under the same custody. Base64 raw Ed25519 public
// key.
var ReleaseAdvisoryKey = "HismnHv7rcB/AWSrzalz/+c3t921VQ1gmHVJlNx0gfQ="

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Artifact describes one release binary.
type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the signed payload naming the head of a channel.
type Manifest struct {
	Schema          int                 `json:"schema"`
	Channel         string              `json:"channel"`
	Version         string              `json:"version"`
	Seq             uint64              `json:"seq"`
	Previous        *string             `json:"previous"`
	ReleasedAt      string              `json:"released_at"`
	ExpiresAt       string              `json:"expires_at"`
	MinAgentVersion string              `json:"min_agent_version"`
	Artifacts       map[string]Artifact `json:"artifacts"`
}

// Envelope is the on-disk signed artifact.
type Envelope struct {
	Format    int    `json:"format"`
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

// CompareVersions orders two strict X.Y.Z versions. Returns -1, 0 or 1;
// malformed inputs compare as equal to nothing (caller validates first).
func CompareVersions(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		ua, _ := strconv.ParseUint(pa[i], 10, 32)
		ub, _ := strconv.ParseUint(pb[i], 10, 32)
		if ua != ub {
			if ua < ub {
				return -1
			}
			return 1
		}
	}
	return 0
}

func validVersion(v string) bool { return versionPattern.MatchString(v) }

// Validate checks manifest semantics against now. It does not authenticate
// provenance; only VerifyEnvelope does, over the exact signed bytes.
func (m *Manifest) Validate(now time.Time) error {
	if m.Schema != 1 {
		return errors.New("unsupported manifest schema")
	}
	if !Channels[m.Channel] {
		return errors.New("unknown release channel")
	}
	if !validVersion(m.Version) || !validVersion(m.MinAgentVersion) {
		return errors.New("malformed release version")
	}
	if m.Seq == 0 {
		return errors.New("manifest sequence must be positive")
	}
	if m.Seq == 1 && m.Previous != nil {
		return errors.New("bootstrap manifest cannot chain a predecessor")
	}
	if m.Seq > 1 && (m.Previous == nil || !sha256Pattern.MatchString(*m.Previous)) {
		return errors.New("chained manifest requires predecessor digest")
	}
	released, err1 := time.Parse(time.RFC3339, m.ReleasedAt)
	expires, err2 := time.Parse(time.RFC3339, m.ExpiresAt)
	if err1 != nil || err2 != nil {
		return errors.New("manifest timestamps must be RFC3339")
	}
	if released.After(now.Add(5 * time.Minute)) {
		return errors.New("manifest released in the future")
	}
	if !expires.After(now) {
		return errors.New("manifest expired")
	}
	if !expires.After(released) || expires.Sub(released) > MaxValidity {
		return fmt.Errorf("manifest validity window must be positive and at most %s", MaxValidity)
	}
	if len(m.Artifacts) == 0 {
		return errors.New("manifest lists no artifacts")
	}
	for arch, a := range m.Artifacts {
		if a.Name != "impreza-agent-linux-"+arch {
			return errors.New("artifact name does not match its architecture")
		}
		if !sha256Pattern.MatchString(a.SHA256) {
			return errors.New("artifact digest must be lowercase sha256 hex")
		}
		if a.Size <= 0 || a.Size > MaxArtifactBytes {
			return errors.New("artifact size out of bounds")
		}
	}
	return nil
}

// ParseManifest decodes an unsigned payload with unknown-field rejection.
func ParseManifest(payload []byte) (*Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid manifest payload")
	}
	return &m, nil
}

// MarshalPayload encodes a manifest canonically for signing.
func MarshalPayload(m *Manifest) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil || len(payload) > MaxPayloadBytes {
		return nil, errors.New("manifest payload too large")
	}
	return payload, nil
}

// Sign produces the envelope bytes for a validated manifest.
func Sign(m *Manifest, key ed25519.PrivateKey, now time.Time) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid signing key")
	}
	if err := m.Validate(now); err != nil {
		return nil, err
	}
	payload, err := MarshalPayload(m)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, append([]byte(SignatureDomain), payload...))
	return json.Marshal(Envelope{Format: 1, Payload: payload, Signature: sig})
}

// VerifyEnvelope authenticates envelope bytes against a pinned key and
// validates the decoded manifest against now. This is the single trust
// decision every consumer (updater script, agent, release checks) mirrors.
func VerifyEnvelope(raw []byte, key ed25519.PublicKey, now time.Time) (*Manifest, error) {
	if len(raw) > MaxEnvelopeBytes || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid envelope size or key")
	}
	var envelope Envelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&envelope) != nil || envelope.Format != 1 || len(envelope.Payload) > MaxPayloadBytes || len(envelope.Signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid release envelope")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing release envelope data")
	}
	if !ed25519.Verify(key, append([]byte(SignatureDomain), envelope.Payload...), envelope.Signature) {
		return nil, errors.New("invalid release signature")
	}
	m, err := ParseManifest(envelope.Payload)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(now); err != nil {
		return nil, err
	}
	return m, nil
}

// EnvelopeSHA256 is the digest chained into the next manifest's previous
// field and recorded by verifiers for rollback and equivocation checks.
func EnvelopeSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TrustedKey returns the pinned verification key, honoring the explicit
// environment override used by tests and operator recovery.
func TrustedKey() (ed25519.PublicKey, error) {
	key := ReleaseAdvisoryKey
	if override := os.Getenv(TrustedKeyEnv); override != "" {
		key = override
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("release trust key unavailable")
	}
	return ed25519.PublicKey(raw), nil
}
