package client

import (
	"context"
	"fmt"
	"net/url"
)

// Order is one row of GET /v1/orders. `order_number` is the
// customer-facing reference shown in the admin UI — the server
// emits it as an int (10 digits) so we decode as int64 to fit.
type Order struct {
	ID            int     `json:"id"`
	OrderNumber   int64   `json:"order_number,omitempty"`
	Status        string  `json:"status,omitempty"`
	Date          string  `json:"date,omitempty"`
	Amount        float64 `json:"amount,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	InvoiceID     int     `json:"invoice_id,omitempty"`
	PaymentMethod string  `json:"payment_method,omitempty"`
}

// ordersListResponse unwraps {orders, total}.
type ordersListResponse struct {
	Orders []Order `json:"orders"`
	Total  int     `json:"total"`
}

// OrdersList wraps GET /v1/orders with optional status filter.
func (c *Client) OrdersList(ctx context.Context, status string) ([]Order, error) {
	var q url.Values
	if status != "" {
		q = url.Values{"status": []string{status}}
	}
	var resp ordersListResponse
	if err := c.Get(ctx, "/v1/orders", q, &resp); err != nil {
		return nil, err
	}
	return resp.Orders, nil
}

// OrderDetail is the response from GET /v1/orders/{id}.
type OrderDetail struct {
	Order
	Items []OrderItem `json:"items,omitempty"`
}

// OrderItem matches the live /v1/orders/{id} item shape, which
// differs from /v1/orders list rows: items carry the service_id
// the order provisioned, plus product name + domain + billing
// cycle + amount-as-string.
type OrderItem struct {
	ServiceID    int    `json:"service_id,omitempty"`
	Product      string `json:"product,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Status       string `json:"status,omitempty"`
	BillingCycle string `json:"billingcycle,omitempty"`
	Amount       string `json:"amount,omitempty"` // string on the wire ("25.00")
}

// OrderShow wraps GET /v1/orders/{id}.
func (c *Client) OrderShow(ctx context.Context, id int) (*OrderDetail, error) {
	var o OrderDetail
	if err := c.Get(ctx, fmt.Sprintf("/v1/orders/%d", id), nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// OrderCreateRequest is the body for POST /v1/orders. Mirrors the
// Python SDK's create() — string-keyed config_options + custom_fields
// because that's what the WHMCS backend wants on the wire.
type OrderCreateRequest struct {
	ProductID     int               `json:"product_id"`
	BillingCycle  string            `json:"billing_cycle"`           // monthly / annually / etc.
	Domain        string            `json:"domain,omitempty"`        // required for hosting / domain products
	Hostname      string            `json:"hostname,omitempty"`      // optional for VPS
	ConfigOptions map[string]int    `json:"config_options,omitempty"` // {option_id: sub_option_id}
	CustomFields  map[string]string `json:"custom_fields,omitempty"`  // {field_id: value}
	PaymentMethod string            `json:"payment_method,omitempty"` // gateway slug; "credit" to pay from balance
}

// OrderCreateResult is the response from POST /v1/orders. Carries the
// new order_id, invoice_id, and a hint at the balance impact so the
// CLI can render "X.XX USD will be charged".
type OrderCreateResult struct {
	OrderID      int      `json:"order_id"`
	OrderNumber  int64    `json:"order_number,omitempty"`
	InvoiceID    int      `json:"invoice_id,omitempty"`
	Amount       float64  `json:"amount,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	BalanceAfter *float64 `json:"balance_after,omitempty"`
}

// OrderCreate wraps POST /v1/orders.
func (c *Client) OrderCreate(ctx context.Context, req OrderCreateRequest) (*OrderCreateResult, error) {
	var res OrderCreateResult
	if err := c.Post(ctx, "/v1/orders", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// OrderUpgradeRequest is the body for POST /v1/orders/{service_id}/upgrade.
type OrderUpgradeRequest struct {
	ProductID     int               `json:"product_id"`                // target product id
	BillingCycle  string            `json:"billing_cycle"`
	ConfigOptions map[string]int    `json:"config_options,omitempty"`
	PaymentMethod string            `json:"payment_method,omitempty"`
}

// OrderUpgrade wraps POST /v1/orders/{service_id}/upgrade.
func (c *Client) OrderUpgrade(ctx context.Context, serviceID int, req OrderUpgradeRequest) (*OrderCreateResult, error) {
	var res OrderCreateResult
	path := fmt.Sprintf("/v1/orders/%d/upgrade", serviceID)
	if err := c.Post(ctx, path, req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
