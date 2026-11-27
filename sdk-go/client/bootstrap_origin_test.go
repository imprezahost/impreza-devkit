package client

import (
	"context"
	"errors"
	"testing"

	"github.com/imprezahost/impreza-devkit/sdk-go/config"
)

func TestBootstrapRejectsUnprotectedOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, origin := range []string{"http://fixture.invalid", "https://fixture.invalid?option=1"} {
		result, err := Bootstrap(ctx, "synthetic", BootstrapRequest{}, BootstrapOptions{BaseURL: origin})
		if result != nil || !errors.Is(err, config.ErrCredentialOrigin) {
			t.Error("bootstrap did not reject the configured origin before transport")
		}
	}
}
