package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Exercise the command dispatcher and retained archive, rather than only its helper.
func TestRetainedBindingRollbackEntry(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		for _, transition := range []string{"rotation", "removal"} {
			t.Run(engine+"/"+transition, func(t *testing.T) {
				d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
				id := "dpl_" + strings.Repeat("a", 16)
				dir := d.appDir(id)
				if err := os.MkdirAll(filepath.Join(dir, "releases"), 0700); err != nil {
					t.Fatal(err)
				}
				key := make([]byte, 32)
				if _, err := rand.Read(key); err != nil {
					t.Fatal(err)
				}
				old := hex.EncodeToString(key)
				if _, err := rand.Read(key); err != nil {
					t.Fatal(err)
				}
				next := hex.EncodeToString(key)
				login := "ibg_" + strings.Repeat("b", 24) + "_" + strings.Repeat("c", 24)
				url := func(password string) string {
					if engine == "mysql" {
						return "mysql://" + login + ":" + password + "@mariadb_dpl_" + strings.Repeat("d", 16) + ":3306/imp_" + strings.Repeat("b", 24)
					}
					return "postgresql://" + login + ":" + password + "@pg_dpl_" + strings.Repeat("d", 16) + ":5432/imp_" + strings.Repeat("b", 24) + "?sslmode=disable"
				}
				image := "sha256:" + strings.Repeat("e", 64)
				model := func(value string) []byte {
					raw, _ := json.Marshal(map[string]any{"name": id, "services": map[string]any{"app": map[string]any{"image": image, "environment": map[string]string{"DATABASE_URL": value}, "networks": map[string]any{"default": nil}}}})
					return raw
				}
				target := runtimeRelease{Compose: model(url(old)), Metadata: sdkclient.DeploymentRelease{ID: "rel_retained_binding", ImageIDs: map[string]string{"app": image}}, Env: []byte("DATABASE_URL=" + url(old) + "\n"), EnvExists: true}
				archive, _ := json.Marshal(target)
				file := filepath.Join(dir, "releases", "rel_retained_binding.json")
				if err := os.WriteFile(file, archive, 0600); err != nil {
					t.Fatal(err)
				}
				// The current implementation retains the old secret on disk: measure this explicitly.
				saved, err := os.ReadFile(file)
				if err != nil || !strings.Contains(string(saved), old) {
					t.Fatal("retention measurement failed")
				}
				current := url(next)
				if transition == "removal" {
					current = ""
				}
				if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), model(current), 0600); err != nil {
					t.Fatal(err)
				}
				double := dockertest.Install(t, dockertest.State{ComposeConfig: map[string]string{dir: string(model(current))}})
				payload, _ := json.Marshal(sdkclient.RollbackPayload{DeploymentID: id, TargetVersion: "rel_retained_binding"})
				result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_retained_rollback", Kind: sdkclient.CommandRollback, Payload: payload})
				if result.Status != "failed" || !strings.Contains(result.Error, "managed database connection") {
					t.Fatalf("revoked binding rollback was not refused by its boundary: status=%s", result.Status)
				}
				output, _ := json.Marshal(result)
				if strings.Contains(string(output), old) || strings.Contains(string(output), next) {
					t.Fatal("result leaked credential")
				}
				for _, call := range double.State().Calls {
					for _, arg := range call.Args {
						if arg == "up" || arg == "down" || arg == "rm" || arg == "tag" {
							t.Fatal("refused rollback changed runtime")
						}
					}
				}
				unchanged, _ := os.ReadFile(file)
				if string(unchanged) != string(archive) {
					t.Fatal("refused rollback altered retained evidence")
				}
			})
		}
	}
}
