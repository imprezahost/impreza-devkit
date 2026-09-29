package executor

// A manual rollback checks that the retained release keeps the app's
// managed database connection, and that check only knew the Postgres URL
// grammar: every app bound to MariaDB was refused with "release database
// connection cannot be verified", so its rollback never worked (the refusal
// was safe, the feature dead). These tests send the rollback through
// Execute, with the current model Compose resolves served by the docker
// double and a retained release on disk; the release image is missing, so
// a rollback that passes the connection check stops at the image check and
// no container is touched. They only use what the 0.6.23 agent had, so the
// same file also runs against that agent, where it fails.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	x75App     = "dpl_0c5a000000000001"
	x75Release = "rel_x75previous"
)

var x75ReleaseImage = "sha256:" + strings.Repeat("75", 32)

func x75MariaDBURL(secret string) string {
	return "mysql://ibg_" + strings.Repeat("a", 24) + "_" + strings.Repeat("b", 24) + ":" + strings.Repeat(secret, 64) +
		"@mariadb_dpl_" + strings.Repeat("d", 16) + ":3306/imp_" + strings.Repeat("e", 24)
}

func x75PostgresURL(secret string) string {
	return "postgresql://ibg_" + strings.Repeat("a", 24) + "_" + strings.Repeat("b", 24) + ":" + strings.Repeat(secret, 64) +
		"@pg_dpl_" + strings.Repeat("d", 16) + ":5432/imp_" + strings.Repeat("e", 24) + "?sslmode=disable"
}

// x75Model is the resolved Compose model of a web service bound to a
// managed database through its binding network.
func x75Model(image, url string) string {
	raw, _ := json.Marshal(map[string]any{
		"name": x75App,
		"services": map[string]any{"web": map[string]any{
			"image":       image,
			"environment": map[string]any{"DATABASE_URL": url},
			"networks":    map[string]any{"default": nil, "impreza-binding-a75": nil},
		}},
		"networks": map[string]any{"default": map[string]any{}, "impreza-binding-a75": map[string]any{"external": true}},
	})
	return string(raw)
}

// x75Rollback asks to roll the app back to its retained release and
// returns the agent's result. The .env holds the binding's URL, as a bound
// app's does; the resolved models carry currentURL and releaseURL.
func x75Rollback(t *testing.T, currentURL, releaseURL string) sdkclient.DeployResult {
	t.Helper()
	return x75RollbackWithEnv(t, currentURL, currentURL, releaseURL)
}

func x75RollbackWithEnv(t *testing.T, envURL, currentURL, releaseURL string) sdkclient.DeployResult {
	t.Helper()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dir := d.appDir(x75App)
	if err := os.MkdirAll(filepath.Join(dir, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: example.invalid/web:2\n    networks: [default, impreza-binding-a75]\n" +
		"networks:\n  impreza-binding-a75:\n    external: true\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATABASE_URL='"+envURL+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := runtimeRelease{
		Metadata: sdkclient.DeploymentRelease{ID: x75Release, CreatedAt: "2026-09-27T00:00:00Z", ImageIDs: map[string]string{"web": x75ReleaseImage}},
		Compose:  json.RawMessage(x75Model(x75ReleaseImage, releaseURL)),
	}
	raw, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "releases", x75Release+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dockertest.Install(t, dockertest.State{
		ComposeConfig: map[string]string{dir: x75Model("example.invalid/web:2", currentURL)},
		MissingImages: []string{x75ReleaseImage},
	})
	payload, err := json.Marshal(sdkclient.RollbackPayload{DeploymentID: x75App, TargetVersion: x75Release})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.Execute(ctx, &sdkclient.PollCommand{ID: "cmd_rollback", Kind: sdkclient.CommandRollback, Payload: payload})
}

func TestRollbackOfAMariaDBBoundAppPassesTheConnectionCheck(t *testing.T) {
	r := x75Rollback(t, x75MariaDBURL("c"), x75MariaDBURL("c"))
	if strings.Contains(r.Error, "cannot be verified") {
		t.Fatalf("an app bound to MariaDB cannot be rolled back: %s", r.Error)
	}
	if !strings.Contains(r.Error, "release image is unavailable") {
		t.Fatalf("the rollback should pass the connection check and stop at the missing image: %s", r.Error)
	}
}

func TestRollbackRefusesAReleaseWithAnotherMariaDBConnection(t *testing.T) {
	r := x75Rollback(t, x75MariaDBURL("c"), x75MariaDBURL("f"))
	if r.Status == "success" || !strings.Contains(r.Error, "release changes a managed database connection") {
		t.Fatalf("a release with another MariaDB login must be refused as a connection change: %s", r.Error)
	}
	if strings.Contains(r.Error, strings.Repeat("c", 64)) || strings.Contains(r.Error, strings.Repeat("f", 64)) {
		t.Fatal("the refusal carries a database password")
	}
}

// Guards: these read the same before and after the fix.

func TestRollbackOfAPostgresBoundAppPassesTheConnectionCheck(t *testing.T) {
	r := x75Rollback(t, x75PostgresURL("c"), x75PostgresURL("c"))
	if !strings.Contains(r.Error, "release image is unavailable") {
		t.Fatalf("a Postgres-bound rollback no longer reaches the image check: %s", r.Error)
	}
}

func TestRollbackRefusesAnUnmanagedConnectionOnABindingNetwork(t *testing.T) {
	url := "mysql://someone:" + strings.Repeat("c", 64) + "@db.example.invalid:3306/app"
	r := x75RollbackWithEnv(t, x75MariaDBURL("c"), url, url)
	if r.Status == "success" || !strings.Contains(r.Error, "cannot be verified") {
		t.Fatalf("a binding network with an unmanaged connection must stay refused: %s", r.Error)
	}
}
