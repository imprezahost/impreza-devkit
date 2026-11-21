package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// OfficialAPIOrigin is the only clearnet origin that may receive a
// credential without a separate authorization. A custom endpoint is
// allowed only when the caller binds that exact origin on purpose.
const OfficialAPIOrigin = "https://api.imprezahost.com"

// ErrCredentialOrigin is returned before any credential header is set
// when the request URL is not the official origin, not loopback, and
// not the origin bound to this credential.
var ErrCredentialOrigin = errors.New("recusando enviar a credencial: origem nao autorizada")

// AllowCredentialBase checks a configured base URL. A query or fragment
// on that URL is refused. An empty URL means the official default.
func AllowCredentialBase(raw, bound string) error {
	return allowCredential(raw, bound, true)
}

// AllowCredentialSend checks the URL of a request that would carry a
// credential. Request query strings are normal; userinfo and fragments
// are not.
func AllowCredentialSend(raw, bound string) error {
	return allowCredential(raw, bound, false)
}

// SameOrigin reports whether two URLs name the same scheme and host.
func SameOrigin(a, b string) bool {
	ao, aerr := originOf(a)
	bo, berr := originOf(b)
	return aerr == nil && berr == nil && ao == bo
}

func allowCredential(raw, bound string, base bool) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%w", ErrCredentialOrigin)
	}
	if base && u.RawQuery != "" {
		return fmt.Errorf("%w", ErrCredentialOrigin)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	scheme := strings.ToLower(u.Scheme)
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	onion := strings.HasSuffix(host, ".onion")
	switch scheme {
	case "https":
	case "http":
		if !loopback && !onion {
			return fmt.Errorf("%w", ErrCredentialOrigin)
		}
	default:
		return fmt.Errorf("%w", ErrCredentialOrigin)
	}
	origin := scheme + "://" + strings.ToLower(u.Host)
	if bound != "" {
		want, err := originOf(bound)
		if err != nil || origin != want {
			return fmt.Errorf("%w", ErrCredentialOrigin)
		}
		return nil
	}
	if origin == OfficialAPIOrigin || loopback {
		return nil
	}
	return fmt.Errorf("%w", ErrCredentialOrigin)
}

func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Scheme == "" {
		return "", ErrCredentialOrigin
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
