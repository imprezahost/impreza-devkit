package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Domain is the response from GET /v1/domains/{name}.
type Domain struct {
	Domain       string   `json:"domain,omitempty"`
	Status       string   `json:"status,omitempty"`
	Registrar    string   `json:"registrar,omitempty"`
	Nameservers  []string `json:"nameservers,omitempty"`
	RegisteredAt string   `json:"registered_at,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	AutoRenew    bool     `json:"auto_renew,omitempty"`
	Lock         bool     `json:"lock,omitempty"`
	IDProtection bool     `json:"id_protection,omitempty"`
	ServiceID    int      `json:"service_id,omitempty"`
}

// DomainShow wraps GET /v1/domains/{name}.
func (c *Client) DomainShow(ctx context.Context, name string) (*Domain, error) {
	var d Domain
	if err := c.Get(ctx, "/v1/domains/"+url.PathEscape(name), nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// DomainAvailability is one row of the availability check.
type DomainAvailability struct {
	Domain    string  `json:"domain"`
	Available bool    `json:"available"`
	Premium   bool    `json:"premium,omitempty"`
	Price     float64 `json:"price,omitempty"`
	Currency  string  `json:"currency,omitempty"`
}

// domainCheckResponse unwraps the {availability: {...}} shape the
// server uses (a map of domain → bool availability).
type domainCheckResponse struct {
	Availability map[string]bool `json:"availability"`
	Pricing      map[string]struct {
		Register float64 `json:"register"`
		Currency string  `json:"currency"`
	} `json:"pricing,omitempty"`
}

// DomainCheck wraps GET /v1/domains/check. Server expects `domains`
// (plural) as a comma-joined query parameter.
func (c *Client) DomainCheck(ctx context.Context, domains []string) ([]DomainAvailability, error) {
	q := url.Values{"domains": []string{strings.Join(domains, ",")}}
	var resp domainCheckResponse
	if err := c.Get(ctx, "/v1/domains/check", q, &resp); err != nil {
		return nil, err
	}
	out := make([]DomainAvailability, 0, len(resp.Availability))
	// Preserve the caller's order so the table output matches arg order.
	for _, d := range domains {
		entry := DomainAvailability{Domain: d, Available: resp.Availability[d]}
		if p, ok := resp.Pricing[d]; ok {
			entry.Price = p.Register
			entry.Currency = p.Currency
		}
		out = append(out, entry)
	}
	return out, nil
}

// DomainPricing wraps GET /v1/domains/pricing with a single-TLD filter.
// The server doesn't expose a per-TLD route; we filter the matrix
// endpoint down to one row.
func (c *Client) DomainPricing(ctx context.Context, tld string) (*TldPricing, error) {
	clean := strings.TrimPrefix(tld, ".")
	rows, err := c.CatalogTlds(ctx, []string{clean})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no pricing entry for .%s", clean)
	}
	row := rows[0]
	return &row, nil
}

// DnsRecord is one row of GET /v1/domains/{name}/dns. Server returns
// `ttl` as a string and `priority` as either null or a string-ish
// numeric. We decode them as string for maximum compatibility; the
// CLI renders them as-is.
type DnsRecord struct {
	ID       int    `json:"id,omitempty"`
	Type     string `json:"type"`
	Host     string `json:"host,omitempty"` // server emits "host", not "name"
	Name     string `json:"name,omitempty"` // older alias; keep for forward compat
	Value    string `json:"value"`
	TTL      string `json:"ttl,omitempty"`
	Priority string `json:"priority,omitempty"`
}

// DisplayName returns Host if set, falling back to Name. The server
// uses "host" today; older shapes used "name". Keeps the CLI agnostic.
func (r DnsRecord) DisplayName() string {
	if r.Host != "" {
		return r.Host
	}
	return r.Name
}

// dnsRecordsResponse unwraps {records: [...]}.
type dnsRecordsResponse struct {
	Records []DnsRecord `json:"records"`
}

// DomainDnsList wraps GET /v1/domains/{name}/dns.
func (c *Client) DomainDnsList(ctx context.Context, domain string) ([]DnsRecord, error) {
	var resp dnsRecordsResponse
	path := fmt.Sprintf("/v1/domains/%s/dns", url.PathEscape(domain))
	if err := c.Get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Records, nil
}

// ═══════════════════════════════════════════════════════════════════
//             Phase 7.5.1 — advanced domain write surface
// ═══════════════════════════════════════════════════════════════════
//
// Nine verbs that close the Python-CLI parity gap: register, transfer,
// set-nameservers, lock, unlock, id-protection, raa-verify, gdpr-auth,
// transfer-approval. Wire contract mirrors the Python SDK at
// sdk-python/impreza/resources/domains.py — same endpoints, same body
// shapes, same response shapes.

// DomainRegisterRequest is the body for POST /v1/domains/register.
// Nameservers is optional; the server falls back to Impreza-default
// nameservers when omitted.
type DomainRegisterRequest struct {
	Domain      string   `json:"domain"`
	Years       int      `json:"years"`
	Nameservers []string `json:"nameservers,omitempty"`
}

// DomainTransferRequest is the body for POST /v1/domains/transfer.
// The EPP / authorisation code comes from the losing registrar.
type DomainTransferRequest struct {
	Domain  string `json:"domain"`
	EppCode string `json:"epp_code"`
	Years   int    `json:"years"`
}

// DomainOrderResult is the shape both /register and /transfer return.
// Same fields as the Python SDK's DomainRegistration + DomainTransfer
// models (those are structurally identical — we collapse them here).
type DomainOrderResult struct {
	OrderID   int     `json:"order_id"`
	InvoiceID int     `json:"invoice_id"`
	Domain    string  `json:"domain"`
	Years     int     `json:"years"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status,omitempty"`
	Message   string  `json:"message,omitempty"`
}

// DomainRegister wraps POST /v1/domains/register. The amount field on
// the response carries the currency-denominated charge against the
// account balance.
func (c *Client) DomainRegister(ctx context.Context, req DomainRegisterRequest) (*DomainOrderResult, error) {
	var resp DomainOrderResult
	if err := c.Post(ctx, "/v1/domains/register", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DomainTransfer wraps POST /v1/domains/transfer.
func (c *Client) DomainTransfer(ctx context.Context, req DomainTransferRequest) (*DomainOrderResult, error) {
	var resp DomainOrderResult
	if err := c.Post(ctx, "/v1/domains/transfer", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DomainSetNameservers wraps PUT /v1/domains/{name}/nameservers. The
// server requires ≥ 2 nameservers; callers should validate upstream so
// the failure message is friendlier than the 400 we'd otherwise see.
func (c *Client) DomainSetNameservers(ctx context.Context, domain string, nameservers []string) error {
	path := fmt.Sprintf("/v1/domains/%s/nameservers", url.PathEscape(domain))
	body := map[string][]string{"nameservers": nameservers}
	return c.Put(ctx, path, body, nil)
}

// DomainLock wraps POST /v1/domains/{name}/lock. No-op if already
// locked (server returns 200 either way).
func (c *Client) DomainLock(ctx context.Context, domain string) error {
	path := fmt.Sprintf("/v1/domains/%s/lock", url.PathEscape(domain))
	return c.Post(ctx, path, nil, nil)
}

// DomainUnlock wraps DELETE /v1/domains/{name}/lock. Returns the EPP /
// authorisation code that the server hands back with the unlock — that
// code authorises transferring the domain *away* from Impreza, so it
// is sensitive and the CLI prints it once then forgets it.
//
// Uses do() directly because the public Delete() helper discards the
// response body (most DELETE endpoints return 204 or empty data); the
// unlock endpoint is the only DELETE that carries data we need.
func (c *Client) DomainUnlock(ctx context.Context, domain string) (string, error) {
	path := fmt.Sprintf("/v1/domains/%s/lock", url.PathEscape(domain))
	var resp struct {
		EppCode string `json:"epp_code"`
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, nil, &resp); err != nil {
		return "", err
	}
	return resp.EppCode, nil
}

// DomainPurchaseIDProtection wraps POST /v1/domains/{name}/id-protection.
// Pays from account balance. Return shape is registrar-dependent — we
// pass it through as a free-form map so the CLI can render whatever
// fields the upstream emits (typically invoice_id + amount, sometimes
// also an order_id or a status string).
func (c *Client) DomainPurchaseIDProtection(ctx context.Context, domain string) (map[string]any, error) {
	path := fmt.Sprintf("/v1/domains/%s/id-protection", url.PathEscape(domain))
	var resp map[string]any
	if err := c.Post(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// DomainResendRAAVerification wraps POST /v1/domains/{name}/raa-verify.
// Re-sends the ICANN RAA email-verification message to the registrant.
// Without confirming the address within 15 days of registration, ICANN
// suspends the domain — this verb resends if the user lost the original.
func (c *Client) DomainResendRAAVerification(ctx context.Context, domain string) error {
	path := fmt.Sprintf("/v1/domains/%s/raa-verify", url.PathEscape(domain))
	return c.Post(ctx, path, nil, nil)
}

// DomainResendGDPRAuth wraps POST /v1/domains/{name}/gdpr-auth.
// Re-sends the GDPR data-processing authorisation email (required for
// EU-resident registrants on certain TLDs).
func (c *Client) DomainResendGDPRAuth(ctx context.Context, domain string) error {
	path := fmt.Sprintf("/v1/domains/%s/gdpr-auth", url.PathEscape(domain))
	return c.Post(ctx, path, nil, nil)
}

// DomainResendTransferApproval wraps POST
// /v1/domains/{name}/transfer-approval. Re-sends the inbound-transfer
// approval email that the gaining registrar (Impreza) needs the
// registrant to click before the transfer completes.
func (c *Client) DomainResendTransferApproval(ctx context.Context, domain string) error {
	path := fmt.Sprintf("/v1/domains/%s/transfer-approval", url.PathEscape(domain))
	return c.Post(ctx, path, nil, nil)
}
