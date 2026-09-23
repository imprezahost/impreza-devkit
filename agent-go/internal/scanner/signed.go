package scanner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

const SignedBaseName = "advisories.signed.json"
const MaxSignedBytes = MaxBaseBytes*4/3 + 4096
const signatureDomain = "impreza-advisory-base-v1\x00"

// Reviewed production trust anchor, independent of the distribution server.
// A different operator may override it explicitly at build time or in the
// root-owned service environment. No trust is downloaded from the feed.
var ReleaseAdvisoryKey = "HismnHv7rcB/AWSrzalz/+c3t921VQ1gmHVJlNx0gfQ="

// ValidateAdvisoryCandidate checks an unsigned collector output without exposing
// a signing key to collection/review. It does not authenticate its provenance.
func ValidateAdvisoryCandidate(raw []byte, now time.Time) (*AdvisoryBase, error) {
	if len(raw) > MaxBaseBytes {
		return nil, errors.New("advisory payload too large")
	}
	var b AdvisoryBase
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF || !validSignedBase(&b, now) {
		return nil, errors.New("invalid advisory candidate")
	}
	expires, _ := time.Parse(time.RFC3339, b.ExpiresAt)
	if !expires.After(now) {
		return nil, errors.New("advisory candidate expired")
	}
	return &b, nil
}

func TrustedAdvisoryKey() (ed25519.PublicKey, error) {
	key := ReleaseAdvisoryKey
	if override := os.Getenv("IMPREZA_ADVISORY_PUBLIC_KEY"); override != "" {
		key = override
	}
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("advisory trust key unavailable")
	}
	return ed25519.PublicKey(b), nil
}

type signedBase struct {
	Format    int    `json:"format"`
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

func validSignedBase(b *AdvisoryBase, now time.Time) bool {
	if !validBase(b) || b.Schema != 2 || b.Revision == 0 || len(b.Entries) == 0 || len(b.Attribution) != 2 {
		return false
	}
	if b.Attribution[0] != GitHubAttribution || b.Attribution[1] != GoAttribution {
		return false
	}
	generated, e1 := time.Parse(time.RFC3339, b.GeneratedAt)
	expires, e2 := time.Parse(time.RFC3339, b.ExpiresAt)
	if e1 != nil || e2 != nil || generated.After(now.Add(5*time.Minute)) || !expires.After(generated) || expires.Sub(generated) > 7*24*time.Hour {
		return false
	}
	for _, e := range b.Entries {
		if e.Source != "github-reviewed" && e.Source != "go-vulndb" {
			return false
		}
	}
	return true
}

func SignAdvisoryBase(b *AdvisoryBase, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || !validSignedBase(b, time.Now()) {
		return nil, errors.New("invalid signing input")
	}
	payload, err := json.Marshal(b)
	if err != nil || len(payload) > MaxBaseBytes {
		return nil, errors.New("advisory payload too large")
	}
	sig := ed25519.Sign(key, append([]byte(signatureDomain), payload...))
	return json.Marshal(signedBase{Format: 1, Payload: payload, Signature: sig})
}

func VerifyAdvisoryBase(raw []byte, key ed25519.PublicKey, now time.Time) (*AdvisoryBase, error) {
	if len(raw) > MaxSignedBytes || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid signed advisory size or key")
	}
	var envelope signedBase
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&envelope) != nil || envelope.Format != 1 || len(envelope.Payload) > MaxBaseBytes || len(envelope.Signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid advisory envelope")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing advisory data")
	}
	if !ed25519.Verify(key, append([]byte(signatureDomain), envelope.Payload...), envelope.Signature) {
		return nil, errors.New("invalid advisory signature")
	}
	var b AdvisoryBase
	if json.Unmarshal(envelope.Payload, &b) != nil || !validSignedBase(&b, now) {
		return nil, errors.New("invalid advisory schema")
	}
	return &b, nil
}

func LoadAdvisoryBase(stateDir string) *AdvisoryBase {
	key, err := TrustedAdvisoryKey()
	if err != nil {
		return nil
	}
	raw, err := readBaseFile(stateDir, SignedBaseName, MaxSignedBytes)
	if err != nil {
		return nil
	}
	b, err := VerifyAdvisoryBase(raw, key, time.Now())
	if err != nil {
		return nil
	}
	return b
}

// InstallAdvisoryBase serializes processes, verifies before writing, and rejects
// rollback/equivocation. A corrupt existing file requires operator investigation;
// it must not reset the trusted revision silently. No unsigned fallback exists.
func InstallAdvisoryBase(stateDir string, raw []byte, key ed25519.PublicKey, now time.Time) error {
	b, err := VerifyAdvisoryBase(raw, key, now)
	if err != nil {
		return err
	}
	expires, _ := time.Parse(time.RFC3339, b.ExpiresAt)
	if !expires.After(now) {
		return errors.New("advisory update expired")
	}
	r, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer r.Close()
	lock, err := r.OpenFile("advisories.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		lock, err = openNoFollow(r, "advisories.lock")
	}
	if err != nil {
		return errors.New("advisory lock unavailable")
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("invalid advisory lock")
	}
	if err = lockAdvisories(lock); err != nil {
		return errors.New("advisory update already running")
	}
	old, err := readBaseFile(stateDir, SignedBaseName, MaxSignedBytes)
	if err == nil {
		previous, e := VerifyAdvisoryBase(old, key, now)
		if e != nil {
			return errors.New("existing advisory database is invalid")
		}
		previousTime, _ := time.Parse(time.RFC3339, previous.GeneratedAt)
		newTime, _ := time.Parse(time.RFC3339, b.GeneratedAt)
		if previous.Revision > b.Revision || previousTime.After(newTime) {
			return errors.New("advisory rollback refused")
		}
		if previous.Revision == b.Revision {
			var a, z signedBase
			_ = json.Unmarshal(old, &a)
			_ = json.Unmarshal(raw, &z)
			if !bytes.Equal(a.Payload, z.Payload) {
				return errors.New("advisory revision conflict")
			}
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("existing advisory database unreadable")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".advisories-" + hex.EncodeToString(nonce[:])
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(name)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = r.Rename(name, SignedBaseName); err != nil {
		return err
	}
	return syncAdvisoryDirectory(r)
}
