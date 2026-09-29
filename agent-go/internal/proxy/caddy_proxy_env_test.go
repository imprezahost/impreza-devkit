package proxy

// The pieces of the proxy recreation fix: the env-file reader follows the Docker
// CLI, the environment comparison, and the spec label, which never carries
// a credential and does not move when only the credentials change.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadProxyEnvFileFollowsTheDockerCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caddy.env")
	t.Setenv("PROXY_PASSED_THROUGH", "from-agent")
	body := "\xef\xbb\xbf# managed by impreza-agent — do not edit\r\n" +
		"IMPREZA_AGENT_ID=agt_1\r\n" +
		"   IMPREZA_API_URL=https://api.example.invalid/?a=b#frag\n" +
		"\n" +
		"  # indented comment\n" +
		"EMPTY=\n" +
		"PROXY_PASSED_THROUGH\n" +
		"PROXY_UNSET_IN_AGENT\n" +
		"QUOTED='kept as written'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProxyEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"IMPREZA_AGENT_ID=agt_1", "IMPREZA_API_URL=https://api.example.invalid/?a=b#frag", "EMPTY=",
		"PROXY_PASSED_THROUGH=from-agent", "QUOTED='kept as written'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env-file read differently from the Docker CLI:\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"=novalue\n", "HAS SPACE=x\n", "OK=\xff\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readProxyEnvFile(path); err == nil {
			t.Fatalf("the Docker CLI refuses %q, so must the check", bad)
		}
	}
}

func TestProxyEnvMatches(t *testing.T) {
	image := []string{"PATH=/usr/bin", "CADDY_VERSION=v2"}
	creds := []string{"IMPREZA_AGENT_ID=agt_1", "IMPREZA_AGENT_SECRET=s1", "IMPREZA_API_URL="}
	cases := []struct {
		name string
		want []string
		have []string
		ok   bool
	}{
		{"same credentials over image defaults", creds, append(append([]string{}, image...), creds...), true},
		{"rotated secret", []string{"IMPREZA_AGENT_ID=agt_1", "IMPREZA_AGENT_SECRET=s2", "IMPREZA_API_URL="}, append(append([]string{}, image...), creds...), false},
		{"credentials cleared, stale copy in the container", nil, append(append([]string{}, image...), creds...), false},
		{"placeholder file, clean container", nil, image, true},
		{"new variable", append(append([]string{}, creds...), "EXTRA=1"), append(append([]string{}, image...), creds...), false},
		{"a repeated name, the last one wins", []string{"IMPREZA_AGENT_ID=old", "IMPREZA_AGENT_ID=agt_1", "IMPREZA_AGENT_SECRET=s1", "IMPREZA_API_URL="}, creds, true},
		{"variable removed from the env-file, stale copy in the container", creds, append(append(append([]string{}, image...), creds...), "STALE_EXTRA=1"), false},
		{"all credentials removed, only image defaults may remain", nil, image, true},
	}
	for _, c := range cases {
		if got := proxyEnvMatches(c.want, c.have, []string{"PATH", "CADDY_VERSION"}); got != c.ok {
			t.Errorf("%s: got %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestProxySpecLabelNamesTheRunArgumentsOnly(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-a", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	first := proxyContainer(t, d)
	label := first.Config.Labels[proxySpecLabel]
	envFile := filepath.Join(c.StateDir, "caddy.env")
	if label != proxySpecDigest(c.runArgs(envFile, filepath.Join(c.StateDir, "Caddyfile"))) {
		t.Fatalf("the proxy is not stamped with the digest of its own run arguments: %q", label)
	}
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-b", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	second := proxyContainer(t, d)
	if second.ID == first.ID || second.Config.Labels[proxySpecLabel] != label {
		t.Fatal("new credentials must recreate the proxy under the same spec: the label carries no credential")
	}
	for _, call := range d.Calls("run") {
		for _, arg := range call.Args {
			if strings.Contains(arg, "proxy-secret") {
				t.Fatal("a credential reached the docker command line")
			}
		}
	}
}
