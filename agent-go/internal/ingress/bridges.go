package ingress

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ReasonPublicBridge: the interface of a default route is one Docker lists as
// a bridge, so exempting the host's own containers would exempt the internet.
const ReasonPublicBridge = "public_interface_exempt"

var (
	interfaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)
	networkID     = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

// DockerBridges lists the Linux bridge of every Docker bridge network: the
// com.docker.network.bridge.name option when set (docker0 for the default
// network), br-<first 12 of the id> otherwise. Names that are not valid
// interface names are dropped, never rendered.
func DockerBridges(ctx context.Context, run Runner) ([]string, error) {
	out, err := run(ctx, "docker", []string{"network", "ls", "--filter", "driver=bridge", "--format", "{{.ID}}"}, "")
	if err != nil {
		return nil, errors.New("docker networks unavailable")
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 || len(ids) > 256 {
		return nil, errors.New("docker networks unavailable")
	}
	out, err = run(ctx, "docker", append([]string{"network", "inspect", "--format",
		`{{.Id}}|{{index .Options "com.docker.network.bridge.name"}}`}, ids...), "")
	if err != nil {
		return nil, errors.New("docker networks unavailable")
	}
	seen := map[string]bool{}
	var bridges []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || !networkID.MatchString(id) {
			continue
		}
		// A network without the option prints nothing or "<no value>".
		if name = strings.TrimSpace(name); name == "" || name == "<no value>" {
			name = "br-" + id[:12]
		}
		if !interfaceName.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		bridges = append(bridges, name)
	}
	sort.Strings(bridges)
	return bridges, nil
}

// DefaultRouteInterfaces lists the interfaces of the IPv4 and IPv6 default
// routes, where the internet arrives.
func DefaultRouteInterfaces() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if interfaceName.MatchString(name) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if raw, err := os.ReadFile("/proc/net/route"); err == nil {
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 7 && f[1] == "00000000" && f[7] == "00000000" {
				add(f[0])
			}
		}
	}
	if raw, err := os.ReadFile("/proc/net/ipv6_route"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) == 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && f[9] != "lo" {
				add(f[9])
			}
		}
	}
	return out
}

func sameList(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
