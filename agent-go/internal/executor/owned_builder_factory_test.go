package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedBuilderPinnedImage(t *testing.T) {
	for _, kind := range []string{"valid", "config", "tag", "platform", "digest", "id", "duplicate", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			row := map[string]any{"Id": ownedBuilderImage, "Os": "linux", "Architecture": "amd64", "RepoDigests": []string{ownedBuilderImage}}
			switch kind {
			case "config":
				row["Id"] = ownedBuilderConfigImage
			case "tag":
				row["Id"] = "moby/buildkit:rootless"
			case "platform":
				row["Architecture"] = "arm64"
			case "digest":
				row["Id"] = "sha256:" + strings.Repeat("4", 64)
			case "id":
				row["Id"] = "short"
			}
			rows := []any{row}
			if kind == "duplicate" {
				rows = append(rows, row)
			}
			raw, _ := json.Marshal(rows)
			if kind == "malformed" {
				raw = []byte("{")
			}
			_, err := verifyOwnedBuilderImage(raw)
			if (err == nil) != (kind == "valid" || kind == "config") {
				t.Fatalf("image decision %s: %v", kind, err)
			}
		})
	}
}

func TestOwnedBuilderFactoryPersistsBeforeMutationAndNeverRetries(t *testing.T) {
	for _, kind := range []string{"success", "profile failure", "lost create response", "invalid cid", "foreign inspect", "invalid volume", "inspect failure", "lost start response", "missing receipt", "different receipt"} {
		t.Run(kind, func(t *testing.T) {
			dir, r, b := builderStoreFixture(t)
			r.Profile = "planned"
			_, _, row := ownedBuilderFixture()
			created, started, profiles := 0, 0, 0
			profile := func(_ context.Context, dir string, current *ownedBuilderRecord, create bool) error {
				profiles++
				onDisk, err := loadOwnedBuilderRecord(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID)
				if err != nil || onDisk.Phase != "intent" || !create {
					t.Fatal("mutation before durable intent", err)
				}
				if kind == "profile failure" {
					return errors.New("ambiguous profile")
				}
				current.Profile = "loaded"
				return writeWorkJSON(dir, "builder.json", current)
			}
			run := func(_ context.Context, args ...string) ([]byte, error) {
				switch strings.Join(args[:2], " ") {
				case "container create":
					created++
					onDisk, err := loadOwnedBuilderRecord(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID)
					if err != nil || onDisk.Profile != "loaded" || onDisk.Identity != nil {
						t.Fatal("creation before durable profile", err)
					}
					if args[len(args)-1] != r.ImageID || strings.Contains(strings.Join(args, " "), "--privileged") {
						t.Fatal("unsafe create args", args)
					}
					if len(args) < 4 || args[2] != "--cidfile" || args[3] != filepath.Join(dir, "builder.cid") || !onDisk.CIDFile {
						t.Fatal("missing durable receipt contract", args)
					}
					if kind != "missing receipt" {
						cid := b.ContainerID
						if kind == "different receipt" {
							cid = strings.Repeat("f", 64)
						}
						if err := os.WriteFile(args[3], []byte(cid), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "lost create response" {
						return nil, errors.New("timeout after creation")
					}
					if kind == "invalid cid" {
						return []byte("short"), nil
					}
					return []byte(b.ContainerID + "\n"), nil
				case "container inspect":
					if kind == "inspect failure" {
						return nil, errors.New("inspect uncertain")
					}
					if kind == "foreign inspect" {
						row["Name"] = "/foreign"
					}
					if kind == "invalid volume" {
						row["Mounts"].([]map[string]string)[0]["Name"] = "named-volume"
					}
					return json.Marshal([]any{row})
				case "container start":
					started++
					onDisk, err := loadOwnedBuilderRecord(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID)
					if err != nil || onDisk.Phase != "bound" || onDisk.Identity.ContainerID != b.ContainerID {
						t.Fatal("start before binding", err)
					}
					if err = withOwnedBuilderLock(dir, func() error { t.Fatal("cancellation acquired start lock"); return nil }); err == nil {
						t.Fatal("start lock not held")
					}
					if kind == "lost start response" {
						return nil, errors.New("start uncertain")
					}
					return []byte(b.ContainerID), nil
				case "container exec":
					return []byte("worker"), nil
				default:
					t.Fatal("unexpected command", args)
					return nil, errors.New("unexpected")
				}
			}
			_, err := createOwnedBuilder(context.Background(), dir, r, run, profile)
			if (err == nil) != (kind == "success") {
				t.Fatal(kind, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "builder.json"))
			if err != nil {
				t.Fatal(err)
			}
			beforeCreate, beforeStart := created, started
			// Fresh caller with original intent must not restart even after a lost response.
			retry := *r
			retry.Phase = "intent"
			retry.Identity = nil
			retry.Profile = "planned"
			if _, err = createOwnedBuilder(context.Background(), dir, &retry, run, profile); err == nil {
				t.Fatal("factory replay accepted")
			}
			after, _ := os.ReadFile(filepath.Join(dir, "builder.json"))
			if string(data) != string(after) || created != beforeCreate || started != beforeStart || profiles != 1 {
				t.Fatal("replay changed state")
			}
		})
	}
}

func TestOwnedBuilderImageStoreFallback(t *testing.T) {
	for _, kind := range []string{"manifest", "config", "missing", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			raw, err := inspectOwnedBuilderImage(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				expected := ownedBuilderImage
				if calls == 2 {
					expected = ownedBuilderConfigImage
				}
				if strings.Join(args, " ") != "image inspect "+expected || calls > 2 {
					t.Fatal(args)
				}
				if kind == "missing" || (kind == "config" && calls == 1) {
					return nil, errors.New("not found")
				}
				if kind == "foreign" {
					expected = "sha256:" + strings.Repeat("0", 64)
				}
				return json.Marshal([]map[string]string{{"Id": expected, "Os": "linux", "Architecture": "amd64"}})
			})
			if err == nil {
				_, err = verifyOwnedBuilderImage(raw)
			}
			if (err == nil) != (kind == "manifest" || kind == "config") {
				t.Fatal(kind, err)
			}
			if (kind == "manifest" || kind == "foreign") && calls != 1 {
				t.Fatal("unexpected fallback")
			}
		})
	}
}
