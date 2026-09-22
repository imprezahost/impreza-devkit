package client

import (
	"github.com/imprezahost/impreza-devkit/sdk-go/config"
	"strings"
	"testing"
)

func TestOnionClientRequiresSocksBeforeDNS(t *testing.T) {
	onion := "http://api." + strings.Repeat("a", 56) + ".onion"
	if _, err := New(config.Context{URL: onion}); err == nil {
		t.Fatal("account client allowed direct onion")
	}
	if _, err := NewAgent(AgentOptions{BaseURL: onion}); err == nil {
		t.Fatal("agent client allowed direct onion")
	}
	if _, err := New(config.Context{URL: onion, UseTor: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgent(AgentOptions{BaseURL: onion, UseTor: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.Context{URL: "http://invalid.onion", UseTor: true}); err == nil {
		t.Fatal("invalid onion accepted")
	}
}
