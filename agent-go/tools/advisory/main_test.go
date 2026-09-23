package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCollectorArchiveBoundaries(t *testing.T) {
	const name = "GHSA-35jh-r3h4-6jhm.json"
	const record = `{"id":"GHSA-35jh-r3h4-6jhm","modified":"2026-09-20T00:00:00Z","database_specific":{"github_reviewed":true},"affected":[{"package":{"ecosystem":"npm","name":"fixture"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"1.0.0"}]}]}]}`
	for _, test := range []struct {
		name, member, record string
		bad                  bool
	}{
		{"valid", name, record, false},
		{"traversal", "GHSA-../" + name, record, true},
		{"mismatched-id", name, strings.Replace(record, "35jh", "2345", 1), true},
		{"unreviewed", name, strings.Replace(record, `"github_reviewed":true`, `"github_reviewed":false`, 1), true},
		{"withdrawn", name, strings.Replace(record, `"modified":`, `"withdrawn":"2026-09-21T00:00:00Z","modified":`, 1), true},
		{"malformed", name, "{", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			f, _ := z.Create(test.member)
			f.Write([]byte(test.record))
			z.Close()
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://storage.googleapis.com/osv-vulnerabilities/npm/all.zip" || r.Method != "GET" || r.Body != nil {
					t.Fatal("unexpected source request")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(buf.Bytes())), ContentLength: int64(buf.Len()), Header: http.Header{}}, nil
			})}
			entries, report, err := collectEcosystem(context.Background(), client, "npm")
			if test.bad {
				if err == nil {
					t.Fatal("accepted invalid/empty snapshot")
				}
				return
			}
			if err != nil || len(entries) != 1 || len(report.SHA256) != 64 || report.Records != 1 {
				t.Fatal(entries, report, err)
			}
		})
	}
}

func TestCollectorAndSignerRemainSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.json")
	if run("collect", path, "", "private-key", 1, false) == nil {
		t.Fatal("collection accepted signing key")
	}
	if run("sign", path, "", "", 0, false) == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid operation wrote output")
	}
}
