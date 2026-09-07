package cmd

// Shared remediation text for IP_NOT_WHITELISTED failures. Both the
// `login` wizard and `doctor` hit the same wall — the calling machine's
// IP isn't on the key's whitelist — so they share one hint that:
//   1. deep-links to the API Management page (pre-filling the IP when we
//      know it, so the customer just clicks "Add my current IP"), and
//   2. points headless / CI / dynamic-IP / Tor callers at the per-key IP
//      modes that remove the IP step entirely (first-use lock / key-only).
//
// The modes are the Tier 0 escape hatch: a CLI/MCP client rarely runs
// from a stable, known IP, so telling the user to "whitelist your IP" is
// often a dead end — the better fix is to flip the key's IP mode.

import (
	"fmt"
	"net/url"
	"strings"
)

// portalAPIURL is the clientarea API Management page (keys, IP whitelist,
// per-key IP mode). Same URL the OpenAPI spec + READMEs point at.
const portalAPIURL = "https://portal.imprezahost.com/imprezaapi.php"

// ipWhitelistHint returns multi-line remediation for an IP_NOT_WHITELISTED
// error. When requestIP is non-empty it builds a deep-link that pre-fills
// that IP on the whitelist tab. Lines are prefixed so the block reads well
// when appended under an error/Detail.
func ipWhitelistHint(requestIP string) string {
	link := portalAPIURL
	if requestIP != "" {
		link = fmt.Sprintf("%s?addip=%s", portalAPIURL, url.QueryEscape(requestIP))
	}

	var b strings.Builder
	if requestIP != "" {
		fmt.Fprintf(&b, "Your IP %s isn't on this key's whitelist. Two ways to fix it:\n", requestIP)
	} else {
		b.WriteString("The calling IP isn't on this key's whitelist. Two ways to fix it:\n")
	}
	fmt.Fprintf(&b, "  - Whitelist it: %s\n", link)
	b.WriteString("  - Headless, CI, dynamic IP or Tor? Set this key's IP mode to ")
	b.WriteString("\"First-use lock\" or \"Key only\" in API Management — then there's no IP to keep in sync.")
	return b.String()
}
