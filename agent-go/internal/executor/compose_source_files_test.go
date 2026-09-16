package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestComposeSourceFilesImmutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "build-ctx", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "build-ctx", "config", "app.conf")
	if err := os.WriteFile(input, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &sdkclient.BuildContext{SHA256: strings.Repeat("a", 64), AuxiliaryProtocol: composeSourceProtocol, AuxiliaryFiles: []string{"config/app.conf"}}
	if err := stageComposeSourceFiles(dir, b); err != nil {
		t.Fatal(err)
	}
	if err := stageComposeSourceFiles(dir, b); err != nil {
		t.Fatal("idempotent", err)
	}
	if err := os.WriteFile(input, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageComposeSourceFiles(dir, b); err == nil {
		t.Fatal("replaced immutable contents")
	}
	b.SHA256 = strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("file: ./source-files/"+strings.Repeat("a", 64)+"/config/app.conf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageComposeSourceFiles(dir, b); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(dir, "source-files", strings.Repeat("a", 64), "config", "app.conf"))
	if err != nil || string(old) != "old" {
		t.Fatal("previous release source changed", err)
	}
}

func TestComposeSourceFilesRejectUnsafe(t *testing.T) {
	for _, name := range []string{"../escape", "/etc/passwd", "config/../../escape", "config\\escape", ".git/config", "node_modules/file", "${FILE}"} {
		t.Run(name, func(t *testing.T) {
			b := &sdkclient.BuildContext{SHA256: strings.Repeat("a", 64), AuxiliaryProtocol: composeSourceProtocol, AuxiliaryFiles: []string{name}}
			if stageComposeSourceFiles(t.TempDir(), b) == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "build-ctx"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build-ctx", "large"), make([]byte, composeSourceMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &sdkclient.BuildContext{SHA256: strings.Repeat("a", 64), AuxiliaryProtocol: composeSourceProtocol, AuxiliaryFiles: []string{"large"}}
	if stageComposeSourceFiles(dir, b) == nil {
		t.Fatal("oversized file accepted")
	}
	b.AuxiliaryProtocol = "future"
	if stageComposeSourceFiles(dir, b) == nil {
		t.Fatal("unknown protocol accepted")
	}
}

func TestComposeSourceCollectionPreservesReferences(t *testing.T) {
	dir := t.TempDir()
	for _, letter := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(dir, "source-files", strings.Repeat(letter, 64)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("file: ./source-files/"+strings.Repeat("a", 64)+"/current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(dir, "releases", "rel_fixture.json")
	image := "sha256:" + strings.Repeat("e", 64)
	model := runtimeRelease{Metadata: sdkclient.DeploymentRelease{ID: "rel_fixture", ImageIDs: map[string]string{"web": image}}, Compose: json.RawMessage(`{"services":{"web":{"image":"` + image + `"}},"configs":{"file":"/state/source-files/` + strings.Repeat("b", 64) + `/old"}}`)}
	encoded, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collectComposeSourceFiles(dir, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	for _, letter := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(dir, "source-files", strings.Repeat(letter, 64))); err != nil {
			t.Fatal("referenced snapshot removed", letter, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "source-files", strings.Repeat("d", 64))); !os.IsNotExist(err) {
		t.Fatal("unused snapshot retained")
	}
	if err := os.WriteFile(release, []byte(`{"truncated":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collectComposeSourceFiles(dir, strings.Repeat("c", 64)); err == nil {
		t.Fatal("invalid release allowed cleanup")
	}
	if _, err := os.Stat(filepath.Join(dir, "source-files", strings.Repeat("b", 64))); err != nil {
		t.Fatal("uncertain release snapshot removed")
	}
}
