package executor

// renderEnv quotes every value literally, and the four
// redaction patterns required the bare `KEY=url` line. The quotes broke the
// redaction of every bound managed-database credential as soon as the new
// agent rewrote the .env: runtimeServiceBindingRedactions refused ("bound
// runtime credentials cannot be safely redacted"), withholding every
// command's output, and finishReplacement — the sequence the detached
// replacement worker writes its receipt with — got an EMPTY redaction set,
// so compose output and failure logs reached the receipt in the clear (and
// a restarted agent sends that receipt without the Execute redactor).
//
// These tests go renderEnv → .env on disk → both redactors, for every
// managed URL shape. No earlier test covered it: they all built the bare
// line by hand.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

type managedURLCase struct {
	name     string
	key      string
	url      string
	password string
}

// Fixture credentials only: fixed hex in the exact grammar the agent
// redacts, never a real secret.
func managedURLCases() []managedURLCase {
	pw := strings.Repeat("e", 64)
	bnd := strings.Repeat("a", 24)
	rev := strings.Repeat("b", 24)
	job := strings.Repeat("c", 16)
	provider := "dpl_" + strings.Repeat("d", 16)
	return []managedURLCase{
		{"postgres binding", "DATABASE_URL", "postgresql://imp_" + bnd + ":" + pw + "@pg_" + provider + ":5432/imp_" + bnd + "?sslmode=disable", pw},
		{"postgres generation", "DATABASE_URL", "postgresql://ibg_" + bnd + "_" + rev + ":" + pw + "@pg_" + provider + ":5432/imp_" + bnd + "?sslmode=disable", pw},
		{"mariadb generation", "DATABASE_URL", "mysql://ibg_" + bnd + "_" + rev + ":" + pw + "@mariadb_" + provider + ":3306/imp_" + bnd, pw},
		{"postgres verify job", "IMPREZA_DATABASE_JOB_URL", "postgresql://ijv_" + job + ":" + pw + "@pg_" + provider + ":5432/imp_verify_" + job + "?sslmode=disable", pw},
		{"postgres restore job", "DATABASE_URL", "postgresql://ijr_" + job + ":" + pw + "@pg_" + provider + ":5432/imp_restore_" + job + "?sslmode=disable", pw},
		{"mariadb verify job", "DATABASE_URL", "mysql://ijv_" + job + ":" + pw + "@mariadb_" + provider + ":3306/imp_verify_" + job, pw},
		{"mariadb restore job", "IMPREZA_DATABASE_JOB_URL", "mysql://ijr_" + job + ":" + pw + "@mariadb_" + provider + ":3306/imp_restore_" + job, pw},
	}
}

// boundAppFixture writes what deploy() writes for a bound consumer: the
// compose file joining the binding network, and the .env from renderEnv.
func boundAppFixture(t *testing.T, key, url string) (*Docker, string) {
	t.Helper()
	d := &Docker{StateDir: t.TempDir(), Log: discardLogger()}
	id := "dpl_" + strings.Repeat("f", 16)
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  app:\n    image: example.invalid/app:1\n    networks: [impreza-binding-" + strings.Repeat("a", 24) + "]\n" +
		"networks:\n  impreza-binding-" + strings.Repeat("a", 24) + ":\n    name: dpl_" + strings.Repeat("d", 16) + "_default\n    external: true\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	env := renderEnv(map[string]any{key: url, "APP_NAME": "it's mine", "DEPLOYMENT_ID": id})
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return d, id
}

func TestRenderedEnvKeepsRuntimeBindingRedaction(t *testing.T) {
	for _, c := range managedURLCases() {
		t.Run(c.name, func(t *testing.T) {
			d, id := boundAppFixture(t, c.key, c.url)
			values, err := d.runtimeServiceBindingRedactions(id)
			if err != nil {
				t.Fatalf("a bound app whose .env this agent wrote cannot be redacted: %v", err)
			}
			if values[c.url] == "" || values[c.password] == "" {
				t.Fatalf("the rendered credential is missing from the redaction set: %d values", len(values))
			}
		})
	}
}

func TestRenderedEnvKeepsReplacementRedaction(t *testing.T) {
	for _, c := range managedURLCases() {
		t.Run(c.name, func(t *testing.T) {
			// The exact expression finishReplacement builds its redaction from.
			values := serviceBindingEnvRedactions([]byte(renderEnv(map[string]any{c.key: c.url, "OTHER": `a\b`})))
			if values[c.url] == "" || values[c.password] == "" {
				t.Fatalf("finishReplacement would redact nothing for a rendered %s", c.name)
			}
			text := redactServiceBindingText("compose said: "+c.key+"='"+c.url+"' pw="+c.password, values)
			if strings.Contains(text, c.password) || strings.Contains(text, c.url) {
				t.Fatalf("credential survived redaction: %q", text)
			}
		})
	}
}

// The .env an earlier agent wrote holds the bare value; it stays the
// previous release's redaction source, so it must keep matching. Either
// quote style matches too.
func TestEnvBindingRedactionAcceptsEveryQuoting(t *testing.T) {
	for _, c := range managedURLCases() {
		for _, line := range []string{c.key + "=" + c.url, c.key + "='" + c.url + "'", c.key + `="` + c.url + `"`, c.key + "='" + c.url + "'\r"} {
			values := serviceBindingEnvRedactions([]byte("A='x'\n" + line + "\nZ='y'\n"))
			if len(values) != 2 || values[c.url] == "" || values[c.password] == "" {
				t.Fatalf("%s: %q not recognized (%d values)", c.name, line, len(values))
			}
		}
	}
	// The rollback comparison still reads only the bare, Compose-resolved value.
	pg := managedURLCases()[0]
	if !bindingRuntimeURLPattern.MatchString("DATABASE_URL=" + pg.url) {
		t.Fatal("bare resolved value no longer recognized")
	}
	if bindingRuntimeURLPattern.MatchString("DATABASE_URL='" + pg.url + "'") {
		t.Fatal("the rollback check must not accept quote characters in a resolved value")
	}
}

// The integration the review found broken: finishReplacement — shared by
// the synchronous path and the detached replacement worker — fails the
// startup with compose output and container logs that echo the .env. The
// result (the worker's receipt) must carry no credential.
func TestFinishReplacementRedactsRenderedCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux command execution")
	}
	for _, c := range managedURLCases()[:3] {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			// Every call prints the private .env and the bare password (the way
			// an app logs its connection string); `up` fails.
			docker := "#!/bin/sh\ncat .env 2>/dev/null\necho \"password=" + c.password + "\"\n" +
				"case \" $* \" in *\" up \"*) exit 1;; esac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			d, id := boundAppFixture(t, c.key, c.url)
			p := sdkclient.DeployPayload{DeploymentID: id, Vars: map[string]any{c.key: c.url, "APP_NAME": "it's mine", "DEPLOYMENT_ID": id},
				Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ComposeYAML: "services: {}"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result := d.finishReplacement(ctx, &sdkclient.PollCommand{ID: "cmd_redact"}, p, nil, false, "")
			raw, _ := json.Marshal(result)
			if result.Status == "success" {
				t.Fatal("the failing startup reported success")
			}
			if strings.Contains(string(raw), c.password) || strings.Contains(string(raw), c.url) {
				t.Fatalf("the replacement result carries the bound credential: %s", raw)
			}
			if !strings.Contains(string(raw), "[redacted]") {
				t.Fatalf("the output was not redacted but dropped; expected [redacted] markers: %s", raw)
			}
		})
	}
}
