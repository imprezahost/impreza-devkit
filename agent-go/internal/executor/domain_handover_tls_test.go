package executor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The handover verification must survive one full ACME retry cycle —
// the first dns-01 attempt NXDOMAINs while the TXT propagates, Caddy
// retries handoverACMERetryDelay later, and only then the certificate
// serves. The scenarios run against a real TLS listener with the retry
// clock scaled down, so "the challenge fails once and passes on the
// retry" is the real-world failure in miniature.

// handoverTLSListener serves HTTPS on a random loopback port whose answers flip
// from "not ready" to "serving" once readyAfter has elapsed since the
// listener started — the proxy-side stand-in for the certificate arriving
// one ACME retry cycle later.
func handoverTLSListener(t *testing.T, readyAfter time.Duration) (addr net.Addr, pool *x509.CertPool) {
	t.Helper()
	readyAt := time.Now().Add(readyAfter)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(readyAt) {
			// The certificate is not serving yet (handshake fails or the
			// origin answers 5xx): not ready.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener, _ = net.Listen("tcp", "127.0.0.1:0")
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert := srv.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return srv.Listener.Addr(), roots
}

func setHandoverTLSSeams(t *testing.T, budget, retry time.Duration) {
	t.Helper()
	prevBudget, prevRetry := handoverTLSBudgetNow, handoverTLSRetry
	handoverTLSBudgetNow, handoverTLSRetry = budget, retry
	t.Cleanup(func() { handoverTLSBudgetNow, handoverTLSRetry = prevBudget, prevRetry })
}

// The real-world scenario: the first attempts fail (the challenge
// NXDOMAINs), one full retry cycle later the certificate serves — the
// handover must COMPLETE, not roll back.
func TestHandoverTLSSurvivesOneACMERetryCycle(t *testing.T) {
	// Scaled clock: one retry cycle = 3 retry intervals; the budget
	// covers one cycle + issuance + slack (7 intervals).
	setHandoverTLSSeams(t, 700*time.Millisecond, 100*time.Millisecond)
	addr, pool := handoverTLSListener(t, 300*time.Millisecond)
	probe, cancel := context.WithTimeout(context.Background(), handoverTLSBudgetNow)
	defer cancel()
	if err := awaitVerifiedHTTPS(probe, addr.String(), "example.com", pool); err != nil {
		t.Fatalf("the handover rolled back inside one retry cycle: %v (addr %v)", err, addr)
	}
}

// The old flat window (the previous behavior, 2.5 retry intervals here)
// expires mid-cycle and the SAME scenario rolls back — the negative
// control: with the previous short ceiling this is the observed failure.
func TestHandoverTLSFlatWindowRollsTheSameScenarioBack(t *testing.T) {
	setHandoverTLSSeams(t, 250*time.Millisecond, 100*time.Millisecond)
	addr, pool := handoverTLSListener(t, 300*time.Millisecond)
	probe, cancel := context.WithTimeout(context.Background(), handoverTLSBudgetNow)
	defer cancel()
	if err := awaitVerifiedHTTPS(probe, addr.String(), "example.com", pool); err == nil {
		t.Fatal("the old flat window survived a full retry cycle it cannot cover")
	}
}

// A certificate that never serves still rolls the handover back — within
// the budget, not a minute more.
func TestHandoverTLSNeverReadyRollsBackWithinTheBudget(t *testing.T) {
	setHandoverTLSSeams(t, 400*time.Millisecond, 50*time.Millisecond)
	addr, pool := handoverTLSListener(t, time.Hour) // never ready
	start := time.Now()
	probe, cancel := context.WithTimeout(context.Background(), handoverTLSBudgetNow)
	defer cancel()
	if err := awaitVerifiedHTTPS(probe, addr.String(), "example.com", pool); err == nil {
		t.Fatal("a certificate that never serves completed the handover")
	}
	if elapsed := time.Since(start); elapsed > handoverTLSBudgetNow+150*time.Millisecond {
		t.Fatalf("the rollback waited %v beyond the budget", elapsed-handoverTLSBudgetNow)
	}
}

// The budget arithmetic is pinned: one ACME retry + issuance + slack. A
// regression to a flat 90 s reintroduces the race that was observed.
func TestHandoverTLSBudgetCoversOneACMERetryCycle(t *testing.T) {
	if handoverTLSBudget < handoverACMERetryDelay+handoverACMEIssuance+handoverTLSSlack {
		t.Fatalf("budget %v does not cover one retry cycle (%v + %v + %v)",
			handoverTLSBudget, handoverACMERetryDelay, handoverACMEIssuance, handoverTLSSlack)
	}
	if handoverTLSBudget < 180*time.Second {
		t.Fatalf("budget %v is shorter than the observed race window (90 s probe + 60 s retry)", handoverTLSBudget)
	}
}

// The production path validates against the SYSTEM roots only — no test
// pool, no skip: a listener with a self-signed certificate never passes.
func TestHandoverTLSProductionRootsRefuseSelfSigned(t *testing.T) {
	setHandoverTLSSeams(t, 300*time.Millisecond, 50*time.Millisecond)
	addr, _ := handoverTLSListener(t, 0) // serving immediately
	probe, cancel := context.WithTimeout(context.Background(), handoverTLSBudgetNow)
	defer cancel()
	if err := awaitVerifiedHTTPS(probe, addr.String(), "example.com", nil); err == nil {
		t.Fatal("the production probe accepted a certificate outside the system roots")
	}
}
