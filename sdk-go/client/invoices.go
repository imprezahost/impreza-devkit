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

// InvoicePayment is the response from POST /v1/invoices/{id}/pay.
//
// Amount is what actually left the balance, which is the invoice total
// unless credit had already been applied. It is a float64 to match every
// other money field in this package — see the note on Invoice; do not
// accumulate these values, compare and total them in minor units.
type InvoicePayment struct {
	InvoiceID int     `json:"invoice_id"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency,omitempty"`
	Message   string  `json:"message,omitempty"`
}

// InvoicePay wraps POST /v1/invoices/{id}/pay — pays an unpaid invoice
// from the account credit balance.
//
// This spends real money and the API offers no way to undo it. Callers
// that front a human (the CLI does) must confirm before calling.
//
// A 409 comes back as *Conflict, with two cases worth telling apart via
// APIError.Code: ALREADY_PAID (nothing to do) and INSUFFICIENT_BALANCE
// (top up first, see TopupCreate). A 404 comes back as *NotFound.
func (c *Client) InvoicePay(ctx context.Context, id int) (*InvoicePayment, error) {
	var out InvoicePayment
	if err := c.Post(ctx, "/v1/invoices/"+formatInt(id)+"/pay", nil, &out); err != nil {
		return nil, err
	}
	// The endpoint echoes invoice_id, but fall back to the id we asked
	// about so the result is never ambiguous about which invoice moved.
	if out.InvoiceID == 0 {
		out.InvoiceID = id
	}
	return &out, nil
}
