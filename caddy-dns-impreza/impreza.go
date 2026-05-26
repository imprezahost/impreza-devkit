// Package impreza implements a Caddy DNS provider that proxies ACME
// DNS-01 challenges through the Impreza Platform's public API instead
// of holding the upstream provider's credentials locally. See the
// package README for the threat-model rationale.
package impreza

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/libdns/libdns"
)

// Provider is the Caddy/libdns provider for Impreza Platform's
// proxied DNS-01 challenge endpoints.
//
// Caddy's ACME client invokes (only) AppendRecords + DeleteRecords on
// us — we implement the rest of the libdns.Provider surface as no-ops
// to satisfy the interface without exposing data the operator hasn't
// chosen to surface.
type Provider struct {
	// AgentID identifies the caller to the Impreza API. Same value the
	// agent's poll loop uses in the X-Agent-Id header. Typically read
	// from {env.IMPREZA_AGENT_ID} via the Caddyfile.
	AgentID string `json:"agent_id,omitempty"`

	// AgentSecret authenticates the caller against the Impreza API.
	// Same value the agent's poll loop uses in the X-Agent-Secret
	// header. Typically read from {env.IMPREZA_AGENT_SECRET}.
	AgentSecret string `json:"agent_secret,omitempty"`

	// BaseURL of the Impreza public API. Default
	// "https://api.imprezahost.com" — override for staging or
	// self-hosted control planes. Read from {env.IMPREZA_API_URL}
	// when set on the env-file, falls back to default otherwise.
	BaseURL string `json:"base_url,omitempty"`

	// HTTPClient is overridable for tests. Production uses
	// http.DefaultClient with a per-request context timeout.
	HTTPClient *http.Client `json:"-"`
}

// CaddyModule registers us as `dns.providers.impreza`. Caddy resolves
// `tls { dns impreza ... }` blocks to this module by id.
func (Provider) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "dns.providers.impreza",
		New: func() caddy.Module { return new(Provider) },
	}
}

// Provision is called by Caddy after JSON unmarshal but before any
// requests. We use it to resolve placeholders (env vars) so config
// reloads pick up env-file changes without restarting Caddy.
func (p *Provider) Provision(ctx caddy.Context) error {
	repl := caddy.NewReplacer()
	p.AgentID = repl.ReplaceAll(p.AgentID, "")
	p.AgentSecret = repl.ReplaceAll(p.AgentSecret, "")
	p.BaseURL = repl.ReplaceAll(p.BaseURL, "")
	if p.BaseURL == "" {
		p.BaseURL = "https://api.imprezahost.com"
	}
	// Validate. Empty creds at provision time is fatal — Caddy would
	// fail every cert issuance otherwise, with a more confusing error.
	if p.AgentID == "" || p.AgentSecret == "" {
		return fmt.Errorf("caddy-dns-impreza: agent_id and agent_secret are required")
	}
	if p.HTTPClient == nil {
		p.HTTPClient = http.DefaultClient
	}
	return nil
}

// AppendRecords is called by Caddy's ACME client to PRESENT the
// DNS-01 challenge. We only act on TXT records — anything else is
// silently dropped because Caddy doesn't request non-TXT for ACME and
// we deliberately don't expose write access to other record types.
//
// The fqdn we send to the server is `record_name + "." + zone`,
// reassembled from libdns's split (which strips the zone suffix from
// `record.Name`). The server validates that the resulting fqdn is in
// an operator-managed zone AND that this agent owns a deployment with
// the corresponding hostname — defense in depth against an agent
// trying to issue a cert for a domain it doesn't legitimately serve.
func (p *Provider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(recs))
	for _, r := range recs {
		rr := r.RR()
		if !strings.EqualFold(rr.Type, "TXT") {
			continue
		}
		fqdn := joinFQDN(rr.Name, zone)
		if err := p.call(ctx, "/v1/agent/dns-challenge/present", map[string]string{
			"fqdn":  fqdn,
			"value": rr.Data,
		}); err != nil {
			return out, fmt.Errorf("present %s: %w", fqdn, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteRecords is called by Caddy's ACME client to CLEAN UP a DNS-01
// challenge. Best-effort: cleanup failures are logged + ignored so a
// transient API hiccup can't poison a successful issuance. Stale
// records age out via the short TTL set server-side.
func (p *Provider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(recs))
	for _, r := range recs {
		rr := r.RR()
		if !strings.EqualFold(rr.Type, "TXT") {
			continue
		}
		fqdn := joinFQDN(rr.Name, zone)
		if err := p.call(ctx, "/v1/agent/dns-challenge/cleanup", map[string]string{
			"fqdn": fqdn,
		}); err != nil {
			// Don't fail; just skip from "successfully deleted" set.
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// GetRecords returns an empty set: the Impreza API doesn't (and won't)
// expose zone listing to agents. Caddy doesn't call this on the ACME
// happy path; the implementation exists only to satisfy
// libdns.RecordGetter so xcaddy treats us as a complete Provider.
func (p *Provider) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	return nil, nil
}

// SetRecords is implemented as Append (= upsert) for the same reason
// AppendRecords exists: ACME challenges treat present as idempotent
// "this value is now live", which is the SetRecords semantics anyway.
func (p *Provider) SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.AppendRecords(ctx, zone, recs)
}

// UnmarshalCaddyfile parses the Caddyfile syntax. Two forms accepted:
//
//   dns impreza <agent_id> <agent_secret>            // compact, BaseURL default
//   dns impreza <agent_id> <agent_secret> <base_url> // compact w/ override
//   dns impreza {                                    // block form
//       agent_id <agent_id>
//       agent_secret <agent_secret>
//       base_url <url>
//   }
func (p *Provider) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		// Compact form arguments after `impreza`.
		args := d.RemainingArgs()
		switch len(args) {
		case 0:
			// Block-only form; fall through to subdirectives.
		case 1:
			p.AgentID = args[0]
		case 2:
			p.AgentID = args[0]
			p.AgentSecret = args[1]
		case 3:
			p.AgentID = args[0]
			p.AgentSecret = args[1]
			p.BaseURL = args[2]
		default:
			return d.Errf("too many positional args to `dns impreza`: %d", len(args))
		}

		// Optional block subdirectives (override compact-form values
		// or fill what compact didn't).
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "agent_id":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.AgentID = d.Val()
			case "agent_secret":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.AgentSecret = d.Val()
			case "base_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.BaseURL = d.Val()
			default:
				return d.Errf("unknown impreza subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

// call POSTs JSON to the Impreza API with agent auth headers and
// surfaces non-2xx as a structured error. Times out after 15 seconds
// — longer than the typical CF round-trip but tight enough that ACME's
// own retry budget kicks in if the API is hung.
func (p *Provider) call(ctx context.Context, path string, body map[string]string) error {
	url := strings.TrimRight(p.BaseURL, "/") + path
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	// Wrap ctx in our own deadline if the caller didn't set one;
	// keeps a stuck request from holding the ACME flow indefinitely.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Agent-Id", p.AgentID)
	req.Header.Set("X-Agent-Secret", p.AgentSecret)
	req.Header.Set("User-Agent", "caddy-dns-impreza/0.1")

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain so connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Surface the API's error envelope when present; truncate the rest
	// so a misconfigured 500 page doesn't flood the Caddy log.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("impreza dns api: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

// joinFQDN reassembles the libdns split. libdns splits a record into
// (name relative to zone, zone). For a TXT at
// `_acme-challenge.vault.imprezaapps.com` in zone `imprezaapps.com`,
// libdns hands us name=`_acme-challenge.vault` and zone=`imprezaapps.com.`.
// The server expects the full fqdn including the host portion of the
// challenged hostname (it doesn't see the zone).
//
// libdns normalises zone with a trailing dot; we strip it. If name is
// "@" or empty, the fqdn is just the zone (a rare ACME case but we
// handle it for completeness).
func joinFQDN(name, zone string) string {
	zone = strings.TrimSuffix(zone, ".")
	if name == "" || name == "@" {
		return zone
	}
	return name + "." + zone
}

func init() {
	caddy.RegisterModule(Provider{})
}

// Compile-time guards.
var (
	_ libdns.RecordAppender = (*Provider)(nil)
	_ libdns.RecordDeleter  = (*Provider)(nil)
	_ libdns.RecordGetter   = (*Provider)(nil)
	_ libdns.RecordSetter   = (*Provider)(nil)
	_ caddyfile.Unmarshaler = (*Provider)(nil)
	_ caddy.Provisioner     = (*Provider)(nil)
)
