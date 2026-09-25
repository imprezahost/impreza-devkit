package poll

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJournalAcceptsOnlySecretFreeFencePayloads(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	recipient := strings.Repeat("A", 43) + "="
	base := map[string]any{"deployment_id": "dpl_" + strings.Repeat("c", 16), "hostname": "site-abcdef.imprezaapps.com",
		"epoch": 2, "cutover_id": "fov_" + strings.Repeat("b", 24)}
	with := func(extra map[string]any) json.RawMessage {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for name, tc := range map[string]struct {
		raw  json.RawMessage
		want bool
	}{
		"legacy fence":             {with(nil), true},
		"onion transfer fence":     {with(map[string]any{"onion": onion, "onion_recipient": recipient}), true},
		"onion without recipient":  {with(map[string]any{"onion": onion}), false},
		"recipient without onion":  {with(map[string]any{"onion_recipient": recipient}), false},
		"invalid onion":            {with(map[string]any{"onion": "x.onion", "onion_recipient": recipient}), false},
		"recipient is not 32 byte": {with(map[string]any{"onion": onion, "onion_recipient": "AAAA"}), false},
		"unexpected secret field":  {with(map[string]any{"onion": onion, "secret_key_b64": recipient}), false},
		"extra field":              {with(map[string]any{"onion": onion, "onion_recipient": recipient, "x": 1}), false},
	} {
		if got := validFencePayload(tc.raw); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
