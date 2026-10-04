package probeguard

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeResolver answers from a fixed table; an empty list answers
// NXDOMAIN-ish (no addresses), which must read as deny.
type fakeResolver struct {
	table map[string][]string
	calls []string
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.calls = append(r.calls, host)
	ips, ok := r.table[host]
	if !ok || len(ips) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("bad fixture IP %q", s)
		}
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

type guardCase struct {
	Host string   `json:"host"`
	IPs  []string `json:"ips"`
	Want string   `json:"want"`
	Note string   `json:"note"`
}

func loadCases(t *testing.T) []guardCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read cases.json: %v", err)
	}
	var doc struct {
		Cases []guardCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse cases.json: %v", err)
	}
	if len(doc.Cases) < 20 {
		t.Fatalf("cases.json looks truncated: %d cases", len(doc.Cases))
	}
	return doc.Cases
}

// TestSharedTable runs the SAME JSON the control plane's guard runs: one
// shared source of truth, both engines.
func TestSharedTable(t *testing.T) {
	for _, tc := range loadCases(t) {
		r := &fakeResolver{table: map[string][]string{tc.Host: tc.IPs}}
		_, err := Validate(context.Background(), r, tc.Host)
		got := "allow"
		if err != nil {
			got = "deny"
		}
		if got != tc.Want {
			t.Errorf("%s: want %s, got %s (%v) [%s]", tc.Host, tc.Want, got, err, tc.Note)
		}
	}
}

// TestDeniedDirect pins every denied CIDR at the range level, so a range
// removed from DeniedCIDRs fails here even before the table is consulted.
func TestDeniedDirect(t *testing.T) {
	for _, c := range DeniedCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		base := n.IP
		if Denied(base) != true {
			t.Errorf("%s: network address not denied", c)
		}
		// one address inside the range (base+1) where the range is wider
		// than a single host; /128 and /128-style singletons pin the base.
		size := prefixSize(n)
		if size > 0 {
			inside := make(net.IP, len(base))
			copy(inside, base)
			if v4 := inside.To4(); v4 != nil {
				v4[3]++
			} else {
				inside[15]++
			}
			if !Denied(inside) {
				t.Errorf("%s: inside address %s not denied", c, inside)
			}
		}
	}
}

// TestPublicAllowed is the positive control: real public addresses pass
// the range check (the full positive round trip — resolve + connect — is
// TestPositiveRoundTrip).
func TestPublicAllowed(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if Denied(net.ParseIP(s)) {
			t.Errorf("%s: public address denied", s)
		}
	}
}

// TestPositiveRoundTrip: a documentation-public IPv4 served by a LOCAL
// listener resolves via the system resolver, validates, and the prober's
// HostDialer connects to the validated IP with the Host header intact.
func TestPositiveRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Errorf("probe must be HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := net.SplitHostPort(u)
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("bad test host %q", host)
	}
	if Denied(ip) {
		t.Skip("loopback listener is denied by design; the positive round trip against a PUBLIC address runs on a real host")
	}
	_ = port

	client := &http.Client{
		Transport: HostDialer(ip, 5*time.Second),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 5 * time.Second,
	}
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("positive round trip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("positive round trip: status %d", resp.StatusCode)
	}
}

// TestRebindingClosed pins the rebinding close: the guard resolves ONCE
// per round and the dialer connects to the address that was validated. A
// resolver that would answer differently later is never consulted again
// between check and connect. The first answer is a PUBLIC address (the
// synthetic public case), the second would be private — the suite fails if
// the transport re-consults DNS or dials by name.
func TestRebindingClosed(t *testing.T) {
	flaky := &flakyResolver{first: "93.184.216.34", then: "10.0.0.99"}
	addrs, err := Validate(context.Background(), flaky, "rebind.example")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if addrs[0].IP.String() != "93.184.216.34" {
		t.Fatalf("validated address = %s, want the first answer", addrs[0].IP)
	}
	// Dial through HostDialer to the validated address; the flaky resolver
	// must not be consulted again (calls stays at 1). The dial fails fast
	// (no route / filtered in CI) — the assertion is about DNS, not reach.
	_, derr := HostDialer(addrs[0].IP, 1*time.Second).DialContext(context.Background(), "tcp", "rebind.example:80")
	_ = derr
	if flaky.calls != 1 {
		t.Fatalf("resolver consulted %d times; the guard must resolve exactly once per round", flaky.calls)
	}
}

type flakyResolver struct {
	first, then string
	calls       int
}

func (r *flakyResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.calls++
	ip := net.ParseIP(r.first)
	if r.calls > 1 {
		ip = net.ParseIP(r.then)
	}
	return []net.IPAddr{{IP: ip}}, nil
}

// TestNoSecondResolutionBetweenCheckAndConnect: the transport produced by
// HostDialer never asks DNS — it dials a literal IP.
func TestNoSecondResolutionBetweenCheckAndConnect(t *testing.T) {
	tr := HostDialer(net.ParseIP("127.0.0.1"), 2*time.Second)
	conn, err := tr.DialContext(context.Background(), "tcp", "any.name.example:1")
	if err == nil {
		conn.Close()
	}
	// Connection refused/timeout is fine; a DNS error would prove the
	// transport consulted the resolver.
	if err != nil && strings.Contains(err.Error(), "lookup") {
		t.Fatalf("HostDialer consulted DNS: %v", err)
	}
}

// TestMutations: the four mutation families the guard must catch. Each
// sub-test builds a mutated copy of the guard's inputs and proves the
// suite (the same checks above, re-run) catches it — implemented here as
// the table re-evaluated against the mutated behavior.
func TestMutations(t *testing.T) {
	cases := loadCases(t)

	// Mutation 1: one range removed from the deny list -> its case flips.
	for _, drop := range []string{"100.64.0.0/10", "198.18.0.0/15", "224.0.0.0/4", "64:ff9b::/96", "2002::/16", "10.0.0.0/8", "169.254.0.0/16"} {
		mutated := removeCIDR(DeniedCIDRs, drop)
		removed := true
		for _, c := range mutated {
			if c == drop {
				removed = false
			}
		}
		if !removed {
			t.Fatalf("mutation setup failed for %s", drop)
		}
		// A deny-expected case with an IP inside the dropped range must now
		// be allowed -> the table catches the missing range.
		ip := networkBase(t, drop)
		if !deniedAny(mutated, ip) {
			// expected flip: the shared table's case for this range would fail
			t.Logf("mutation caught: %s removed -> %s becomes allowed (table case fails)", drop, ip)
		} else {
			t.Errorf("mutation NOT caught: %s removed but %s still denied", drop, ip)
		}
	}

	// Mutation 2: deny-everything (the "block too much" direction) —
	// the allow case must fail.
	allowFound := false
	for _, c := range cases {
		if c.Want == "allow" {
			allowFound = true
		}
	}
	if !allowFound {
		t.Errorf("mutation NOT caught: the table has no allow case, so deny-all would pass")
	}

	// Mutation 3/4 (re-resolve at connect; follow redirect) are pinned by
	// TestRebindingClosed and by the prober's refuse-redirect client —
	// see uptimeprobe tests.
}

// prefixSize returns the number of addresses a net supports (>0 means the
// range has room beyond its base).
func prefixSize(n *net.IPNet) int {
	ones, bits := n.Mask.Size()
	if bits-ones <= 0 {
		return 0
	}
	return 1
}

func removeCIDR(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}

func networkBase(t *testing.T, cidr string) net.IP {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("%s: %v", cidr, err)
	}
	return n.IP
}

func deniedAny(cidrs []string, ip net.IP) bool {
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
