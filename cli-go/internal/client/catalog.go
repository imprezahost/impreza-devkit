package client

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// strconvItoa is a tiny local alias so PriceForYears doesn't have to
// import strconv at call-site.
var strconvItoa = strconv.Itoa

// Product is one row of GET /v1/products. Shape matches the live
// server response, not the Python SDK's model (which uses Pydantic
// aliases — Go's json tags work directly).
type Product struct {
	ID          int                     `json:"id"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	GroupID     int                     `json:"group_id,omitempty"`
	Group       string                  `json:"group,omitempty"` // group name; server emits "group", not "group_name"
	Type        string                  `json:"type,omitempty"`
	Module      string                  `json:"module,omitempty"`
	Currency    string                  `json:"currency,omitempty"`
	Pricing     map[string]ProductPrice `json:"pricing,omitempty"` // keyed by cycle: monthly / quarterly / annually / etc.
}

// ProductPrice is one billing-cycle entry under Product.Pricing.
// Server emits {"price": N, "setup_fee": M}.
type ProductPrice struct {
	Price    float64 `json:"price"`
	SetupFee float64 `json:"setup_fee,omitempty"`
}

// productsListResponse unwraps {products, total}.
type productsListResponse struct {
	Products []Product `json:"products"`
	Total    int       `json:"total"`
}

// CatalogProducts wraps GET /v1/products with optional group filter.
// (The endpoint was originally documented under `/v1/catalog/products`;
// the live server exposes it at `/v1/products` — matches the Python
// SDK path.)
func (c *Client) CatalogProducts(ctx context.Context, group string) ([]Product, error) {
	var q url.Values
	if group != "" {
		q = url.Values{"group": []string{group}}
	}
	var resp productsListResponse
	if err := c.Get(ctx, "/v1/products", q, &resp); err != nil {
		return nil, err
	}
	return resp.Products, nil
}

// ProductDetail is the response from GET /v1/products/{id}.
// Carries the same fields as Product plus config_options + custom_fields.
type ProductDetail struct {
	Product
	ConfigOptions []ConfigOption `json:"config_options,omitempty"`
	CustomFields  []CustomField  `json:"custom_fields,omitempty"`
}

// ConfigOption is one configurable option on a product (e.g. "Operating
// System", "Datacenter Location"). `Type` is the WHMCS-internal option
// type code: 1=dropdown, 2=radio, 3=yes/no, 4=text-box. Sub-options
// (per Options) carry their own pricing maps.
type ConfigOption struct {
	ID      int               `json:"id"`
	Name    string            `json:"name"`
	Type    int               `json:"type,omitempty"`
	Options []ConfigSubOption `json:"options,omitempty"`
}

// ConfigSubOption is one selectable value under a ConfigOption. Pricing
// is a flat cycle → price map (no setup_fee at the sub-option level).
type ConfigSubOption struct {
	ID      int                `json:"id"`
	Name    string             `json:"name"`
	Pricing map[string]float64 `json:"pricing,omitempty"`
}

// CustomField is a free-text / dropdown field on a product (e.g.
// "VLAN Tag", "License Key").
type CustomField struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// CatalogProduct wraps GET /v1/products/{id}.
func (c *Client) CatalogProduct(ctx context.Context, id int) (*ProductDetail, error) {
	var p ProductDetail
	if err := c.Get(ctx, "/v1/products/"+formatInt(id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ProductGroup is one row of GET /v1/products/groups.
type ProductGroup struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// productGroupsListResponse unwraps {groups, total}.
type productGroupsListResponse struct {
	Groups []ProductGroup `json:"groups"`
	Total  int            `json:"total"`
}

// CatalogProductGroups wraps GET /v1/products/groups.
func (c *Client) CatalogProductGroups(ctx context.Context) ([]ProductGroup, error) {
	var resp productGroupsListResponse
	if err := c.Get(ctx, "/v1/products/groups", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Groups, nil
}

// TldPricing is one row of GET /v1/domains/pricing. Matches the live
// shape: `register` and `renew` are maps of year → price (multi-year
// registrations have proportional pricing).
type TldPricing struct {
	TLD      string             `json:"tld"`
	Register map[string]float64 `json:"register,omitempty"`
	Renew    map[string]float64 `json:"renew,omitempty"`
	Currency string             `json:"currency,omitempty"`
	MinYears int                `json:"min_years,omitempty"`
	Cheapest float64            `json:"cheapest,omitempty"` // headline 1-year register price
}

// PriceForYears returns the price for an N-year period from a price
// map (Register or Renew). Returns 0 if N-years isn't present.
func PriceForYears(m map[string]float64, years int) float64 {
	if m == nil {
		return 0
	}
	return m[strconvItoa(years)]
}

// tldsListResponse unwraps {tlds, total}.
type tldsListResponse struct {
	TLDs  []TldPricing `json:"tlds"`
	Total int          `json:"total"`
}

// CatalogTlds wraps GET /v1/domains/pricing with optional TLD filter
// (server-side param is `tld`, comma-joined). The "catalog tlds"
// CLI verb name is preserved for parity with the Python CLI even
// though the underlying path lives under /domains/pricing.
func (c *Client) CatalogTlds(ctx context.Context, filter []string) ([]TldPricing, error) {
	var q url.Values
	if len(filter) > 0 {
		clean := make([]string, 0, len(filter))
		for _, t := range filter {
			clean = append(clean, strings.TrimPrefix(strings.TrimSpace(t), "."))
		}
		q = url.Values{"tld": []string{strings.Join(clean, ",")}}
	}
	var resp tldsListResponse
	if err := c.Get(ctx, "/v1/domains/pricing", q, &resp); err != nil {
		return nil, err
	}
	return resp.TLDs, nil
}
