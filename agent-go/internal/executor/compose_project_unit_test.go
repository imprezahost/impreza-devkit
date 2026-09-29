package executor

// The pieces of the per-app project fix: the project an app directory resolves to
// and the variable names no .env may carry.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeProjectIsTheOneComposeDerived(t *testing.T) {
	const own = "dpl_0c1a000000000001"
	cases := []struct {
		name, compose, want string
	}{
		{"no top-level name", "services: {}\n", own},
		{"empty name", "name: \"\"\nservices: {}\n", own},
		{"literal name, normalized as Compose does", "name: Legacy Shop_1\nservices: {}\n", "legacyshop_1"},
		{"its own id", "name: " + own + "\n", own},
		{"a name that normalizes to nothing", "name: \"__\"\n", own},
		{"an alias to a literal name", "x-n: &n shop\nname: *n\n", "shop"},
		// Compose without -p refused these, so nothing ever ran under them.
		{"a number", "name: 123\n", own},
		{"null", "name: null\n", own},
		{"a mapping", "name: {a: b}\n", own},
		{"not a mapping at all", "new compose", own},
		{"invalid YAML", "name: [\n", own},
		// Refused: another project on this server, or one a variable picks.
		{"another app's project", "name: dpl_0c1b000000000002\n", ""},
		{"an internal job's project", "name: bkpjob_0123456789abcdef\n", ""},
		{"a variable", "name: ${APP}\n", ""},
		{"a variable with a default", "name: ${APP:-shop}\n", ""},
		{"an escaped dollar", "name: shop$$x\n", ""},
		{"an alias to a variable", "x-n: &n ${APP}\nname: *n\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), own)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(c.compose), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := composeProject(dir)
			if c.want == "" {
				if err == nil {
					t.Fatalf("resolved %q, want a refusal", got)
				}
				if strings.Contains(err.Error(), "dpl_a71b") || strings.Contains(err.Error(), "shop") {
					t.Fatalf("the refusal quotes the compose file: %v", err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			args, err := composeCommand(dir, "down")
			if err != nil || strings.Join(args, " ") != "compose -p "+c.want+" -f "+filepath.Join(dir, "compose.yaml")+" down" {
				t.Fatalf("pinned command %q, %v", args, err)
			}
		})
	}
	// A directory without compose.yaml keeps its own name; Compose itself
	// reports the missing file. Compose lowercases a directory name.
	dir := filepath.Join(t.TempDir(), "dpl_0C1A000000000001")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := composeProject(dir); err != nil || got != "dpl_0c1a000000000001" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestValidateEnvNames(t *testing.T) {
	for _, ok := range []map[string]any{nil, {}, {"DEPLOYMENT_ID": "x", "DOMAIN_URL": "https://a", "_LEADING": 1, "lower_case": "y", "COMPOSER_HOME": "/c", "DOCKERFILE_PATH": "d"}} {
		if err := validateEnvNames(ok); err != nil {
			t.Fatalf("%v refused: %v", ok, err)
		}
	}
	err := validateEnvNames(map[string]any{"COMPOSE_FILE": 1, "docker_host": 2, "Compose_Profiles": 3, "BAD\nKEY": 4, "A=B": 5, "APP": 6})
	if err == nil {
		t.Fatal("engine and malformed names were accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "COMPOSE_FILE, Compose_Profiles, docker_host") || !strings.Contains(msg, "2 invalid names") {
		t.Fatalf("the refusal does not name the engine variables and count the malformed ones: %q", msg)
	}
	if strings.Contains(msg, "BAD") || strings.Contains(msg, "A=B") || strings.Contains(msg, "\n") || strings.Contains(msg, "APP,") {
		t.Fatalf("the refusal echoes a malformed or an accepted name: %q", msg)
	}
	if _, err := renderEnv(map[string]any{"APP": "x", "COMPOSE_PROJECT_NAME": "dpl_0c1b000000000002"}); err == nil {
		t.Fatal("renderEnv wrote a .env naming the Compose project")
	}
	if rendered, err := renderEnv(map[string]any{"APP": "x"}); err != nil || rendered != "APP='x'\n" {
		t.Fatalf("a plain .env no longer renders: %q, %v", rendered, err)
	}
}
