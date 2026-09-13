package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
