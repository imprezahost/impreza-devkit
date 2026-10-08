package proxy

import (
	"strings"
	"testing"
)

// status-page-v1: reserved path prefixes served by the control plane
// on the app's own origins. The goldens below pin the rendered shape; the
// negative cases pin the validation the Apply path runs before any write.

func TestRenderFragmentPlatformRoutesOnion(t *testing.T) {
	got := renderFragment("dpl_aaaa1111bbbb2222", []Route{{
		OnionAddr: "abc123def456.onion",
		Upstream:  "dpl_aaaa1111bbbb2222:80",
		PlatformRoutes: []PlatformRoute{{
			Prefix:   "/status",
			Upstream: "https://api.imprezahost.com",
		}},
	}})
	// The platform handle must sit INSIDE the onion block, ahead of the
	// app's catch-all: everything the proxy-metrics/shield layer writes in
	// between is allowed to move.
	onionStart := strings.Index(got, "http://abc123def456.onion {")
	handle := strings.Index(got, "handle /status/* {")
	catchAll := strings.LastIndex(got, "reverse_proxy dpl_aaaa1111bbbb2222:80")
	if onionStart == -1 || handle == -1 || catchAll == -1 || !(onionStart < handle && handle < catchAll) {
		t.Fatalf("the platform handle must be inside the onion block ahead of the catch-all:\n%s", got)
	}
}

func TestRenderFragmentPlatformRoutesClearnetToo(t *testing.T) {
	got := renderFragment("dpl_aaaa1111bbbb2222", []Route{{
		Hostname:  "app.example.com",
		OnionAddr: "abc123def456.onion",
		Upstream:  "dpl_aaaa1111bbbb2222:80",
		PlatformRoutes: []PlatformRoute{{
			Prefix:   "/status/",
			Upstream: "https://api.imprezahost.com",
		}},
	}})
	// Both origins carry the handle; a trailing slash on the prefix is
	// normalized so the rendered matcher is exactly /status/*.
	want := "  handle /status/* {\n    reverse_proxy https://api.imprezahost.com\n  }\n"
	if strings.Count(got, want) != 2 {
		t.Fatalf("expected the platform handle on BOTH origins (clearnet + onion), got %d:\n%s", strings.Count(got, want), got)
	}
}

func TestRenderFragmentWithoutPlatformRoutesUnchanged(t *testing.T) {
	// An old server never sends the field: the fragment must render no
	// handle at all — byte-shape unchanged for existing deployments.
	withNil := renderFragment("dpl_aaaa1111bbbb2222", []Route{{Hostname: "app.example.com", Upstream: "dpl_a:80"}})
	if strings.Contains(withNil, "handle") {
		t.Fatalf("nil platform routes must render no handle:\n%s", withNil)
	}
}

func TestPlatformRouteValidate(t *testing.T) {
	cases := []struct {
		name string
		pr   PlatformRoute
		ok   bool
	}{
		{"good", PlatformRoute{Prefix: "/status", Upstream: "https://api.imprezahost.com"}, true},
		{"relative prefix", PlatformRoute{Prefix: "status", Upstream: "https://api.imprezahost.com"}, false},
		{"wildcard in prefix", PlatformRoute{Prefix: "/st*", Upstream: "https://api.imprezahost.com"}, false},
		{"space in prefix", PlatformRoute{Prefix: "/st atus", Upstream: "https://api.imprezahost.com"}, false},
		{"http upstream", PlatformRoute{Prefix: "/status", Upstream: "http://api.imprezahost.com"}, true},
		{"https with port", PlatformRoute{Prefix: "/status", Upstream: "https://api.imprezahost.com:8443"}, true},
		{"upstream with path", PlatformRoute{Prefix: "/status", Upstream: "https://api.imprezahost.com/base"}, false},
		{"no scheme", PlatformRoute{Prefix: "/status", Upstream: "api.imprezahost.com"}, false},
		{"container upstream", PlatformRoute{Prefix: "/status", Upstream: "dpl_x:80"}, false},
		{"space in upstream", PlatformRoute{Prefix: "/status", Upstream: "https://api.imprezahost.com x"}, false},
	}
	for _, c := range cases {
		err := c.pr.validate()
		if c.ok && err != nil {
			t.Errorf("%s: expected valid, got %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected refusal", c.name)
		}
	}
}
