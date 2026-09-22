package proxy

import (
	"strings"
	"testing"
)

func TestLegacyOnionListenerIsPrivate(t *testing.T) {
	raw := "# deployment dpl_test\nexample.test {\n  reverse_proxy app:80\n}\nhttp://" + strings.Repeat("a", 56) + ".onion {\n  reverse_proxy app:80\n}\n"
	secured, err := secureOnionFragment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(secured, onionBind) != 1 {
		t.Fatal("onion not isolated")
	}
	again, err := secureOnionFragment(secured)
	if err != nil || again != secured {
		t.Fatal("migration not idempotent")
	}
	if !strings.Contains(secured, "example.test {\n  reverse_proxy app:80\n}") {
		t.Fatal("clearnet changed")
	}
	if _, err := secureOnionFragment(strings.Replace(secured, onionBind, "  bind 0.0.0.0", 1)); err == nil {
		t.Fatal("public binding accepted")
	}
}
