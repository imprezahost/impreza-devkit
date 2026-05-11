package client

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TopupInvoice is the response from POST /v1/account/topup.
//
// Created in the "pending" state with a `payment_url` pointing at the
// btcpayinline gateway. Use Wait/WaitUntilPaid to block until the
// gateway confirms payment.
type TopupInvoice struct {
	InvoiceID     int     `json:"invoice_id"`
	Amount        float64 `json:"amount,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	Method        string  `json:"method,omitempty"`
	Status        string  `json:"status,omitempty"`
	PaymentURL    string  `json:"payment_url,omitempty"`
	CryptoAddress string  `json:"crypto_address,omitempty"`
	QrCodePng     string  `json:"qr_code_png,omitempty"` // base64-encoded
	ExpiresAt     string  `json:"expires_at,omitempty"`
	CreatedAt     string  `json:"created_at,omitempty"`
	BalanceAfter  *float64 `json:"balance_after,omitempty"`

	client *Client `json:"-"`
}

// IsSettled returns true if the invoice has reached a terminal payment
// state (paid by the gateway, or cancelled / expired without payment).
func (inv *TopupInvoice) IsSettled() bool {
	switch inv.Status {
	case "paid", "cancelled", "expired", "refunded":
		return true
	}
	return false
}

// ErrTopupExpired is returned by Wait/WaitUntilPaid when the invoice
// reaches a terminal state other than "paid".
var ErrTopupExpired = errors.New("top-up invoice expired or cancelled before payment")

// AccountTopupRequest is the body for POST /v1/account/topup.
type AccountTopupRequest struct {
	Amount float64 `json:"amount"`
	Method string  `json:"method,omitempty"` // btc | xmr | trx | usdt | usdt_trc20
}

// AccountTopup wraps POST /v1/account/topup. Creates a new invoice
// for the supplied amount + payment method and returns the
// TopupInvoice future.
func (c *Client) AccountTopup(ctx context.Context, req AccountTopupRequest) (*TopupInvoice, error) {
	var inv TopupInvoice
	if err := c.Post(ctx, "/v1/account/topup", req, &inv); err != nil {
		return nil, err
	}
	inv.client = c
	return &inv, nil
}

// AccountTopupStatus wraps GET /v1/account/topup/{invoice_id}. Returns
// the latest snapshot of the invoice — status, balance_after if paid,
// timestamps.
func (c *Client) AccountTopupStatus(ctx context.Context, invoiceID int) (*TopupInvoice, error) {
	var inv TopupInvoice
	if err := c.Get(ctx, fmt.Sprintf("/v1/account/topup/%d", invoiceID), nil, &inv); err != nil {
		return nil, err
	}
	inv.client = c
	return &inv, nil
}

// TopupWaitOptions controls polling behaviour for WaitUntilPaid.
type TopupWaitOptions struct {
	Timeout      time.Duration // default 2h
	PollInterval time.Duration // default 5s
	OnUpdate     func(inv *TopupInvoice)
}

// WaitUntilPaid blocks until the invoice reaches a terminal state.
// Returns nil if the status settled to "paid"; ErrTopupExpired (wrapped
// with the actual terminal status) otherwise.
//
// Polls GET /v1/account/topup/{invoice_id} every PollInterval. Honours
// ctx cancellation.
func (inv *TopupInvoice) WaitUntilPaid(ctx context.Context, opts TopupWaitOptions) error {
	if inv.client == nil {
		return errors.New("TopupInvoice not bound to a Client (constructed without ctx?)")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	interval := opts.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for {
		if inv.IsSettled() {
			if inv.Status == "paid" {
				return nil
			}
			return fmt.Errorf("%w: status=%q", ErrTopupExpired, inv.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("WaitUntilPaid: timeout after %s (last status: %s)", timeout, inv.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		next, err := inv.client.AccountTopupStatus(ctx, inv.InvoiceID)
		if err != nil {
			return fmt.Errorf("poll topup: %w", err)
		}
		// Carry the client binding forward.
		next.client = inv.client
		*inv = *next

		if opts.OnUpdate != nil {
			opts.OnUpdate(inv)
		}
	}
}
