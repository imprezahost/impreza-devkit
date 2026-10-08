package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const forkNetworkLabel = "com.impreza.preview-fork"

var forkBridge = regexp.MustCompile(`^br-[a-f0-9]{12}$`)

func forkChain(id string) string {
	h := sha256.Sum256([]byte(id))
	return "IPF" + hex.EncodeToString(h[:10])
}
func firewallCall(ctx context.Context, binary string, args ...string) error {
	if _, err := exec.CommandContext(ctx, binary, append([]string{"-w", "5"}, args...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("fork firewall operation failed (%s)", binary)
	}
	return nil
}

// Isolated gateways block routed IPv4/IPv6 and host access. This guard also
// blocks NEW connections to the ingress peer; only ingress and its replies pass.
// Reconcile it whenever Caddy reconnects so a changed peer address never leaves
// a stale address exception assigned to untrusted code.
func ConfigureForkFirewall(ctx context.Context, id string) error {
	// Bridged IPv4/IPv6 packets must traverse the verified firewall chains.
	for _, key := range []string{"net.bridge.bridge-nf-call-iptables", "net.bridge.bridge-nf-call-ip6tables"} {
		value, err := exec.CommandContext(ctx, "sysctl", "-n", key).Output()
		if err != nil || strings.TrimSpace(string(value)) != "1" {
			return errors.New("fork preview requires bridge firewall filtering for IPv4 and IPv6")
		}
	}
	n, err := inspectTorNetwork(ctx, id)
	if err != nil {
		return err
	}
	if n.Labels[forkNetworkLabel] != "true" || len(n.ID) != 64 {
		return errors.New("unverified fork network")
	}
	bridge := "br-" + n.ID[:12]
	if !forkBridge.MatchString(bridge) {
		return errors.New("invalid fork bridge")
	}
	peers := map[string]string{}
	for _, c := range n.Containers {
		if c.Name == ContainerName {
			peers["iptables"] = strings.Split(c.IPv4Address, "/")[0]
			peers["ip6tables"] = strings.Split(c.IPv6Address, "/")[0]
		}
	}
	preparing := []string{"-i", bridge, "-m", "comment", "--comment", "impreza-fork:" + id + ":preparing", "-j", "DROP"}
	jump := []string{"-i", bridge, "-m", "comment", "--comment", "impreza-fork:" + id, "-j", forkChain(id)}
	// Stop traffic while replacing rules. A partial failure leaves DROP installed.
	for _, binary := range []string{"iptables", "ip6tables"} {
		if peers[binary] == "" {
			return errors.New("fork ingress has no verified address")
		}
		if firewallCall(ctx, binary, append([]string{"-C", "DOCKER-USER"}, preparing...)...) != nil {
			if err := firewallCall(ctx, binary, append([]string{"-I", "DOCKER-USER", "1"}, preparing...)...); err != nil {
				return err
			}
		}
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		chain := forkChain(id)
		if firewallCall(ctx, binary, "-N", chain) != nil {
			if err := firewallCall(ctx, binary, "-L", chain); err != nil {
				return err
			}
		}
		if err := firewallCall(ctx, binary, "-F", chain); err != nil {
			return err
		}
		for _, rule := range [][]string{{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}, {"-s", peers[binary], "-j", "RETURN"}, {"-j", "DROP"}} {
			if err := firewallCall(ctx, binary, append([]string{"-A", chain}, rule...)...); err != nil {
				return err
			}
		}
		// DOCKER-USER ends with RETURN. Install before it, below the temporary
		// DROP at position 1; an appended rule would never be reached.
		for firewallCall(ctx, binary, append([]string{"-C", "DOCKER-USER"}, jump...)...) == nil {
			if err := firewallCall(ctx, binary, append([]string{"-D", "DOCKER-USER"}, jump...)...); err != nil {
				return err
			}
		}
		if err := firewallCall(ctx, binary, append([]string{"-I", "DOCKER-USER", "2"}, jump...)...); err != nil {
			return err
		}
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		if err := firewallCall(ctx, binary, append([]string{"-D", "DOCKER-USER"}, preparing...)...); err != nil {
			return err
		}
	}
	return nil
}

// A missing network can still have guards from an interrupted teardown. Remove
// only this identity's exact bridge/comment/jump shape, never a shared chain.
func clearForkFirewall(ctx context.Context, id string) error {
	if !torDeploymentID.MatchString(id) {
		return errors.New("invalid fork identity")
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		raw, err := exec.CommandContext(ctx, binary, "-w", "5", "-S", "DOCKER-USER").Output()
		if err != nil {
			continue
		} // legacy non-fork host may have no IPv6 chain
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 10 || fields[0] != "-A" || fields[1] != "DOCKER-USER" || fields[2] != "-i" || !forkBridge.MatchString(fields[3]) || fields[4] != "-m" || fields[5] != "comment" || fields[6] != "--comment" || fields[8] != "-j" {
				continue
			}
			comment := strings.Trim(fields[7], "\"")
			if (comment != "impreza-fork:"+id || fields[9] != forkChain(id)) && (comment != "impreza-fork:"+id+":preparing" || fields[9] != "DROP") {
				continue
			}
			fields[0] = "-D"
			fields[7] = comment
			if err := firewallCall(ctx, binary, fields...); err != nil {
				return err
			}
		}
		if firewallCall(ctx, binary, "-L", forkChain(id)) == nil {
			if err := firewallCall(ctx, binary, "-F", forkChain(id)); err != nil {
				return err
			}
			if err := firewallCall(ctx, binary, "-X", forkChain(id)); err != nil {
				return err
			}
		}
	}
	return nil
}
