package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	"gopkg.in/yaml.v3"
)

const TorSidecarService = "tor-socks"
const TorSocksPort = 9050
const TorEgressProtocol = "tor-egress-v1"

// Transform the effective Compose structurally, never by concatenating YAML.
// Only runtime traffic is isolated; source fetch and image builds are outside
// this contract. A Docker-owned isolated network is verified before replacement.
func applyTorEgress(composeYAML, deploymentID string, _ map[string]any) (string, error) {
	if len(composeYAML) > 2<<20 || !recoveryDeploymentID.MatchString(deploymentID) {
		return "", errors.New("invalid Tor egress deployment")
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(composeYAML), &node); err != nil {
		return "", errors.New("invalid Tor egress Compose")
	}
	count := 0
	var check func(*yaml.Node) error
	check = func(n *yaml.Node) error {
		count++
		if count > 20000 || n.Kind == yaml.AliasNode || n.Anchor != "" {
			return errors.New("Tor egress Compose does not support aliases or oversized documents")
		}
		for _, child := range n.Content {
			if err := check(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(&node); err != nil {
		return "", err
	}
	var doc map[string]any
	if err := node.Decode(&doc); err != nil {
		return "", errors.New("invalid Tor egress Compose mapping")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 16 {
		return "", errors.New("Tor egress requires 1-16 services")
	}
	if _, exists := services[TorSidecarService]; exists {
		return "", errors.New("reserved Tor sidecar service")
	}
	if _, exists := doc["include"]; exists {
		return "", errors.New("Tor egress refuses Compose includes")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("invalid Tor service")
		}
		for _, key := range []string{"network_mode", "privileged", "cap_add", "devices", "device_cgroup_rules", "sysctls", "pid", "ipc", "extends", "provider"} {
			if _, exists := service[key]; exists {
				return "", fmt.Errorf("Tor egress refuses service option %s", key)
			}
		}
		// Enforce least privilege regardless of the source manifest's defaults.
		caps := []string{"NET_ADMIN", "NET_RAW"}
		if old, ok := service["cap_drop"].([]any); ok {
			for _, v := range old {
				value, ok := v.(string)
				if !ok {
					return "", errors.New("invalid capability drop list")
				}
				caps = append(caps, value)
			}
		}
		service["cap_drop"] = caps
		opts := []string{"no-new-privileges:true"}
		if old, exists := service["security_opt"]; exists {
			items, ok := old.([]any)
			if !ok {
				return "", errors.New("invalid security options")
			}
			for _, item := range items {
				v, ok := item.(string)
				if v == "no-new-privileges:true" || v == "no-new-privileges" {
					continue
				}
				if !ok || strings.Contains(v, "unconfined") || strings.HasPrefix(v, "no-new-privileges") {
					return "", errors.New("unsupported Tor security option")
				}
				opts = append(opts, v)
			}
		}
		service["security_opt"] = opts
		service["networks"] = []string{"tor-internal"}
		service["dns"] = []string{"127.0.0.1"} // Docker still resolves service names; external queries have no resolver.
		delete(service, "ports")               // ingress must use the managed reverse proxy, never host NAT
		env := map[string]any{}
		switch old := service["environment"].(type) {
		case nil:
		case map[string]any:
			for k, v := range old {
				env[k] = v
			}
		case []any:
			for _, item := range old {
				v, ok := item.(string)
				if !ok {
					return "", errors.New("invalid Tor service environment")
				}
				k, val, found := strings.Cut(v, "=")
				if !found {
					return "", errors.New("Tor egress refuses inherited environment")
				}
				env[k] = val
			}
		default:
			return "", errors.New("invalid Tor service environment")
		}
		for k, v := range torEgressProxyVars() {
			env[k] = v
		}
		service["environment"] = env
		services[name] = service
	}
	services[TorSidecarService] = map[string]any{
		"image": proxy.TorImage, "container_name": deploymentID + "_tor_socks", "restart": "unless-stopped",
		"user": "debian-tor", "read_only": true, "tmpfs": []string{"/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777"},
		"cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"},
		"networks": []string{"tor-internal", "tor-uplink"},
		"command":  []string{"tor", "--ignore-missing-torrc", "-f", "/tmp/impreza-empty-torrc", "--DataDirectory", "/tmp/tor", "--SocksPort", "0.0.0.0:9050", "--ClientOnly", "1", "--ClientRejectInternalAddresses", "1", "--SafeLogging", "1", "--Log", "notice stdout"},
	}
	doc["networks"] = map[string]any{
		"tor-internal": map[string]any{"external": true, "name": proxy.TorEgressNetworkName(deploymentID)},
		"tor-uplink":   map[string]any{},
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return "", errors.New("cannot encode Tor egress Compose")
	}
	return string(raw), nil
}
func torEgressProxyVars() map[string]string {
	return map[string]string{"ALL_PROXY": "socks5h://tor-socks:9050", "all_proxy": "socks5h://tor-socks:9050",
		"HTTP_PROXY": "socks5h://tor-socks:9050", "http_proxy": "socks5h://tor-socks:9050", "HTTPS_PROXY": "socks5h://tor-socks:9050", "https_proxy": "socks5h://tor-socks:9050",
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1"}
}
func (d *Docker) prepareTorEgress(ctx context.Context, id string) error {
	return proxy.EnsureTorEgressNetwork(ctx, id)
}
