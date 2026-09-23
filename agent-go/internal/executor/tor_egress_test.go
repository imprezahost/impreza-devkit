package executor

import (
	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestTorEgressIsolatesEffectiveServices(t *testing.T) {
	input := `services:
  web:
    image: busybox:1.37
    environment: ["KEEP=yes"]
    cap_drop: [ALL]
    ports: ["8080:80"]
    networks: [impreza-proxy]
  db:
    image: postgres:16
networks:
  impreza-proxy:
    external: true
volumes:
  data: {}
`
	out, err := applyTorEgress(input, "dpl_aaaaaaaaaaaaaaaa", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	services := doc["services"].(map[string]any)
	for _, name := range []string{"web", "db"} {
		service := services[name].(map[string]any)
		nets := service["networks"].([]any)
		if len(nets) != 1 || nets[0] != "tor-internal" {
			t.Fatal("application has an escape network", service)
		}
		if _, ok := service["ports"]; ok {
			t.Fatal("published app port bypasses managed ingress")
		}
		env := service["environment"].(map[string]any)
		if env["ALL_PROXY"] != "socks5h://tor-socks:9050" {
			t.Fatal("remote DNS not enforced")
		}
		if service["dns"].([]any)[0] != "127.0.0.1" {
			t.Fatal("external DNS allowed")
		}
	}
	web := services["web"].(map[string]any)
	if web["environment"].(map[string]any)["KEEP"] != "yes" {
		t.Fatal("environment lost")
	}
	if !strings.Contains(out, "- ALL") {
		t.Fatal("preexisting capability hardening lost")
	}
	tor := services[TorSidecarService].(map[string]any)
	if tor["image"] != proxy.TorImage || len(tor["networks"].([]any)) != 2 {
		t.Fatal("sidecar must bridge only its own private networks")
	}
	if _, ok := tor["ports"]; ok {
		t.Fatal("SOCKS exposed to host")
	}
	if len(doc["networks"].(map[string]any)) != 2 {
		t.Fatal("external networks retained")
	}
}
func TestTorEgressRejectsEscapeOptions(t *testing.T) {
	for _, option := range []string{"network_mode: host", "privileged: true", "cap_add: [NET_ADMIN]", "devices: [/dev/net/tun]", "pid: host", "sysctls: {net.ipv4.ip_forward: 1}", "extends: other"} {
		_, err := applyTorEgress("services:\n  web:\n    image: busybox\n    "+option+"\n", "dpl_aaaaaaaaaaaaaaaa", nil)
		if err == nil {
			t.Fatal("escape accepted", option)
		}
	}
	for _, input := range []string{"services: [broken]", "services:\n  web: &x {image: busybox}\n  other: *x\n", "services:\n  tor-socks: {image: hostile}\n", "services:\n  web: {image: a}\n  web: {image: b}\n"} {
		if _, err := applyTorEgress(input, "dpl_aaaaaaaaaaaaaaaa", nil); err == nil {
			t.Fatal("unsafe YAML accepted", input)
		}
	}
}

func TestReplacementKeepsTorPolicy(t *testing.T) {
	p := sdkclient.DeployPayload{Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{TorEgress: true}}}
	if !replacementPayload(p).Manifest.Runtime.TorEgress {
		t.Fatal("worker lost Tor ingress policy")
	}
}
