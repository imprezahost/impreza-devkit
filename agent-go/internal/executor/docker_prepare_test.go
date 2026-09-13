package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareDeployImagesStopsBeforeReplacement(t *testing.T) {
	want := [][]string{{"config", "--quiet"}, {"pull", "--ignore-buildable"}, {"build"}}
	for _, failAt := range []int{-1, 0, 1, 2} {
		t.Run(string(rune('a'+failAt+1)), func(t *testing.T) {
			var calls [][]string
			err := prepareDeployImages(context.Background(), func(ctx context.Context, args ...string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("preparation has no deadline")
				}
				calls = append(calls, args)
				if len(calls)-1 == failAt {
					return []byte("diagnostic"), errors.New("intentional failure")
				}
				return nil, nil
			})
			n := len(want)
			if failAt >= 0 {
				n = failAt + 1
				if err == nil || !strings.Contains(err.Error(), "diagnostic") {
					t.Fatalf("missing failure diagnostic: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, want[:n]) {
				t.Fatalf("unsafe preparation commands: %v", calls)
			}
		})
	}
}

func TestRestoreDeployConfig(t *testing.T) {
	for _, envExists := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing_env", false: "absent_env"}[envExists], func(t *testing.T) {
			dir := t.TempDir()
			original := []byte("services: original\n")
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), original, 0644); err != nil {
				t.Fatal(err)
			}
			if envExists {
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("VERSION=old\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := captureDeployConfig(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"compose.yaml", ".env"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := snapshot.restore(dir); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
			if err != nil || string(got) != string(original) {
				t.Fatalf("compose not restored: %q %v", got, err)
			}
			got, err = os.ReadFile(filepath.Join(dir, ".env"))
			if envExists {
				if err != nil || string(got) != "VERSION=old\n" {
					t.Fatalf("env not restored: %q %v", got, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new env not removed: %v", err)
			}
		})
	}
}

func TestCaptureDeployConfigRejectsUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "compose.yaml"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := captureDeployConfig(dir, true); err == nil {
		t.Fatal("accepted a directory as compose.yaml")
	}
}

func TestRestoreDeployConfigReportsFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env", "keep"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := deployConfigSnapshot{{name: ".env"}}
	if err := snapshot.restore(dir); err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("missing restore error: %v", err)
	}
}
