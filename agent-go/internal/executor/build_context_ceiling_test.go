package executor

// The REAL agent path that downloads a custom deploy's build context through sdk-go GetRaw.
// The public docs promise tarball build contexts up to 100 MB; this one is a legitimate 10 MiB (incompressible) tar.gz
// with the right sha256 and size. It must be fetched and extracted; the 4 MiB GetRaw ceiling alone would refuse it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestBuildContextAboveTheGetRawCeilingStillDownloads(t *testing.T) {
	payload := make([]byte, 10<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len("FROM scratch\n"))})
	_, _ = tw.Write([]byte("FROM scratch\n"))
	_ = tw.WriteHeader(&tar.Header{Name: "blob.bin", Mode: 0o644, Size: int64(len(payload))})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	tarball := buf.Bytes()
	sum := sha256.Sum256(tarball)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	cli, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_bcceil", AgentSecret: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	bc := &sdkclient.BuildContext{URL: "/v1/agent/build-context/ceiling", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(tarball))}
	if err := fetchAndExtractBuildContext(context.Background(), cli, slog.Default(), bc, dest, "", "dpl_bcceil"); err != nil {
		t.Fatalf("a %d-byte build context (docs allow 100 MB) must still download: %v", len(tarball), err)
	}
	if st, err := os.Stat(filepath.Join(dest, "blob.bin")); err != nil || st.Size() != int64(len(payload)) {
		t.Fatalf("the build context was not extracted: %v", err)
	}
}

// A context larger than the size the control plane declared is refused at that size, not read to the end.
func TestBuildContextLargerThanDeclaredIsRefusedAtTheDeclaredSize(t *testing.T) {
	big := bytes.Repeat([]byte("y"), 6<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) }))
	defer srv.Close()
	cli, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_bcceild", AgentSecret: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	bc := &sdkclient.BuildContext{URL: "/v1/agent/build-context/ceiling-declared", SHA256: "00", SizeBytes: 1 << 20}
	err = fetchAndExtractBuildContext(context.Background(), cli, slog.Default(), bc, t.TempDir(), "", "dpl_bcceild")
	if err == nil || !strings.Contains(err.Error(), "1048576-byte ceiling") {
		t.Fatalf("a body above the declared size must stop at that size, got: %v", err)
	}
}
