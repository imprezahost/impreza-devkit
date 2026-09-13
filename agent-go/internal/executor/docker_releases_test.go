package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPinReleaseCompose(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	raw := []byte(`{"name":"app","services":{"web":{"image":"mutable:latest","build":{"context":"."},"pull_policy":"always","environment":{"SECRET":"literal$$NOT_A_VAR"},"volumes":["data:/data"]}},"volumes":{"data":{}}}`)
	pinned, err := pinReleaseCompose(raw, map[string]string{"web": id})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(pinned, &got); err != nil {
		t.Fatal(err)
	}
	web := got["services"].(map[string]any)["web"].(map[string]any)
	if web["image"] != id || web["pull_policy"] != "never" {
		t.Fatalf("mutable runtime: %s", pinned)
	}
	if _, ok := web["build"]; ok {
		t.Fatal("recovery can rebuild")
	}
	if web["environment"].(map[string]any)["SECRET"] != "literal$$NOT_A_VAR" {
		t.Fatal("literal dollars are not protected")
	}
	if _, ok := got["volumes"]; !ok {
		t.Fatal("lost named volumes")
	}
	for _, images := range []map[string]string{nil, {"web": "latest"}, {"web": "sha256:" + strings.Repeat("z", 64)}} {
		if _, err := pinReleaseCompose(raw, images); err == nil {
			t.Fatal("accepted unknown image identity")
		}
	}
}
