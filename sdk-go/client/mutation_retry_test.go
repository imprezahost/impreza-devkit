package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type mutationRetryFixture struct {
	fault string
	calls int
}

func (f *mutationRetryFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	status := http.StatusNoContent
	if f.calls == 1 {
		switch f.fault {
		case "timeout":
			return nil, context.DeadlineExceeded
		case "gateway":
			status = http.StatusBadGateway
		}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func TestTransportDoesNotReplayUnconfirmedMutations(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodGet} {
		for _, fault := range []string{"timeout", "gateway", "confirmed"} {
			t.Run(fmt.Sprintf("%s/%s", method, fault), func(t *testing.T) {
				fixture := &mutationRetryFixture{fault: fault}
				transport := &retryTransport{base: fixture, maxAttempts: 2, baseDelay: time.Nanosecond}
				req, err := http.NewRequestWithContext(context.Background(), method, "https://fixture.invalid/mutation", nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := transport.RoundTrip(req)
				if response != nil {
					defer response.Body.Close()
				}
				if method == http.MethodGet || fault == "confirmed" {
					wantCalls := 1
					if fault != "confirmed" {
						wantCalls = 2
					}
					if err != nil || response == nil || response.StatusCode != http.StatusNoContent || fixture.calls != wantCalls {
						t.Fatalf("confirmed/read transport mismatch: calls=%d, err=%v", fixture.calls, err)
					}
					return
				}
				if fixture.calls != 1 {
					t.Fatalf("unconfirmed mutation was replayed: calls=%d", fixture.calls)
				}
				if fault == "timeout" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("unconfirmed timeout was not returned: %v", err)
					}
				} else if err != nil || response == nil || response.StatusCode != http.StatusBadGateway {
					t.Fatalf("unconfirmed gateway response was not returned: %v", err)
				}
			})
		}
	}
}
