package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveRoutesRequiresVerifiedProxyState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		list      string
		queryErr  bool
		reloadErr bool
		wantErr   bool
		reloads   int
	}{
		{"absent", "", false, false, false, 0},
		{"daemon unavailable", "", true, false, true, 0},
		{"existing proxy", "abc123", false, false, false, 1},
		{"reload failure", "abc123", false, true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			c := &Caddy{StateDir: t.TempDir(), containerList: func(context.Context) ([]byte, error) {
				if tc.queryErr {
					return nil, errors.New("daemon down")
				}
				return []byte(tc.list), nil
			}, switchReload: func(context.Context) error {
				count++
				if tc.reloadErr {
					return errors.New("reload failed")
				}
				return nil
			}}
			if err := c.ensureDirs(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.StateDir, "deployments", "dpl_fixture.caddy")
			if err := os.WriteFile(path, []byte("old.example.test {\n reverse_proxy app:80\n}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := c.RemoveDeploymentRoutes(t.Context(), "dpl_fixture")
			if (err != nil) != tc.wantErr || count != tc.reloads {
				t.Fatalf("unexpected cleanup result: %v, reloads=%d", err, count)
			}
			body, _ := os.ReadFile(filepath.Join(c.StateDir, "Caddyfile"))
			if strings.Contains(string(body), "old.example.test") {
				t.Fatal("removed route remains on disk")
			}
		})
	}
}
func TestRemoveRoutesDoesNotIgnoreFilesystemFailure(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir(), containerList: func(context.Context) ([]byte, error) {
		t.Fatal("Docker queried after failed fragment removal")
		return nil, nil
	}}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.StateDir, "deployments", "dpl_fixture.caddy")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "unexpected"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDeploymentRoutes(t.Context(), "dpl_fixture"); err == nil {
		t.Fatal("filesystem failure ignored")
	}
}
