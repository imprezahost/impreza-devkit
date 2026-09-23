package client

// Tor v3 key custody —
// /v1/platform/deployments/{id}/onion/export|rotate. Export seals the
// hidden-service identity bundle on the agent to the customer's X25519
// recipient key (NaCl/libsodium anonymous sealed box): the plaintext key
// never transits and never rests on the platform. Rotate mints a fresh
// key and the .onion address CHANGES — the old one dies for visitors.
//
// Deploy bodies accept `onion_import` with secret_key_b64 and public_key_b64
// (both C Tor key files) to bring an existing identity — deploy-time only,
// requires onion:true. Export plaintext is a JSON version:1 bundle with onion
// and those same two fields; it is never a bare secret-key file.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
)

// OnionKeyExportResponse is the data payload of POST .../onion/export
// (202): the enqueued command whose result will carry the sealed blob.
type OnionKeyExportResponse struct {
	CommandID string `json:"command_id"`
	Note      string `json:"note"`
}

// OnionKeyExportBlob is the data payload of GET .../onion/export/{commandId}.
//
// READ-ONCE: a successful read burns the blob server-side — a second read
// is a 404. Persist Sealed immediately; unseal it locally with the
// recipient PRIVATE key (PyNaCl SealedBox / libsodium crypto_box_seal_open).
type OnionKeyExportBlob struct {
	CommandID string `json:"command_id"`
	Onion     string `json:"onion"`
	Sealed    string `json:"sealed"`
	Note      string `json:"note"`
}

// OnionRotateResponse is the data payload of POST .../onion/rotate
// (202): the enqueued command. The new address lands on the deployment
// row when the agent reports.
type OnionRotateResponse struct {
	CommandID string `json:"command_id"`
	Note      string `json:"note"`
}

var onionV3Address = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)

// ValidOnionRecipientPubkey reports whether s is the canonical standard
// base64 of a 32-byte X25519 public key.
func ValidOnionRecipientPubkey(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(raw) == 32 && base64.StdEncoding.EncodeToString(raw) == s
}

// PlatformExportOnionKey wraps POST /v1/platform/deployments/{id}/onion/export.
// Sends confirm:true (the server gate) with the recipient pubkey; the
// sealed blob is retrieved ONCE via PlatformFetchOnionKeyExport after the
// command completes. 422 when the agent lacks onion-custody-v1.
func (c *Client) PlatformExportOnionKey(ctx context.Context, id, recipientPubkey string) (*OnionKeyExportResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if !ValidOnionRecipientPubkey(recipientPubkey) {
		return nil, fmt.Errorf("recipient_pubkey must be the standard base64 of a 32-byte X25519 public key")
	}
	var out OnionKeyExportResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/export",
		map[string]any{"recipient_pubkey": recipientPubkey, "confirm": true}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformFetchOnionKeyExport wraps GET /v1/platform/deployments/{id}/onion/export/{commandId}.
// READ-ONCE: the first successful read burns the blob (second read 404s);
// 409 while the export command has not completed yet.
func (c *Client) PlatformFetchOnionKeyExport(ctx context.Context, id, commandID string) (*OnionKeyExportBlob, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if commandID == "" {
		return nil, fmt.Errorf("command id is required")
	}
	var out OnionKeyExportBlob
	if err := c.Get(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/export/"+url.PathEscape(commandID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformRotateOnionKey wraps POST /v1/platform/deployments/{id}/onion/rotate.
// DESTRUCTIVE: the current .onion stops working for visitors when the
// agent applies the rotation. confirmAddress must be the current address
// verbatim (the server refuses mismatches with 400). Sends confirm:true.
func (c *Client) PlatformRotateOnionKey(ctx context.Context, id, confirmAddress string) (*OnionRotateResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if !onionV3Address.MatchString(confirmAddress) {
		return nil, fmt.Errorf("confirm_address must be the current .onion address, verbatim (56 base32 chars + .onion)")
	}
	var out OnionRotateResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/rotate",
		map[string]any{"confirm": true, "confirm_address": confirmAddress}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OnionPurgeResponse is the data payload of POST .../onion/purge: either an
// enqueued agent command (202, purged copies land on its result) or an
// immediate release (200) when only control-plane records held the address
// (for example an uninstalled deployment's stale row).
type OnionPurgeResponse struct {
	CommandID string `json:"command_id,omitempty"`
	Mode      string `json:"mode"` // "agent" | "released"
	Note      string `json:"note"`
}

// PlatformPurgeOnionKey wraps POST /v1/platform/deployments/{id}/onion/purge.
// IRREVERSIBLE: destroys the parked recovery copies of address on the
// deployment's host and releases the platform-side reservation. The address
// must be a RETAINED identity (a prior address after rotation, or the stale
// row of an uninstalled deployment) — never the deployment's current one.
// Sends confirm:true. 422 when the agent lacks onion-purge-v1 (agent mode).
func (c *Client) PlatformPurgeOnionKey(ctx context.Context, id, address string) (*OnionPurgeResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if !onionV3Address.MatchString(address) {
		return nil, fmt.Errorf("address must be the retained .onion being destroyed, verbatim (56 base32 chars + .onion)")
	}
	var out OnionPurgeResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/purge",
		map[string]any{"confirm": true, "confirm_address": address}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
