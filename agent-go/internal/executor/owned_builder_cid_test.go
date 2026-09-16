package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedBuilderCIDReceiptRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "short", "extra", "mode", "symlink", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir, _, b := builderStoreFixture(t)
			path := filepath.Join(dir, "builder.cid")
			raw := []byte(b.ContainerID)
			if kind == "short" {
				raw = raw[:63]
			}
			if kind == "extra" {
				raw = append(raw, '\n')
			}
			if kind != "missing" && kind != "directory" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "mode":
				os.Chmod(path, 0644)
			case "directory":
				os.Mkdir(path, 0700)
			case "symlink", "hardlink":
				other := filepath.Join(dir, "target")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(other, path)
				} else {
					err = os.Link(other, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			cid, err := readOwnedBuilderCID(dir)
			if (err == nil) != (kind == "valid") || (err == nil && cid != b.ContainerID) {
				t.Fatal(kind, cid, err)
			}
		})
	}
}
func TestOwnedBuilderCIDCandidateRequiresFullIdentity(t *testing.T) {
	for _, kind := range []string{"valid", "foreign cid", "foreign labels", "privileged", "image", "volume", "short cid"} {
		t.Run(kind, func(t *testing.T) {
			_, r, b := builderStoreFixture(t)
			_, _, row := ownedBuilderFixture()
			cid := b.ContainerID
			switch kind {
			case "foreign cid":
				row["Id"] = strings.Repeat("e", 64)
			case "foreign labels":
				row["Config"].(map[string]any)["Labels"] = map[string]string{}
			case "privileged":
				row["HostConfig"].(map[string]any)["Privileged"] = true
			case "image":
				row["Image"] = "sha256:" + strings.Repeat("f", 64)
			case "volume":
				row["Mounts"].([]map[string]string)[0]["Name"] = "foreign-volume"
			case "short cid":
				cid = "short"
			}
			calls := 0
			got, err := inspectOwnedBuilderCandidate(context.Background(), r, cid, func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if strings.Join(args, " ") != "container inspect "+cid {
					t.Fatal("name lookup or mutation", args)
				}
				return json.Marshal([]any{row})
			})
			if (err == nil) != (kind == "valid") || (err == nil && got.ContainerID != cid) {
				t.Fatal(kind, got, err)
			}
			if kind == "short cid" && calls != 0 {
				t.Fatal("invalid CID reached Docker")
			}
		})
	}
}
