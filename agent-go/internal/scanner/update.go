package scanner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/imprezahost/impreza-devkit/sdk-go/tor"
)

const DefaultAdvisoryURL = "https://api.imprezahost.com/releases/advisories/stable.json"

func AdvisoryURL() string {
	if u := os.Getenv("IMPREZA_ADVISORY_URL"); u != "" {
		return u
	}
	return DefaultAdvisoryURL
}

func UpdateAdvisories(ctx context.Context, stateDir string, useTor bool, proxy string) error {
	key, err := TrustedAdvisoryKey()
	if err != nil {
		return err
	}
	if useTor && proxy == "" {
		proxy = tor.DefaultSOCKS
	}
	transport, err := tor.Transport(proxy)
	if err != nil {
		return errors.New("advisory proxy configuration invalid")
	}
	if proxy == "" {
		isolated := http.DefaultTransport.(*http.Transport).Clone()
		isolated.Proxy = nil
		transport = isolated
	}
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("advisory redirect refused") }}
	defer client.CloseIdleConnections()
	return updateAdvisories(ctx, stateDir, AdvisoryURL(), key, client)
}

func updateAdvisories(parent context.Context, stateDir, address string, key ed25519.PublicKey, client *http.Client) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid advisory HTTPS endpoint")
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("advisory request failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "impreza-advisory-updater")
	// This client has no agent credentials, cookies or application inventory.
	response, err := client.Do(request)
	if err != nil {
		return errors.New("advisory download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxSignedBytes {
		return errors.New("advisory download refused")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxSignedBytes+1))
	if err != nil || len(raw) > MaxSignedBytes {
		return errors.New("advisory download exceeds limit or incomplete")
	}
	return InstallAdvisoryBase(stateDir, raw, key, time.Now())
}

func RunAdvisoryUpdates(ctx context.Context, stateDir string, useTor bool, proxy string, log *slog.Logger) {
	if _, err := TrustedAdvisoryKey(); err != nil {
		log.Warn("dependency advisory updates disabled: trust key unavailable")
		return
	}
	for {
		if err := UpdateAdvisories(ctx, stateDir, useTor, proxy); err != nil && ctx.Err() == nil {
			log.Warn("dependency advisory update unconfirmed; continuing with verified cached database", "err", err)
		}
		timer := time.NewTimer(6 * time.Hour)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
