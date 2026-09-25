package executor

import (
	"fmt"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"gopkg.in/yaml.v3"
)

const sandboxBase = `services:
  web:
    image: nginx:alpine
    ports:
      - "${HOST_PORT}:8080"
`

func TestApplySandboxRewritesStructurally(t *testing.T) {
	out, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{MaxLifetimeMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	// Assert on the parsed structure, not on YAML formatting.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	if got := fmt.Sprintf("%v", svc["cap_drop"]); !strings.Contains(got, "ALL") {
		t.Fatalf("cap_drop ALL missing: %v", got)
	}
	caps := fmt.Sprintf("%v", svc["cap_add"])
	for _, want := range []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"} {
		if !strings.Contains(caps, want) {
			t.Fatalf("cap_add missing %s: %v", want, caps)
		}
	}
	if opts := fmt.Sprintf("%v", svc["security_opt"]); !strings.Contains(opts, "no-new-privileges:true") {
		t.Fatalf("no-new-privileges missing: %v", opts)
	}
	if svc["read_only"] != true {
		t.Fatalf("read_only missing: %v", svc["read_only"])
	}
	if fmt.Sprintf("%v", svc["restart"]) != "no" {
		t.Fatalf("restart policy must be no: %v", svc["restart"])
	}
	if _, hasPorts := svc["ports"]; hasPorts {
		t.Fatal("sandbox must not keep host NAT ports")
	}
	if _, hasTmpfs := svc["tmpfs"]; !hasTmpfs {
		t.Fatal("tmpfs /tmp missing")
	}
}

func TestApplySandboxRefusals(t *testing.T) {
	spec := sdkclient.SandboxSpec{MaxLifetimeMinutes: 30}
	bad := map[string]string{
		"privileged":       "services:\n  web:\n    image: x\n    privileged: true\n",
		"cap_add":          "services:\n  web:\n    image: x\n    cap_add: [NET_ADMIN]\n",
		"docker socket":    "services:\n  web:\n    image: x\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n",
		"host bind":        "services:\n  web:\n    image: x\n    volumes:\n      - ./data:/data\n",
		"network_mode":     "services:\n  web:\n    image: x\n    network_mode: host\n",
		"named volume":     "services:\n  web:\n    image: x\n    volumes:\n      - data:/data\n",
		"compose include":  "include: [other.yaml]\nservices:\n  web:\n    image: x\n",
	}
	for why, yml := range bad {
		if _, err := applySandbox(yml, "dpl_aaaa111122223333", spec); err == nil {
			t.Fatalf("sandbox accepted %s", why)
		}
	}
	if _, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{MaxLifetimeMinutes: 0}); err == nil {
		t.Fatal("sandbox accepted a missing wall-clock budget")
	}
	if _, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{MaxLifetimeMinutes: 5000}); err == nil {
		t.Fatal("sandbox accepted an out-of-range budget")
	}
}

func TestApplySandboxNamedVolumesDropped(t *testing.T) {
	// A named volume reference without a bind is also refused (ephemeral
	// means tmpfs only) — the volumes: map itself disappears.
	out, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{MaxLifetimeMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\nvolumes:") {
		t.Fatalf("top-level volumes survived:\n%s", out)
	}
}

func TestApplySandboxKeepsConventionalWritablePaths(t *testing.T) {
	// Regression (live battery 20260925-d02a7f): a read-only rootfs without
	// /run tmpfs killed default images; app-specific paths arrive via
	// ExtraTmpfs with options forced by the transform.
	out, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{
		MaxLifetimeMinutes: 30,
		ExtraTmpfs:         []string{"/var/cache/nginx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	tmpfs := fmt.Sprintf("%v", svc["tmpfs"])
	for _, want := range []string{"/tmp", "/run", "/var/cache/nginx"} {
		if !strings.Contains(tmpfs, want) {
			t.Fatalf("sandbox tmpfs missing %q: %v", want, tmpfs)
		}
	}
	if strings.Count(tmpfs, "nosuid") != 3 {
		t.Fatalf("extra tmpfs must carry forced mount options: %v", tmpfs)
	}
	if svc["read_only"] != true {
		t.Fatal("read_only lost")
	}
	// Unsafe extra paths refuse.
	for _, bad := range []string{"var/cache", "/a/../b", "/x:y", strings.Repeat("/a", 200)} {
		if _, err := applySandbox(sandboxBase, "dpl_aaaa111122223333", sdkclient.SandboxSpec{
			MaxLifetimeMinutes: 30, ExtraTmpfs: []string{bad},
		}); err == nil {
			t.Fatalf("unsafe extra_tmpfs accepted: %q", bad)
		}
	}
}
