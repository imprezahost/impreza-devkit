package tor

import (
	"net/http"
	"testing"
)

func TestExplicitSocksDoesNotInheritHTTPProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	for _, scheme := range []string{"socks5", "socks5h"} {
		rt, err := Transport(scheme + "://127.0.0.1:9050")
		if err != nil {
			t.Fatal(err)
		}
		tr := rt.(*http.Transport)
		if tr.Proxy != nil || tr.DialContext == nil {
			t.Fatal("explicit transport can use environment proxy or lacks cancellation")
		}
	}
}
func TestRejectProxyCredentialsAndInvalidURL(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:9050", "socks5://user:private@127.0.0.1:9050", "socks5://127.0.0.1:9050/path", "socks5://127.0.0.1"} {
		if _, err := Transport(value); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
