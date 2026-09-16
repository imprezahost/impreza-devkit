package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManualReleaseContract(t *testing.T) {
	base := []byte(`{"name":"app","services":{"web":{"image":"old"}}}`)
	old, err := releaseContract(base)
	if err != nil {
		t.Fatal(err)
	}
	allowed, _ := releaseContract([]byte(`{"name":"app","services":{"web":{"image":"new","command":["run"],"environment":{"VERSION":"new"}}}}`))
	if !reflect.DeepEqual(old, allowed) {
		t.Fatal("application-only changes incorrectly change routing contract")
	}
	for _, field := range []string{"ports", "volumes", "networks", "network_mode", "secrets", "configs"} {
		var model map[string]any
		json.Unmarshal(base, &model)
		model["services"].(map[string]any)["web"].(map[string]any)[field] = "changed"
		raw, _ := json.Marshal(model)
		next, _ := releaseContract(raw)
		if reflect.DeepEqual(old, next) {
			t.Fatalf("missed %s", field)
		}
	}
	for _, key := range []string{"DOMAIN", "DOMAIN_URL", "HOST_PORT"} {
		raw, _ := json.Marshal(map[string]any{"name": "app", "services": map[string]any{"web": map[string]any{"environment": map[string]any{key: "changed"}}}})
		next, _ := releaseContract(raw)
		if reflect.DeepEqual(old, next) {
			t.Fatalf("missed %s", key)
		}
	}
}
func TestManualReleaseSnapshotValidation(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "releases"), 0700)
	for _, id := range []string{"../escape", "rel_../escape", "rel_missing", "rel_test\n", ""} {
		if _, err := loadRelease(dir, id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	path := filepath.Join(dir, "releases", "rel_test.json")
	for _, raw := range []string{"corrupt", `{"Metadata":{"id":"rel_other"}}`, `{}`} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, err := loadRelease(dir, "rel_test"); err == nil {
			t.Fatal("accepted corrupt snapshot")
		}
	}
	os.Remove(path)
	os.Mkdir(path, 0700)
	if _, err := loadRelease(dir, "rel_test"); err == nil {
		t.Fatal("accepted directory")
	}
}

func TestManualReleaseVersionedConfigContract(t *testing.T) {
	dir := t.TempDir()
	makeContract := func(hash, name string) map[string]any {
		p := filepath.Join(dir, "source-files", hash, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(hash), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"services": map[string]any{"web": map[string]any{"configs": []string{"settings"}}}, "configs": map[string]any{"settings": map[string]any{"file": filepath.ToSlash(p)}}})
		contract, err := releaseSourceContract(raw, dir)
		if err != nil {
			t.Fatal(err)
		}
		return contract
	}
	first := makeContract(strings.Repeat("a", 64), "settings.conf")
	second := makeContract(strings.Repeat("b", 64), "settings.conf")
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same config at a retained version refused")
	}
	different := makeContract(strings.Repeat("c", 64), "other.conf")
	if reflect.DeepEqual(first, different) {
		t.Fatal("different config file allowed")
	}
	raw, _ := json.Marshal(map[string]any{"services": map[string]any{"web": map[string]any{}}, "configs": map[string]any{"settings": map[string]any{"file": filepath.ToSlash(filepath.Join(dir, "source-files", strings.Repeat("d", 64), "missing"))}}})
	if _, err := releaseSourceContract(raw, dir); err == nil {
		t.Fatal("missing retained source accepted")
	}
}
