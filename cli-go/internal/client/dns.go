package client

import (
	"context"
	"fmt"
	"net/url"
)

// DnsAddRequest is the body for POST /v1/domains/{name}/dns.
//
// The server accepts the standard set of DNS record fields. Priority
// is required for MX/SRV; TTL must be ≥ 7200 (server-enforced).
type DnsAddRequest struct {
	Type     string `json:"type"`               // A / AAAA / CNAME / MX / TXT / NS / SRV
	Host     string `json:"host"`               // record host name (e.g. "www" or "@" for apex)
	Value    string `json:"value"`              // record value (IP, hostname, text, etc.)
	TTL      int    `json:"ttl,omitempty"`      // seconds; server requires ≥ 7200
	Priority int    `json:"priority,omitempty"` // required for MX/SRV
}

// DnsUpdateRequest is the body for PUT /v1/domains/{name}/dns. The
// server identifies the record to update by the (type, host, old_value)
// tuple rather than by id (DNS records don't have stable URL ids in
// the API surface).
type DnsUpdateRequest struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
	TTL      int    `json:"ttl,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// DnsDeleteRequest is the body for DELETE /v1/domains/{name}/dns. Same
// rationale as DnsUpdateRequest — records are identified by content.
type DnsDeleteRequest struct {
	Type  string `json:"type"`
	Host  string `json:"host"`
	Value string `json:"value"`
}

// DomainDnsAdd creates a new DNS record on the domain. Server returns
// `{status: "Success", msg: "Record added successfully."}` — no
// record id is echoed back; caller can re-list to find the new entry.
func (c *Client) DomainDnsAdd(ctx context.Context, domain string, req DnsAddRequest) error {
	path := fmt.Sprintf("/v1/domains/%s/dns", url.PathEscape(domain))
	return c.Post(ctx, path, req, nil)
}

// DomainDnsUpdate edits an existing record. Matches on (type, host,
// old_value); new_value replaces the value, ttl/priority replace the
// metadata if non-zero.
func (c *Client) DomainDnsUpdate(ctx context.Context, domain string, req DnsUpdateRequest) error {
	path := fmt.Sprintf("/v1/domains/%s/dns", url.PathEscape(domain))
	return c.Put(ctx, path, req, nil)
}

// DomainDnsDelete removes a record matched by (type, host, value).
func (c *Client) DomainDnsDelete(ctx context.Context, domain string, req DnsDeleteRequest) error {
	path := fmt.Sprintf("/v1/domains/%s/dns", url.PathEscape(domain))
	return c.Delete(ctx, path, req)
}

// DomainDnsActivate enables DNS management for a domain (turns on
// Impreza's DNS server cluster for this zone). Required before the
// first add/update — server returns INVALID_REQUEST otherwise.
func (c *Client) DomainDnsActivate(ctx context.Context, domain string) error {
	path := fmt.Sprintf("/v1/domains/%s/dns/activate", url.PathEscape(domain))
	return c.Post(ctx, path, nil, nil)
}
