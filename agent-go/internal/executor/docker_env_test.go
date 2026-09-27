package executor

// The .env the agent writes feeds Compose's dotenv parser,
// which interpolates $VAR/${VAR} in unquoted values and treats ` #` as a
// comment — a password like "pa$word #x" silently became "pa", and a var
// containing ${DB_PASSWORD} would publish the database password wherever
// the var lands (a site title, an app log). Values must be written
// LITERALLY: single-quoted when possible, double-quoted with \, " and $
// (doubled) escaped otherwise.
//
// A backslash in single quotes is not inert. Compose reads a
// backslash before the closing quote as an escaped quote — `K='abc\'` does
// not end, the value runs into the next line, and a crafted pair of values
// (`Z1='x\'` then `Z2=' HOST_PORT=… #'`) injects keys past the server's
// reserved ones. The reference evaluator below therefore parses the WHOLE
// file the way compose-go does, never a line at a time.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// dotenvReferenceParse evaluates a whole .env under the rules of Compose's
// dotenv parser (compose-go dotenv/parser.go):
//   - a statement is KEY=VALUE; blank lines and `#` lines are skipped;
//   - a quoted value runs until its closing quote, across lines. A
//     backslash before the quote character escapes it — in single quotes
//     too, which only skip escape expansion and interpolation. A backslash
//     before any other character is kept. After the closing quote, the
//     rest of the line is parsed as the next statement;
//   - an unquoted value ends at the newline, drops a ` #` comment and
//     trailing spaces;
//   - double-quoted values expand \\, \n, \t, \r and \$ (to $$); double-
//     quoted and unquoted values then interpolate $VAR/${VAR}, where $$ is
//     a literal $. Lookup reads env first, then keys parsed earlier.
//
// An unterminated quote or an invalid key is an error, like in Compose.
func dotenvReferenceParse(src string, env map[string]string) (map[string]string, error) {
	out := map[string]string{}
	interpolate := func(v string) (string, error) {
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			c := v[i]
			if c != '$' {
				b.WriteByte(c)
				continue
			}
			if i+1 < len(v) && v[i+1] == '$' {
				b.WriteByte('$')
				i++
				continue
			}
			name := ""
			if i+1 < len(v) && v[i+1] == '{' {
				end := strings.IndexByte(v[i:], '}')
				if end < 0 {
					return "", errors.New("unterminated interpolation")
				}
				name = v[i+2 : i+end]
				i += end
			} else {
				j := i + 1
				for j < len(v) && (v[j] == '_' || v[j] >= 'A' && v[j] <= 'Z' || v[j] >= 'a' && v[j] <= 'z' || v[j] >= '0' && v[j] <= '9') {
					j++
				}
				if j == i+1 {
					b.WriteByte(c)
					continue
				}
				name = v[i+1 : j]
				i = j - 1
			}
			if value, ok := env[name]; ok {
				b.WriteString(value)
			} else {
				b.WriteString(out[name])
			}
		}
		return b.String(), nil
	}
	expandEscapes := func(v string) string {
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] != '\\' || i+1 == len(v) {
				b.WriteByte(v[i])
				continue
			}
			i++
			switch v[i] {
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '$':
				b.WriteString("$$")
			default:
				b.WriteByte('\\')
				b.WriteByte(v[i])
			}
		}
		return b.String()
	}
	rest := src
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		if rest == "" {
			return out, nil
		}
		if rest[0] == '#' {
			if i := strings.IndexByte(rest, '\n'); i >= 0 {
				rest = rest[i:]
				continue
			}
			return out, nil
		}
		// The key runs to '=' (or the YAML-style ':'); a bare key followed by
		// a newline inherits its value from the environment. Any other
		// character fails with the rest of the line quoted in the error —
		// which is how a runaway quoted value echoes the NEXT var's value.
		key, inherited, end := "", false, -1
		for i, r := range rest {
			if r == ' ' || r == '\t' {
				continue
			}
			if r == '=' || r == ':' || r == '\n' {
				key, inherited, end = strings.TrimRightFunc(rest[:i], unicode.IsSpace), r == '\n', i
				break
			}
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.-[]", r)) {
				return nil, fmt.Errorf("unexpected character %q in variable name %q", r, strings.SplitN(rest, "\n", 2)[0])
			}
		}
		if end < 0 || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("invalid statement %q", strings.SplitN(rest, "\n", 2)[0])
		}
		if inherited {
			if value, ok := env[key]; ok {
				out[key] = value
			}
			rest = rest[end:]
			continue
		}
		rest = strings.TrimLeft(rest[end+1:], " \t")
		if rest != "" && (rest[0] == '\'' || rest[0] == '"') {
			quote := rest[0]
			var chars []byte
			escaped := false
			end := -1
			for i := 1; i < len(rest); i++ {
				c := rest[i]
				if c != quote {
					if !escaped && c == '\\' {
						escaped = true
						continue
					}
					if escaped {
						escaped = false
						chars = append(chars, '\\')
					}
					chars = append(chars, c)
					continue
				}
				if escaped {
					escaped = false
					chars = append(chars, c)
					continue
				}
				end = i
				break
			}
			if end < 0 {
				return nil, fmt.Errorf("unterminated quoted value for %s", key)
			}
			value := string(chars)
			if quote == '"' {
				var err error
				if value, err = interpolate(expandEscapes(value)); err != nil {
					return nil, err
				}
			}
			out[key] = value
			rest = rest[end+1:]
			continue
		}
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		value, err := interpolate(strings.TrimRightFunc(line, unicode.IsSpace))
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
}

// The reference itself must reproduce those failure modes, or it proves
// nothing. A trailing backslash in single quotes swallows the newline and
// the next key; the rest of that line then fails as a key name, and the
// error quotes the next var's value (it reached the server through the
// deploy error). A crafted next value instead injects a key.
func TestDotenvReferenceModelsEscapedClosingQuote(t *testing.T) {
	_, err := dotenvReferenceParse("A='abc\\'\nB='next-value'\n", nil)
	if err == nil || !strings.Contains(err.Error(), "next-value") {
		t.Fatalf("reference does not model the runaway value: %v", err)
	}
	got, err := dotenvReferenceParse("Z1='x\\'\nZ2=' HOST_PORT=6666 #'\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["HOST_PORT"] != "6666" || got["Z1"] != "x'\nZ2=" || len(got) != 2 {
		t.Fatalf("reference does not model the key injection: %q", got)
	}
	if _, err := dotenvReferenceParse("A='abc\\'\n", nil); err == nil {
		t.Fatal("reference accepts an unterminated quote")
	}
}

// hostileEnvValues are the values every quoting test round-trips; the
// expected result of each is the value itself, except that a real newline
// travels as the two characters \n (the pre-existing .env contract).
func hostileEnvValues() map[string]string {
	return map[string]string{
		"PLAIN":            "simple-value",
		"EMPTY":            "",
		"PASSWORD":         "pa$word #x",
		"EXPAND":           "${DB_PASSWORD}",
		"BRACEMID":         "prefix-${DB_PASSWORD}-suffix",
		"TITLE":            "Site #1 — $100",
		"SQUOTE":           "it's",
		"DQUOTE":           `say "hi"`,
		"BACKSLASH":        `C:\data\app`,
		"MIXED":            `it's $HOME and "quoted" \ end`,
		"MULTILINE":        "line1\nline2",
		"DOLLARONLY":       "$",
		"TRAILSPACE":       "ends with space ",
		"TRAILBACKSLASH":   `abc\`,
		"ONLYBACKSLASH":    `\`,
		"BACKSLASH_SQUOTE": `a\'b`,
		"BACKSLASH_DQUOTE": `a\"b`,
		"BACKSLASH_DOLLAR": `\$x`,
		"BACKSLASH_N":      `a\nb`,
		"BOTHQUOTES":       `it's "both" \`,
		"DOUBLEBACKSLASH":  `\\`,
	}
}

// injectionEnvValues is the crafted pair from the review, twice: each odd
// value ends in a backslash, each even value carries the statement to
// inject — the server's reserved HOST_PORT, and a DATABASE_URL that would
// slip past the binding collision check. Keys sort after the targets, so a
// successful injection overrides them.
func injectionEnvValues() map[string]string {
	return map[string]string{
		"HOST_PORT": "8080",
		"Z1":        `x\`,
		"Z2":        " HOST_PORT=6666 #",
		"Z3":        `y\`,
		"Z4":        " DATABASE_URL=postgresql://evil #",
	}
}

func expectedEnv(values map[string]string) map[string]string {
	want := map[string]string{}
	for k, v := range values {
		want[k] = strings.ReplaceAll(v, "\n", `\n`)
	}
	return want
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestRenderEnvWritesValuesLiterally(t *testing.T) {
	canary := map[string]string{
		"DB_PASSWORD": "CANARY-SECRET",
		"HOME":        "/home/canary",
		"word":        "CANARY",
	}
	values := hostileEnvValues()
	vars := map[string]any{}
	for k, v := range values {
		vars[k] = v
	}
	rendered := renderEnv(vars)
	got, err := dotenvReferenceParse(rendered, canary)
	if err != nil {
		t.Fatalf("rendered .env does not parse: %v\n%s", err, rendered)
	}
	want := expectedEnv(values)
	if !reflect.DeepEqual(sortedKeys(got), sortedKeys(want)) {
		t.Fatalf("the rendered .env yields another key set:\n got %q\nwant %q\n%s", sortedKeys(got), sortedKeys(want), rendered)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: rendered value does not round-trip: got %q want %q", k, got[k], v)
		}
	}
}

// A trailing backslash must never escape the closing quote. With the
// single-quote form it ran the value into the next line; a crafted pair of
// values then set keys of the caller's choosing — past the server's
// reserved keys (HOST_PORT here) and the DATABASE_URL collision check.
func TestRenderEnvCannotInjectKeys(t *testing.T) {
	values := injectionEnvValues()
	vars := map[string]any{}
	for k, v := range values {
		vars[k] = v
	}
	rendered := renderEnv(vars)
	got, err := dotenvReferenceParse(rendered, nil)
	if err != nil {
		t.Fatalf("rendered .env does not parse: %v\n%s", err, rendered)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("values changed or keys were injected:\n got %q\nwant %q\n%s", got, values, rendered)
	}
}

// The failure mode from the review, spelled out: the rendered file must
// never expose another var's value through interpolation.
func TestRenderEnvNeverLeaksAnotherVar(t *testing.T) {
	vars := map[string]any{
		"DB_PASSWORD": "s3cret-canary",
		"SITE_TITLE":  "${DB_PASSWORD}",
	}
	rendered := renderEnv(vars)
	got, err := dotenvReferenceParse(rendered, map[string]string{"DB_PASSWORD": "s3cret-canary"})
	if err != nil {
		t.Fatal(err)
	}
	if got["SITE_TITLE"] != "${DB_PASSWORD}" {
		t.Fatalf("SITE_TITLE evaluated to %q — the DB password would be published", got["SITE_TITLE"])
	}
	if !strings.Contains(rendered, "SITE_TITLE='${DB_PASSWORD}'") {
		t.Fatalf("SITE_TITLE not written as a single-quoted literal: %q", rendered)
	}
}

// Sanity: the exact render bytes for a representative set, so a future
// change in quoting style is a deliberate diff.
func TestRenderEnvQuotingShape(t *testing.T) {
	rendered := renderEnv(map[string]any{
		"A": "x",
		"B": "pa$word #x",
		"C": "it's",
		"D": `a\b "q" $5`,
		"E": `abc\`,
		"F": `say "hi" $5`,
	})
	want := "A='x'\n" +
		"B='pa$word #x'\n" +
		"C=\"it's\"\n" +
		"D=\"a\\\\b \\\"q\\\" $$5\"\n" +
		"E=\"abc\\\\\"\n" +
		"F='say \"hi\" $5'\n"
	if rendered != want {
		t.Fatalf("unexpected render:\n got %q\nwant %q", rendered, want)
	}
}

// The reference evaluator is a model; the guest's real Compose is the
// judge. `docker compose config` needs only the CLI plugin, no daemon: it
// loads the rendered .env through env_file and prints every key it read.
// Skips where Docker Compose is absent (the development workstation).
func TestRenderEnvThroughRealCompose(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose CLI unavailable")
	}
	values := hostileEnvValues()
	for k, v := range injectionEnvValues() {
		values[k] = v
	}
	vars := map[string]any{}
	for k, v := range values {
		vars[k] = v
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(renderEnv(vars)), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  probe:\n    image: busybox:1.37\n    env_file: [.env]\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("docker", "compose", "--project-name", "l4c-env-probe", "--project-directory", dir, "config", "--format", "json")
	command.Dir = dir
	command.Env = append(os.Environ(), "DB_PASSWORD=CANARY-SECRET", "HOME=/home/canary")
	out, err := command.Output()
	if err != nil {
		t.Fatalf("real docker compose rejected the rendered .env: %v", err)
	}
	var model struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &model); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for k, v := range model.Services["probe"].Environment {
		if v == nil {
			t.Fatalf("%s has no value in the resolved model", k)
		}
		// `config` prints its model re-escaped, so the output is itself a
		// valid compose file: a literal `$` comes out as `$$` (measured on
		// compose 2.40.3). The container receives the single `$`.
		got[k] = strings.ReplaceAll(*v, "$$", "$")
	}
	want := expectedEnv(values)
	if !reflect.DeepEqual(sortedKeys(got), sortedKeys(want)) {
		t.Fatalf("real compose read another key set:\n got %q\nwant %q", sortedKeys(got), sortedKeys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: real compose read %q, want %q", k, got[k], v)
		}
	}
}
