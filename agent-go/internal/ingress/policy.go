// Package ingress enforces the per-deployment ingress allowlists: the
// customer restricts who reaches the non-HTTP ports a deployment publishes
// (managed databases, RustDesk, game servers, DNS) to a list of CIDR sources.
//
// Every deployment is open until the control plane sends a restricted rule;
// the agent then keeps the complete desired state of each deployment on
// disk and renders one host-wide set of rules into chains it owns:
//
//   - IMPREZA-INGRESS, jumped from DOCKER-USER, for ports Docker publishes
//     (DNAT): it matches the original destination port of the connection
//     (conntrack --ctorigdstport, --ctdir ORIGINAL);
//   - IMPREZA-INGRESS-HOST, jumped from INPUT, for network_mode: host and for
//     the docker-proxy path (IPv6 on a host whose Docker does not program
//     ip6tables lands on the host's listener, never in DOCKER-USER).
//
// Both chains exist in both families. ESTABLISHED/RELATED returns first, so
// the agent's own outbound control channel and every existing flow are never
// cut; loopback and the Docker bridges return too, so the host's own
// containers keep reaching the service. Each chain is replaced in a single
// iptables-restore --noflush transaction (no instant with half a rule set);
// a failure leaves the last good set live and is reported, never green.
// Only these two chains and the two jumps are ever touched: no flush or
// reorder of anyone else's rules.
package ingress

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Limits enforced here and again by the control plane.
const (
	MaxRules          = 32
	MaxSourcesPerRule = 64
	MaxSources        = 512
)

var deploymentIDPattern = regexp.MustCompile(`^dpl_[a-zA-Z0-9_-]{1,28}$`)

// reservedPorts can never be restricted by a deployment rule: SSH (a
// lockout of the host) and the ports the shared Caddy proxy publishes.
var reservedPorts = map[string]bool{"tcp/22": true, "tcp/80": true, "tcp/443": true, "udp/443": true}

// Rule restricts one published port of one deployment to Sources.
type Rule struct {
	Port     int      `json:"port"`
	Protocol string   `json:"protocol"`
	Sources  []string `json:"sources"`
}

// Policy is the complete desired state of one deployment. Rules lists only
// restricted ports; no rule means the deployment is open.
type Policy struct {
	DeploymentID string `json:"deployment_id"`
	Revision     uint32 `json:"revision"`
	Rules        []Rule `json:"rules"`
}

// ValidationError carries a stable code (reported to the control plane) and
// an English message.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(code, format string, args ...any) error {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Normalize validates a policy as received (from the server or from disk)
// and returns it in canonical form: rules sorted by port and protocol,
// sources sorted per family. Nothing is trusted: the server validated the
// same payload, and the agent validates it again before any command runs.
func Normalize(p Policy) (Policy, error) {
	if !deploymentIDPattern.MatchString(p.DeploymentID) {
		return Policy{}, invalid("INVALID_DEPLOYMENT", "Invalid deployment id.")
	}
	if p.Revision == 0 {
		return Policy{}, invalid("INVALID_REVISION", "The revision must be a positive integer.")
	}
	if len(p.Rules) > MaxRules {
		return Policy{}, invalid("TOO_MANY_RULES", "At most %d restricted ports per deployment.", MaxRules)
	}
	out := Policy{DeploymentID: p.DeploymentID, Revision: p.Revision, Rules: []Rule{}}
	seenRule := map[string]bool{}
	total := 0
	for index, r := range p.Rules {
		proto := r.Protocol
		if proto != "tcp" && proto != "udp" {
			return Policy{}, invalid("INVALID_PROTOCOL", "The protocol must be tcp or udp.")
		}
		if r.Port < 1 || r.Port > 65535 {
			return Policy{}, invalid("INVALID_PORT", "The port must be between 1 and 65535.")
		}
		key := fmt.Sprintf("%s/%d", proto, r.Port)
		if reservedPorts[key] {
			return Policy{}, invalid("RESERVED_PORT", "Port %s is reserved and cannot be restricted by a deployment rule.", key)
		}
		if seenRule[key] {
			return Policy{}, invalid("DUPLICATE_RULE", "Port %s appears more than once.", key)
		}
		seenRule[key] = true
		sources, err := normalizeSources(r.Sources, fmt.Sprintf("rules[%d].sources", index))
		if err != nil {
			return Policy{}, err
		}
		total += len(sources)
		if total > MaxSources {
			return Policy{}, invalid("TOO_MANY_SOURCES", "At most %d sources per deployment.", MaxSources)
		}
		out.Rules = append(out.Rules, Rule{Port: r.Port, Protocol: proto, Sources: sources})
	}
	sort.Slice(out.Rules, func(i, j int) bool {
		if out.Rules[i].Port != out.Rules[j].Port {
			return out.Rules[i].Port < out.Rules[j].Port
		}
		return out.Rules[i].Protocol < out.Rules[j].Protocol
	})
	return out, nil
}

// NormalizeSources parses CIDR sources: IPv4 or IPv6, host bits zero (a
// prefix with host bits set is refused rather than silently widened), no
// zone, no IPv4-mapped or dotted IPv6, no duplicates, never a whole address
// family, alone or as a union ("open" is a state, not a rule). The result is
// sorted, IPv4 first. Messages name the position, never the value: a refused
// source is often the customer's own address.
func NormalizeSources(raw []string) ([]string, error) {
	return normalizeSources(raw, "sources")
}

func normalizeSources(raw []string, where string) ([]string, error) {
	if len(raw) == 0 {
		return nil, invalid("NO_SOURCES", "A restricted port needs at least one source (%s).", where)
	}
	if len(raw) > MaxSourcesPerRule {
		return nil, invalid("TOO_MANY_SOURCES", "At most %d sources per port (%s).", MaxSourcesPerRule, where)
	}
	seen := map[netip.Prefix]int{}
	var prefixes []netip.Prefix
	for i, s := range raw {
		at := fmt.Sprintf("%s[%d]", where, i)
		if len(s) > 49 || strings.TrimSpace(s) != s || s == "" {
			return nil, invalid("INVALID_SOURCE", "%s must be a CIDR such as 203.0.113.0/24 or 2001:db8::/48.", at)
		}
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, invalid("INVALID_SOURCE", "%s must be a CIDR such as 203.0.113.0/24 or 2001:db8::/48.", at)
		}
		addr := prefix.Addr()
		if addr.Zone() != "" || addr.Is4In6() {
			return nil, invalid("INVALID_SOURCE", "%s: IPv6 zones and IPv4-mapped IPv6 sources are not accepted.", at)
		}
		if addr.Is6() && strings.Contains(s, ".") {
			return nil, invalid("INVALID_SOURCE", "%s: IPv6 sources with an embedded IPv4 address are not accepted; write the IPv6 in hex groups.", at)
		}
		if prefix.Masked() != prefix {
			return nil, invalid("HOST_BITS_SET", "%s has host bits set; use the network address of the prefix.", at)
		}
		if prefix.Bits() == 0 {
			return nil, invalid("SOURCE_TOO_BROAD", "%s covers every address, which is not a restriction; set the port open instead.", at)
		}
		if first, dup := seen[prefix]; dup {
			return nil, invalid("DUPLICATE_SOURCE", "%s repeats %s[%d].", at, where, first)
		}
		seen[prefix] = i
		prefixes = append(prefixes, prefix)
	}
	for _, v4 := range []bool{true, false} {
		var family []netip.Prefix
		for _, p := range prefixes {
			if p.Addr().Is4() == v4 {
				family = append(family, p)
			}
		}
		if len(family) > 0 && coversFamily(family, v4) {
			name := "IPv6"
			if v4 {
				name = "IPv4"
			}
			return nil, invalid("SOURCE_TOO_BROAD", "The %s sources of %s together cover every address, which is not a restriction; set the port open instead.", name, where)
		}
	}
	sort.Slice(prefixes, func(i, j int) bool {
		a, b := prefixes[i], prefixes[j]
		if a.Addr().Is4() != b.Addr().Is4() {
			return a.Addr().Is4()
		}
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c < 0
		}
		return a.Bits() < b.Bits()
	})
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p.String()
	}
	return out, nil
}

// coversFamily reports whether the prefixes together cover the whole family
// (0.0.0.0/1 + 128.0.0.0/1 is "open" in disguise).
func coversFamily(prefixes []netip.Prefix, v4 bool) bool {
	root := netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	if v4 {
		root = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	}
	return covers(root, prefixes)
}

func covers(target netip.Prefix, prefixes []netip.Prefix) bool {
	inside := false
	for _, p := range prefixes {
		if p.Bits() <= target.Bits() && p.Contains(target.Addr()) {
			return true
		}
		if p.Bits() > target.Bits() && target.Contains(p.Addr()) {
			inside = true
		}
	}
	if !inside || target.Bits() >= target.Addr().BitLen() {
		return false
	}
	left := netip.PrefixFrom(target.Addr(), target.Bits()+1)
	raw := target.Addr().AsSlice()
	raw[target.Bits()/8] |= 0x80 >> (target.Bits() % 8)
	rightAddr, _ := netip.AddrFromSlice(raw)
	right := netip.PrefixFrom(rightAddr, target.Bits()+1)
	return covers(left, prefixes) && covers(right, prefixes)
}
