package ingress

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Chains owned by the ingress firewall, and where they are jumped from.
const (
	Chain           = "IMPREZA-INGRESS"      // jumped from DOCKER-USER
	HostChain       = "IMPREZA-INGRESS-HOST" // jumped from INPUT
	DockerUserChain = "DOCKER-USER"
	InputChain      = "INPUT"
)

// Family is v4 (iptables) or v6 (ip6tables).
type Family string

const (
	V4 Family = "v4"
	V6 Family = "v6"
)

// What returns first: every packet of an existing flow (the agent's outbound
// control channel is never affected), loopback, and the bridges Docker
// itself lists (the host's own containers keep reaching the service, as
// today). Only those exact interfaces: a wildcard such as br+ would also
// exempt a host whose public interface is a bridge (br0).
func forwardPreamble(bridges []string) [][]string {
	out := [][]string{{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}}
	for _, b := range bridges {
		out = append(out, []string{"-i", b, "-j", "RETURN"})
	}
	return out
}

func hostPreamble(bridges []string) [][]string {
	out := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
		{"-i", "lo", "-j", "RETURN"},
	}
	for _, b := range bridges {
		out = append(out, []string{"-i", b, "-j", "RETURN"})
	}
	return out
}

// restricted is one (port, protocol) with the union of the sources of every
// deployment that restricts it (normally exactly one deployment).
type restricted struct {
	port    int
	proto   string
	sources []netip.Prefix
}

func merge(policies []Policy) []restricted {
	byKey := map[string]*restricted{}
	for _, p := range policies {
		for _, r := range p.Rules {
			key := fmt.Sprintf("%s/%d", r.Protocol, r.Port)
			entry := byKey[key]
			if entry == nil {
				entry = &restricted{port: r.Port, proto: r.Protocol}
				byKey[key] = entry
			}
			for _, s := range r.Sources {
				prefix, err := netip.ParsePrefix(s)
				if err == nil {
					entry.sources = append(entry.sources, prefix)
				}
			}
		}
	}
	out := make([]restricted, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].port != out[j].port {
			return out[i].port < out[j].port
		}
		return out[i].proto < out[j].proto
	})
	return out
}

// Rules renders both chains for one family: for each restricted port, a
// RETURN per allowed source of that family, then a DROP. A port whose
// sources are all of the other family is dropped entirely in this family.
func Rules(family Family, policies []Policy, bridges []string) (forward, host [][]string) {
	forward = forwardPreamble(bridges)
	host = hostPreamble(bridges)
	for _, r := range merge(policies) {
		port := fmt.Sprint(r.port)
		dnat := []string{"-p", r.proto, "-m", "conntrack", "--ctstate", "DNAT", "--ctdir", "ORIGINAL", "--ctorigdstport", port}
		local := []string{"-p", r.proto, "-m", r.proto, "--dport", port}
		seen := map[netip.Prefix]bool{}
		for _, s := range r.sources {
			if s.Addr().Is4() != (family == V4) || seen[s] {
				continue
			}
			seen[s] = true
			forward = append(forward, append(append([]string{}, dnat...), "-s", s.String(), "-j", "RETURN"))
			host = append(host, append(append([]string{}, local...), "-s", s.String(), "-j", "RETURN"))
		}
		forward = append(forward, append(append([]string{}, dnat...), "-j", "DROP"))
		host = append(host, append(append([]string{}, local...), "-j", "DROP"))
	}
	forward = append(forward, []string{"-j", "RETURN"})
	host = append(host, []string{"-j", "RETURN"})
	return forward, host
}

// restoreInput is the single iptables-restore --noflush transaction that
// replaces both owned chains: declaring a chain resets it, and the table
// commit is atomic, so there is never an instant with half a rule set and
// no other chain is touched.
func restoreInput(forward, host [][]string) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	b.WriteString(":" + Chain + " - [0:0]\n")
	b.WriteString(":" + HostChain + " - [0:0]\n")
	for _, r := range forward {
		b.WriteString("-A " + Chain + " " + strings.Join(r, " ") + "\n")
	}
	for _, r := range host {
		b.WriteString("-A " + HostChain + " " + strings.Join(r, " ") + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}
