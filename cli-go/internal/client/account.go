package client

import (
	"context"
	"net/url"
)

// AccountInfo is the response from GET /v1/account. Field names match
// the Python SDK's `AccountInfo` model so users moving between the two
// CLIs see the same column headers.
type AccountInfo struct {
	ID           int      `json:"id"`
	FirstName    string   `json:"first_name"`
	LastName     string   `json:"last_name"`
	Company      string   `json:"company,omitempty"`
	Email        string   `json:"email"`
	Address1     string   `json:"address1,omitempty"`
	City         string   `json:"city,omitempty"`
	State        string   `json:"state,omitempty"`
	PostCode     string   `json:"postcode,omitempty"`
	Country      string   `json:"country,omitempty"`
	Phone        string   `json:"phone,omitempty"`
	Status       string   `json:"status,omitempty"`
	Balance      float64  `json:"balance"`
	Currency     string   `json:"currency"`
	RegisteredAt string   `json:"registered_at,omitempty"`
	IPRange      []string `json:"ip_range,omitempty"`
}

// AccountInfo wraps GET /v1/account.
func (c *Client) AccountInfo(ctx context.Context) (*AccountInfo, error) {
	var info AccountInfo
	if err := c.Get(ctx, "/v1/account", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Note: there is no separate /account/balance endpoint. `impreza
// account balance` is implemented in cmd/account.go by reusing
// AccountInfo() and printing just the Balance + Currency fields —
// matches the Python CLI.

// Service is one row of GET /v1/account/services. Field shape mirrors
// the SDK's `Service` model (note `product` not `product_name`).
type Service struct {
	ID           int     `json:"id"`
	Product      string  `json:"product,omitempty"`
	ProductGroup string  `json:"product_group,omitempty"`
	Domain       string  `json:"domain,omitempty"`
	Status       string  `json:"status,omitempty"`
	BillingCycle string  `json:"billing_cycle,omitempty"`
	Amount       float64 `json:"amount,omitempty"`
	DedicatedIP  string  `json:"dedicated_ip,omitempty"`
	RegisteredAt string  `json:"registered_at,omitempty"`
	NextDue      string  `json:"next_due,omitempty"`
	VpsBackend   string  `json:"vps_backend,omitempty"`

	// Extra fields populated on GET /v1/account/services/{id} (single-
	// service view). Omitted from the list response.
	Username       string   `json:"username,omitempty"`
	AssignedIPs    string   `json:"assigned_ips,omitempty"`
	ServerHostname string   `json:"server_hostname,omitempty"`
	ServerIP       string   `json:"server_ip,omitempty"`
	Nameservers    []string `json:"nameservers,omitempty"`
}

// servicesListResponse wraps the `{services, total}` shape returned by
// GET /v1/account/services.
type servicesListResponse struct {
	Services []Service `json:"services"`
	Total    int       `json:"total"`
}

// AccountServices wraps GET /v1/account/services with optional status filter.
func (c *Client) AccountServices(ctx context.Context, status string) ([]Service, error) {
	var q url.Values
	if status != "" {
		q = url.Values{"status": []string{status}}
	}
	var resp servicesListResponse
	if err := c.Get(ctx, "/v1/account/services", q, &resp); err != nil {
		return nil, err
	}
	return resp.Services, nil
}

// AccountServiceShow wraps GET /v1/account/services/{id}.
// Returns the single-service rich shape including username, server
// hostname/IP, assigned IPs, nameservers.
func (c *Client) AccountServiceShow(ctx context.Context, id int) (*Service, error) {
	var s Service
	if err := c.Get(ctx, "/v1/account/services/"+itoa(id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// KeyIdentity is the response from GET /v1/account/api-keys/self.
// Used by `impreza key whoami` and by `impreza doctor` (when ported in 7.5).
type KeyIdentity struct {
	ID              int                `json:"id"`
	ClientID        int                `json:"client_id"`
	Prefix          string             `json:"prefix"`
	Label           string             `json:"label,omitempty"`
	Status          string             `json:"status"`
	RateLimitPerMin int                `json:"rate_limit_per_minute,omitempty"`
	IPWhitelist     []IPWhitelistEntry `json:"ip_whitelist"`
	RequestIP       string             `json:"request_ip,omitempty"`
	LastUsedAt      string             `json:"last_used_at,omitempty"`
	CreatedAt       string             `json:"created_at,omitempty"`
}

// IPWhitelistEntry is one row of KeyIdentity.IPWhitelist.
type IPWhitelistEntry struct {
	ID        int    `json:"id"`
	IPAddress string `json:"ip_address"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ApiKeySelf wraps GET /v1/account/api-keys/self.
func (c *Client) ApiKeySelf(ctx context.Context) (*KeyIdentity, error) {
	var k KeyIdentity
	if err := c.Get(ctx, "/v1/account/api-keys/self", nil, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// itoa is a tiny strconv.Itoa wrapper used by path-builder methods
// across resource files. Avoids importing strconv just for one call.
func itoa(i int) string {
	return formatInt(i)
}
