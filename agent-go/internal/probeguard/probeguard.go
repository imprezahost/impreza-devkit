// Package probeguard is the destination guard for the uptime vantage
// prober: the Go port of the control plane's egress range list, plus the
// resolve-once-connect-to-the-IP discipline that closes DNS rebinding.
//
// The control-plane side of the contract resolves the same way on its
// evaluate tick; the vantage
// re-runs this guard on EVERY round against what DNS says right then, and
// connects to the exact address it validated — there is no second
// resolution between the check and the connection.
//
// One denied address among the A and AAAA answers refuses the whole
// target: a name that half-points inside our network is not half-safe.
package probeguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Protocol is announced by vantage agents only (the poller adds it after
// the control plane confirms the vantage mark; a customer agent never
// announces it).
const Protocol = "uptime-probe-v1"

// DeniedCIDRs are the ranges a probe may never connect to. Mirrors
// the control plane's own denied ranges plus everything PHP's
// FILTER_FLAG_NO_PRIV_RANGE|NO_RES_RANGE refuses: private, loopback,
// link-local (169.254/16 carries the metadata service), reserved,
// CGNAT, benchmarking, multicast, protocol assignments, discard,
// documentation, NAT64, 6to4, IPv6 loopback/ULA/link-local and the
// IPv4-mapped forms (the mapped check is explicit because Go parses
// ::ffff:10.0.0.1 into a 16-byte v4-in-v6 that rangeContains would
// otherwise miss).
var denyRanges = []struct{ cidr, class string }{
	// IPv4 — class is the ONLY thing a refusal may log: the
	// vantage's journal must never become a list of customer hostnames and
	// addresses.
	{"0.0.0.0/8", "reserved"}, {"10.0.0.0/8", "private"}, {"100.64.0.0/10", "cgnat"},
	{"127.0.0.0/8", "loopback"}, {"169.254.0.0/16", "linklocal"}, {"172.16.0.0/12", "private"},
	{"192.0.0.0/24", "reserved"}, {"192.0.2.0/24", "documentation"}, {"192.88.99.0/24", "reserved"},
	{"192.168.0.0/16", "private"}, {"198.18.0.0/15", "benchmark"}, {"198.51.100.0/24", "documentation"},
	{"203.0.113.0/24", "documentation"}, {"224.0.0.0/4", "multicast"}, {"240.0.0.0/4", "reserved"},
	// IPv6
	{"::1/128", "loopback"}, {"::/128", "reserved"}, {"64:ff9b::/96", "nat64"},
	{"64:ff9b:1::/48", "nat64"}, {"100::/64", "discard"}, {"2001:db8::/32", "documentation"},
	{"2002::/16", "sixtofour"}, {"fc00::/7", "ula"}, {"fe80::/10", "linklocal"},
	{"ff00::/8", "multicast"},
}

// DeniedCIDRs is the flat range list (kept for the shared test table and
// the mutation driver).
var DeniedCIDRs []string

var denied []net.IPNet
var deniedClass []string

func init() {
	for _, r := range denyRanges {
		_, n, err := net.ParseCIDR(r.cidr)
		if err != nil {
			panic("probeguard: unparseable denied CIDR " + r.cidr)
		}
		denied = append(denied, *n)
		deniedClass = append(deniedClass, r.class)
		DeniedCIDRs = append(DeniedCIDRs, r.cidr)
	}
}

// Refusal is a guard refusal whose Error() is SAFE TO LOG: it names the
// range class only, never the hostname nor the resolved address. Detail is
// for tests and the report; nothing prints it in production paths.
type Refusal struct {
	Class  string
	Detail string
}

func (r *Refusal) Error() string { return "probeguard: target refused (" + r.Class + ")" }

// Denied reports whether one address falls in a denied range. IPv4-mapped
// IPv6 addresses are unwrapped first so ::ffff:10.0.0.1 is refused as
// 10.0.0.1.
func Denied(ip net.IP) bool {
	return DenyClass(ip) != ""
}

// DenyClass returns the range class of the first denied range containing
// ip ("" when the address is publicly probeable). IPv4-mapped IPv6
// addresses are unwrapped first, so the class is that of the embedded
// address.
func DenyClass(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for i, n := range denied {
		if n.Contains(ip) {
			return deniedClass[i]
		}
	}
	return ""
}

// Resolver is the subset of net.Resolver the guard needs; tests inject a
// synthetic one to pin each range and the rebinding case.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Validate resolves host through r and returns the address list only when
// EVERY answer is publicly probeable. A host that does not resolve, or
// with any denied answer, returns an error — never a partial list.
func Validate(ctx context.Context, r Resolver, host string) ([]net.IPAddr, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || len(host) > 253 {
		return nil, errors.New("probeguard: empty or oversized host")
	}
	if ip := net.ParseIP(host); ip != nil {
		// An IP literal is checked directly — targets are hostnames by
		// shape on the server side; anything literal here is refused on
		// principle (the server never lists one) rather than trusted.
		return nil, &Refusal{Class: "ip-literal", Detail: host}
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("probeguard: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("probeguard: %s has no addresses", host)
	}
	for _, a := range addrs {
		if class := DenyClass(a.IP); class != "" {
			return nil, &Refusal{Class: class, Detail: host + " -> " + a.IP.String()}
		}
	}
	return addrs, nil
}

// HostDialer returns an http.Transport whose dialer connects to fixedIP
// while the request keeps the original Host header and TLS SNI (the URL
// still carries the hostname). This is the resolve-once half of the
// rebinding close: DNS is consulted exactly once per round, in Validate,
// and the connection goes to the address that was checked.
func HostDialer(fixedIP net.IP, timeout time.Duration) *http.Transport {
	d := &net.Dialer{Timeout: timeout}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Replace only the address; port stays as the URL dictated.
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, net.JoinHostPort(fixedIP.String(), strconv.Itoa(p)))
		},
		// No redirects are followed at the client level (see NewProber);
		// keep the connection single-use so nothing is pooled across
		// targets or rounds.
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
		Proxy:                 nil, // never a proxy for domain probes
	}
}

// SocksDialer builds a Transport whose connections go through the SOCKS5
// proxy at socksAddr (the local Tor). Used for .onion targets only; with
// no proxy reachable the caller must record err — an onion probe NEVER
// falls back to clearnet.
func SocksDialer(socksAddr string, timeout time.Duration) (*http.Transport, error) {
	d, err := socksDialer(socksAddr, timeout)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     false,
	}, nil
}

// URLForTarget builds the probe URL for one target row.
func URLForTarget(scheme, host string) string {
	u := &url.URL{Scheme: scheme, Host: host}
	return u.String()
}
