package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

type builderArtifactTransport func(*http.Request) (*http.Response, error)

func (f builderArtifactTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuilderArtifactDestination(t *testing.T) {
	for _, address := range []string{"https://github.com/a", "https://release-assets.githubusercontent.com/a", "http://github.com/a", "https://github.com.evil.test/a", "https://user@github.com/a", "https://github.com:443/a", "https://127.0.0.1/a"} {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		want := address == "https://github.com/a" || address == "https://release-assets.githubusercontent.com/a"
		if builderArtifactDestination(u) != want {
			t.Fatal(address)
		}
	}
}

func TestBuilderArtifactDownloadFailsClosed(t *testing.T) {
	for _, mode := range []string{"valid", "checksum", "empty", "oversized", "status", "foreign redirect", "http redirect", "loop", "release redirect"} {
		t.Run(mode, func(t *testing.T) {
			dir, _, _ := builderStoreFixture(t)
			payload := "verified archive fixture"
			digest := sha256.Sum256([]byte(payload))
			calls := 0
			transport := builderArtifactTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if !builderArtifactDestination(r.URL) || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("unsafe request", r.URL)
				}
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: r}
				switch mode {
				case "checksum":
					response.Body = io.NopCloser(strings.NewReader("changed archive"))
				case "empty":
					response.Body = io.NopCloser(strings.NewReader(""))
				case "oversized":
					response.ContentLength = 256*1024*1024 + 1
				case "status":
					response.StatusCode = 404
				case "foreign redirect", "http redirect", "loop", "release redirect":
					if mode != "release redirect" || calls == 1 {
						response.StatusCode = 302
						dest := "https://github.com/loop"
						if mode == "foreign redirect" {
							dest = "https://evil.test/archive"
						}
						if mode == "http redirect" {
							dest = "http://github.com/archive"
						}
						if mode == "release redirect" {
							dest = "https://release-assets.githubusercontent.com/archive"
						}
						response.Header.Set("Location", dest)
					}
				}
				return response, nil
			})
			path, err := downloadBuilderArtifactWithTransport(context.Background(), dir, "https://github.com/archive", hex.EncodeToString(digest[:]), transport)
			valid := mode == "valid" || mode == "release redirect"
			if (err == nil) != valid {
				t.Fatalf("%s: %v", mode, err)
			}
			if valid {
				raw, readErr := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if readErr != nil || statErr != nil || string(raw) != payload || info.Mode().Perm() != 0600 {
					t.Fatal("invalid artifact")
				}
				os.Remove(path)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("download residue", entries, err)
			}
			if (mode == "foreign redirect" || mode == "http redirect") && calls != 1 {
				t.Fatal("followed unsafe redirect")
			}
		})
	}
}
