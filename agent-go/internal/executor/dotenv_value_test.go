package executor

import (
	"strings"
	"testing"
)

// dotenvValue reads KEY's value from an app .env the agent rendered, undoing
// quoteEnvValue: single quotes are literal, double quotes carry `\\`, `\"`
// and `$$` escapes. Live tests used to scan the raw line for
// `DATABASE_URL=postgresql://`, which stopped matching once every value
// was quoted, so they reported "serving credential missing" for a
// credential that was there.
func dotenvValue(raw []byte, key string) (string, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		value, ok := strings.CutPrefix(line, key+"=")
		if !ok {
			continue
		}
		switch {
		case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
			return value[1 : len(value)-1], true
		case len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"':
			var b strings.Builder
			in := value[1 : len(value)-1]
			for i := 0; i < len(in); i++ {
				c := in[i]
				if c == '\\' && i+1 < len(in) {
					i++
					b.WriteByte(in[i])
					continue
				}
				if c == '$' && i+1 < len(in) && in[i+1] == '$' {
					i++
				}
				b.WriteByte(c)
			}
			return b.String(), true
		default:
			return value, true
		}
	}
	return "", false
}

// The helper must read back exactly what the product writes.
func TestDotenvValueReadsWhatRenderEnvWrites(t *testing.T) {
	values := map[string]any{
		"DATABASE_URL":   "postgresql://svc_login:p4ss@db_provider:5432/app",
		"MYSQL_URL":      "mysql://u:p@mariadb_x:3306/db",
		"WITH_QUOTE":     "it's",
		"WITH_BACKSLASH": `a\b`,
		"WITH_DOLLAR":    `x$y\z`,
		"WITH_DQUOTE":    `say "hi" \ now`,
	}
	rendered, err := renderEnv(values)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range values {
		got, ok := dotenvValue([]byte(rendered), k)
		if !ok || got != v.(string) {
			t.Fatalf("%s: rendered %q, read back %q (%v)", k, rendered, got, ok)
		}
	}
	// the raw scan the live tests used no longer matches a rendered URL
	if strings.Contains(rendered, "\nDATABASE_URL=postgresql://") || strings.HasPrefix(rendered, "DATABASE_URL=postgresql://") {
		t.Fatal("renderEnv no longer quotes: the raw-line diagnosis no longer holds")
	}
	if _, ok := dotenvValue([]byte(rendered), "ABSENT"); ok {
		t.Fatal("an absent key was found")
	}
}
