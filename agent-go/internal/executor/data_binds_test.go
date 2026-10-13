package executor

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Which compose bind sources the agent prepares under ./data.
func TestDataBindSourcesOnlyRelativeBindsInsideData(t *testing.T) {
	app := filepath.Join(t.TempDir(), "apps", "dpl_databinds")
	compose := `services:
  app:
    image: busybox
    volumes:
      - ./data/state:/state
      - ./data/workspace:/ws:rw
      - ./data/state:/again
      - ./data:/all
      - ./other:/other
      - ./data/../etc:/etc-escape
      - ../outside:/up
      - /etc:/host-etc:ro
      - named_vol:/named
      - ${HOME}/x:/interp
      - ./data/${SUB}:/interp2
      - type: bind
        source: ./data/auth
        target: /auth
      - type: volume
        source: ./data/not-a-bind
        target: /v
  side:
    image: busybox
    volumes:
      - ./data/deep/er/dir:/deep
volumes:
  named_vol: {}
`
	got, err := dataBindSources(app, compose)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(app, "data")
	want := []string{
		filepath.Join(data, "auth"),
		filepath.Join(data, "deep", "er", "dir"),
		filepath.Join(data, "state"),
		filepath.Join(data, "workspace"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bind sources:\n got %v\nwant %v", got, want)
	}
	if _, err := dataBindSources(app, "services: [unclosed"); err == nil {
		t.Fatal("a compose that does not parse must be an error, not an empty list")
	}
}

func TestPrepareDataBindsCreatesOnlyUnderData(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	data := filepath.Join(app, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	// an existing file bind is left as a file
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcs := []string{filepath.Join(data, "state"), filepath.Join(data, "deep", "er"), filepath.Join(data, "config.json")}
	if err := prepareDataBinds(data, srcs); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"state", filepath.Join("deep", "er")} {
		if fi, err := os.Stat(filepath.Join(data, d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s was not created as a directory", d)
		}
	}
	if fi, err := os.Stat(filepath.Join(data, "config.json")); err != nil || fi.IsDir() {
		t.Fatal("an existing file bind was replaced")
	}
	if err := prepareDataBinds(data, []string{filepath.Join(app, "escape")}); err == nil {
		t.Fatal("a source outside ./data was prepared")
	}
	if _, err := os.Stat(filepath.Join(app, "escape")); err == nil {
		t.Fatal("a directory was created outside ./data")
	}
	if err := prepareDataBinds(data, []string{filepath.Join(data, "config.json", "under-a-file")}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a source under a file was not refused: %v", err)
	}
}

func TestPrepareDataBindsRefusesSymlinkComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows; covered on Linux")
	}
	base := t.TempDir()
	data := filepath.Join(base, "app", "data")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{data, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	err := prepareDataBinds(data, []string{filepath.Join(data, "link", "state")})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a bind source through a planted link was not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "state")); err == nil {
		t.Fatal("the link was followed: a directory appeared outside ./data")
	}
}
