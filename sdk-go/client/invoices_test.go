package client

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// InvoicePay spends real balance, so its wiring is worth pinning: the
// right method on the right path, and the two 409 cases distinguishable
// by code so a caller can tell "nothing to do" from "top up first".

func TestInvoicePayPostsToPayPath(t *testing.T) {
	var gotMethod, gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		envelopeResponder(map[string]any{
			"invoice_id": 2345,
			"amount":     17.0,
			"currency":   "USD",
			"message":    "Invoice paid from account balance.",
		})(w, r)
	}))

	res, err := c.InvoicePay(context.Background(), 2345)
	if err != nil {
		t.Fatalf("InvoicePay: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/v1/invoices/2345/pay"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if res.InvoiceID != 2345 || res.Amount != 17.0 || res.Currency != "USD" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestInvoicePayBackfillsInvoiceID(t *testing.T) {
	// A server that omits invoice_id must not yield a result that is
	// ambiguous about which invoice moved.
	c, _ := newTestClient(t, envelopeResponder(map[string]any{
		"amount":   17.0,
		"currency": "USD",
	}))

	res, err := c.InvoicePay(context.Background(), 2345)
	if err != nil {
		t.Fatalf("InvoicePay: %v", err)
	}
	if res.InvoiceID != 2345 {
		t.Errorf("InvoiceID = %d, want 2345 (backfilled)", res.InvoiceID)
	}
}

func TestInvoicePay409MapsToConflict(t *testing.T) {
	for _, tc := range []struct{ code, msg string }{
		{"ALREADY_PAID", "This invoice is already paid."},
		{"INSUFFICIENT_BALANCE", "Insufficient balance. Required: USD 17.00"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c, _ := newTestClient(t, errorResponder(http.StatusConflict, tc.code, tc.msg))

			_, err := c.InvoicePay(context.Background(), 2345)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			var cf *Conflict
			if !errors.As(err, &cf) {
				t.Fatalf("error is %T, want *Conflict", err)
			}
			if cf.Code != tc.code {
				t.Errorf("Code = %q, want %q", cf.Code, tc.code)
			}
		})
	}
}

func TestInvoicePay404MapsToNotFound(t *testing.T) {
	c, _ := newTestClient(t, errorResponder(http.StatusNotFound, "NOT_FOUND", "No such invoice."))

	_, err := c.InvoicePay(context.Background(), 999)
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("error is %T, want *NotFound", err)
	}
}
