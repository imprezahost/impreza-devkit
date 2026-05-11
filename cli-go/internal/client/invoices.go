package client

import (
	"context"
	"net/url"
)

// Invoice is one row of GET /v1/invoices. Field shape matches the
// live response: invoice_num is the customer-facing reference (not
// to be confused with id which is the WHMCS-internal primary key).
type Invoice struct {
	ID            int     `json:"id"`
	InvoiceNum    string  `json:"invoice_num,omitempty"`
	Date          string  `json:"date,omitempty"`
	DueDate       string  `json:"due_date,omitempty"`
	DatePaid      string  `json:"date_paid,omitempty"`
	Status        string  `json:"status,omitempty"`
	Subtotal      float64 `json:"subtotal,omitempty"`
	Credit        float64 `json:"credit,omitempty"`
	Tax           float64 `json:"tax,omitempty"`
	Total         float64 `json:"total,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	PaymentMethod string  `json:"payment_method,omitempty"`
}

// invoicesListResponse unwraps {invoices, total}.
type invoicesListResponse struct {
	Invoices []Invoice `json:"invoices"`
	Total    int       `json:"total"`
}

// InvoicesList wraps GET /v1/invoices with optional status filter.
func (c *Client) InvoicesList(ctx context.Context, status string) ([]Invoice, error) {
	var q url.Values
	if status != "" {
		q = url.Values{"status": []string{status}}
	}
	var resp invoicesListResponse
	if err := c.Get(ctx, "/v1/invoices", q, &resp); err != nil {
		return nil, err
	}
	return resp.Invoices, nil
}

// InvoiceDetail is the response from GET /v1/invoices/{id}.
type InvoiceDetail struct {
	Invoice
	Items        []InvoiceItem        `json:"items,omitempty"`
	Transactions []InvoiceTransaction `json:"transactions,omitempty"`
}

type InvoiceItem struct {
	ID          int     `json:"id"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	Type        string  `json:"type,omitempty"`
}

type InvoiceTransaction struct {
	ID            int     `json:"id"`
	Date          string  `json:"date,omitempty"`
	Gateway       string  `json:"gateway,omitempty"`
	Amount        float64 `json:"amount"`
	TransactionID string  `json:"transaction_id,omitempty"`
}

// InvoiceShow wraps GET /v1/invoices/{id}.
func (c *Client) InvoiceShow(ctx context.Context, id int) (*InvoiceDetail, error) {
	var inv InvoiceDetail
	if err := c.Get(ctx, "/v1/invoices/"+formatInt(id), nil, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}
