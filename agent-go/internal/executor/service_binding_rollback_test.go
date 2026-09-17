package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServiceBindingRollbackBoundary(t *testing.T) {
	_, credential := bindingFixture()
	url := "postgresql://" + credential.Username + ":" + credential.Password + "@pg_" + credential.ProviderDeploymentID + ":5432/" + credential.Database + "?sslmode=disable"
	model := func(value string, bound bool) []byte {
		networks := map[string]any{"default": nil}
		if bound {
			networks["impreza-binding-test"] = nil
		}
		raw, _ := json.Marshal(map[string]any{"services": map[string]any{"app": map[string]any{"environment": map[string]string{"DATABASE_URL": value}, "networks": networks}}})
		return raw
	}
	for _, tc := range []struct {
		name            string
		current, target []byte
		allowed         bool
	}{
		{"same credential", model(url, true), model(url, true), true},
		{"ordinary user setting", model("postgresql://user:one@db/database", false), model("postgresql://user:two@db/database", false), true},
		{"previous login generation", model(url, true), model(strings.Replace(url, credential.Password, strings.Repeat("9", 64), 1), true), false},
		{"removed connection", model("", false), model(url, true), false},
		{"unbound historical release", model(url, true), model("", false), false},
		{"missing managed value", model("", true), model("", true), false},
		{"invalid resolved config", []byte("invalid"), model(url, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serviceBindingRollbackCompatible(tc.current, tc.target)
			if (err == nil) != tc.allowed || (err != nil && strings.Contains(err.Error(), credential.Password)) {
				t.Fatal("rollback connection boundary or secret-safe error failed")
			}
		})
	}
}
