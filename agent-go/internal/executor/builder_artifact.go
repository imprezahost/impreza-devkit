package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const ownedBuilderArtifactURL = "https://github.com/imprezahost/agent-public/releases/download/builder-v0.33.0-1/builder-linux-amd64.tar.gz"
const ownedBuilderArtifactSHA256 = "269034e5e619b4775d4e3899b66ccf440f6d2d615a7261c458664b557986d6ff"

func builderArtifactDestination(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		(u.Hostname() == "github.com" || u.Hostname() == "release-assets.githubusercontent.com")
}

// Artifact transport carries no API or agent credentials. Redirects are limited
// to GitHub's release service; the compiled checksum is required before Docker
// sees any bytes. The temporary file is never a caller-selected output path.
func downloadBuilderArtifact(ctx context.Context, dir, address, expected string) (string, error) {
	return downloadBuilderArtifactWithTransport(ctx, dir, address, expected, nil)
}

func downloadBuilderArtifactWithTransport(ctx context.Context, dir, address, expected string, transport http.RoundTripper) (string, error) {
	u, err := url.Parse(address)
	if err != nil || !builderArtifactDestination(u) || !workHashPattern.MatchString(expected) {
		return "", errors.New("invalid builder artifact destination or checksum")
	}
	if err = realWorkDirectory(dir); err != nil {
		return "", err
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !builderArtifactDestination(req.URL) {
			return errors.New("unexpected builder artifact redirect")
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	const limit int64 = 256 * 1024 * 1024
	if response.StatusCode != http.StatusOK || response.ContentLength > limit {
		return "", errors.New("builder artifact response refused")
	}
	f, err := os.CreateTemp(dir, "builder-download-*.tar.gz")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0600); err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", err
	}
	if n == 0 || n > limit || hex.EncodeToString(hash.Sum(nil)) != expected {
		return "", errors.New("builder artifact checksum or size mismatch")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	keep = true
	return f.Name(), nil
}
