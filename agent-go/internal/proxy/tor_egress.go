package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const torNetworkLabel = "com.impreza.tor-egress"

var torDeploymentID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,60}$`)

func TorEgressNetworkName(id string) string { return "impreza-tor-" + id }

type torNetwork struct {
	Name       string
	Driver     string
	Internal   bool
	EnableIPv6 bool
	Options    map[string]string
	Labels     map[string]string
	Containers map[string]struct{ Name string }
}

func inspectTorNetwork(ctx context.Context, id string) (*torNetwork, error) {
	if !torDeploymentID.MatchString(id) {
		return nil, errors.New("invalid Tor network identity")
	}
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", TorEgressNetworkName(id)).Output()
	if err != nil {
		return nil, err
	}
	var nets []torNetwork
	if json.Unmarshal(out, &nets) != nil || len(nets) != 1 {
		return nil, errors.New("invalid Tor network inspection")
	}
	n := &nets[0]
	if n.Name != TorEgressNetworkName(id) || n.Driver != "bridge" || !n.Internal || !n.EnableIPv6 || n.Labels[torNetworkLabel] != id || n.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" || n.Options["com.docker.network.bridge.gateway_mode_ipv6"] != "isolated" {
		return nil, errors.New("existing Tor network does not enforce isolation")
	}
	return n, nil
}
func EnsureTorEgressNetwork(ctx context.Context, id string) error {
	if !torDeploymentID.MatchString(id) {
		return errors.New("invalid Tor network identity")
	}
	version, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return errors.New("cannot verify Docker isolated gateway support")
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(version)), ".")[0])
	if err != nil || major < 28 {
		return errors.New("Tor runtime egress requires Docker Engine 28 or newer")
	}
	// Never adopt or overwrite a colliding network. Docker's isolated gateway mode
	// leaves no host bridge address and refuses routed external IPv4 and IPv6.
	out, err := exec.CommandContext(ctx, "docker", "network", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		return errors.New("cannot inspect Tor networks")
	}
	exists := false
	for _, name := range strings.Fields(string(out)) {
		if name == TorEgressNetworkName(id) {
			exists = true
		}
	}
	if !exists {
		_, err = exec.CommandContext(ctx, "docker", "network", "create", "--driver", "bridge", "--internal", "--ipv6",
			"--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "--opt", "com.docker.network.bridge.gateway_mode_ipv6=isolated",
			"--label", torNetworkLabel+"="+id, TorEgressNetworkName(id)).CombinedOutput()
		if err != nil {
			return errors.New("Tor egress requires Docker isolated gateway support; no deployment started")
		}
	}
	_, err = inspectTorNetwork(ctx, id)
	return err
}
func ConnectTorEgressIngress(ctx context.Context, id string) error {
	n, err := inspectTorNetwork(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range n.Containers {
		if c.Name == ContainerName {
			return nil
		}
	}
	if err = exec.CommandContext(ctx, "docker", "network", "connect", n.Name, ContainerName).Run(); err != nil {
		return errors.New("cannot connect managed ingress to isolated app")
	}
	return nil
}
func RemoveTorEgressNetwork(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "docker", "network", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		return err
	}
	found := false
	for _, name := range strings.Fields(string(out)) {
		if name == TorEgressNetworkName(id) {
			found = true
		}
	}
	if !found {
		return nil
	}
	n, err := inspectTorNetwork(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range n.Containers {
		if c.Name == ContainerName {
			if err = exec.CommandContext(ctx, "docker", "network", "disconnect", n.Name, ContainerName).Run(); err != nil {
				return err
			}
		}
	}
	if err = exec.CommandContext(ctx, "docker", "network", "rm", n.Name).Run(); err != nil {
		return errors.New("Tor network still has endpoints; manual review required")
	}
	return nil
}
func (c *Caddy) reconcileTorEgressNetworks(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "network", "ls", "--filter", "label="+torNetworkLabel, "--format", "{{.Name}}").Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(out)) {
		id := strings.TrimPrefix(name, "impreza-tor-")
		if TorEgressNetworkName(id) != name {
			return errors.New("unexpected Tor network name")
		}
		if err = ConnectTorEgressIngress(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
