package executor

// A git clone that fails must leave the previous build-ctx
// byte-identical. The clone runs in a sibling .clone staging directory;
// only a clone AND pinned-commit checkout that both succeed is renamed
// into place. On the pre-fix code the destDir was wiped before the
// clone ran, so a failed clone (revoked credential, network, forge state) left
// the deployment without the context that built the running version.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func dirDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(h, "%s:%d:", filepath.ToSlash(rel), info.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// A failing clone (server refuses) leaves the previous build-ctx
// byte-identical and no staging leftovers behind. On the pre-fix code
// the wipe happened before the clone, so this test fails there.
func TestFailedClonePreservesBuildContext(t *testing.T) {
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := d.appDir("dpl_c10ea00000000c0d")
	ctxDir := filepath.Join(app, "build-ctx")
	if err := os.MkdirAll(ctxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM busybox:1.37.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirDigest(t, ctxDir)

	// An https URL that passes validation but refuses every connection:
	// the clone itself fails, which is the failure this guards (network, revoked
	// credential, the forge's state) — after validation, where the
	// pre-fix code had already wiped the context.
	err := gitCloneIntoBuildContext(context.Background(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		&sdkclient.BuildContextGit{URL: "https://127.0.0.1:1/repo.git", Ref: "main", CommitSHA: "0123456789abcdef0123456789abcdef01234567"},
		ctxDir, "none", "dpl_c10ea00000000c0d")
	if err == nil {
		t.Fatal("a refused clone must fail")
	}
	if after := dirDigest(t, ctxDir); after != before {
		t.Fatalf("MUTATION CATCH: the failed clone changed the previous build context (%s -> %s)", before, after)
	}
	// No staging leftovers in the app dir.
	entries, _ := os.ReadDir(app)
	for _, e := range entries {
		if e.Name() == "build-ctx.clone" {
			t.Fatal("the staging directory outlived the failed clone")
		}
	}
}
