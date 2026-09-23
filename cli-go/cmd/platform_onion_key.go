package cmd

// `impreza platform onion-key` — Tor v3 key custody.
//
//	export — seal the hidden-service private key to YOUR X25519 recipient
//	         key (the plaintext never transits the platform)
//	fetch  — read a completed export's sealed blob, ONCE (the read burns it)
//	rotate — mint a fresh key; the .onion address CHANGES and the old one dies
//
// Deploy-time BYO key lives on the deploy commands as
// --onion-import-secret-file + --onion-import-pub-file.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// readOnionImportFiles loads a local C Tor key PAIR and returns it
// base64-encoded for the deploy body's onion_import field. The secret
// file's body is the expanded scalar (it does NOT contain the public
// key), so both files are required; the address derives from the public
// file. Mirrors the server's 400s before any HTTP call: import requires
// --onion, and the files must be the 96-byte hs_ed25519_secret_key and
// the 64-byte hs_ed25519_public_key.
func readOnionImportFiles(secretPath, pubPath string, onion bool) (*sdkclient.OnionImportKeys, error) {
	if (secretPath == "") != (pubPath == "") {
		return nil, fmt.Errorf("--onion-import-secret-file and --onion-import-pub-file are required together (the secret file does not contain the public key)")
	}
	if secretPath == "" {
		return nil, nil
	}
	if !onion {
		return nil, fmt.Errorf("--onion-import-secret-file/--onion-import-pub-file require --onion")
	}
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, fmt.Errorf("read onion secret key file: %w", err)
	}
	if len(secret) != 96 {
		return nil, fmt.Errorf("%s is %d bytes — expected the 96-byte Tor hs_ed25519_secret_key file", secretPath, len(secret))
	}
	pub, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, fmt.Errorf("read onion public key file: %w", err)
	}
	if len(pub) != 64 {
		return nil, fmt.Errorf("%s is %d bytes — expected the 64-byte Tor hs_ed25519_public_key file", pubPath, len(pub))
	}
	return &sdkclient.OnionImportKeys{
		SecretKeyB64: base64.StdEncoding.EncodeToString(secret),
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

var platformOnionKeyCmd = &cobra.Command{
	Use:   "onion-key",
	Short: "Key custody for a deployment's .onion (export / fetch / rotate).",
}

var (
	platformOnionKeyExportRecipient string
	platformOnionKeyExportFetch     bool
)

var platformOnionKeyExportCmd = &cobra.Command{
	Use:   "export <deployment-id>",
	Short: "Export the hidden-service private key, sealed to your X25519 key.",
	Long: `Export the hidden-service PRIVATE key of a deployment.

The agent seals the key on the server to --recipient-pubkey with a
NaCl/libsodium anonymous sealed box — the plaintext key never transits
and never rests on the platform, and the platform cannot open the blob.
Generate the recipient keypair LOCALLY, e.g.:

  python3 -c "from nacl.public import PrivateKey; import base64; k=PrivateKey.generate(); print('pub:', base64.b64encode(bytes(k.public_key)).decode()); print('priv:', base64.b64encode(bytes(k)).decode())"

Keep the private half; pass the public half here. The blob is retrievable
EXACTLY ONCE via fetch — this command with --fetch, or:

  impreza platform onion-key fetch <deployment-id> --command-id <cmd_...>

Requires an agent with onion-custody-v1 (422 on older agents).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !sdkclient.ValidOnionRecipientPubkey(platformOnionKeyExportRecipient) {
			return fmt.Errorf("--recipient-pubkey must be the standard base64 of a 32-byte X25519 public key")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformExportOnionKey(cmd.Context(), args[0], platformOnionKeyExportRecipient)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Export enqueued. command_id=%s\n", out.CommandID)
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
		if !platformOnionKeyExportFetch {
			fmt.Fprintf(w, "\nOnce the agent finishes, read the sealed blob ONCE with:\n  impreza platform onion-key fetch %s --command-id %s\n", args[0], out.CommandID)
			return nil
		}
		return fetchOnionKeyExport(cmd, c, args[0], out.CommandID, true)
	},
}

var platformOnionKeyFetchCommandID string

var platformOnionKeyFetchCmd = &cobra.Command{
	Use:   "fetch <deployment-id>",
	Short: "Read a completed export's sealed blob — ONCE (the read burns it).",
	Long: `Read the sealed export blob of a completed onion-key export command —
EXACTLY ONCE: this read removes the ciphertext from the platform and a
second read is a 404 (re-export if you lose it). Unseal the blob locally
with the recipient PRIVATE key (PyNaCl SealedBox / libsodium
crypto_box_seal_open).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if platformOnionKeyFetchCommandID == "" {
			return fmt.Errorf("--command-id is required (the cmd_... from onion-key export)")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		return fetchOnionKeyExport(cmd, c, args[0], platformOnionKeyFetchCommandID, false)
	},
}

// fetchOnionKeyExport reads the sealed blob, printing it with the
// read-once warning. With wait=true (export --fetch) it polls through the
// 409 window while the agent is still working.
func fetchOnionKeyExport(cmd *cobra.Command, c *sdkclient.Client, deploymentID, commandID string, wait bool) error {
	var blob *sdkclient.OnionKeyExportBlob
	for attempt := 0; ; attempt++ {
		var err error
		blob, err = c.PlatformFetchOnionKeyExport(cmd.Context(), deploymentID, commandID)
		if err == nil {
			break
		}
		var conflict *sdkclient.Conflict
		if !wait || !errors.As(err, &conflict) || attempt >= 23 {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(5 * time.Second):
		}
	}
	f, err := resolveFormat()
	if err != nil {
		return err
	}
	if f != output.FormatTable {
		return renderJSONOrYAML(cmd.OutOrStdout(), blob, f)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "SAVE THIS BLOB NOW — it was shown exactly once; this read removed it from the platform.")
	if blob.Note != "" {
		fmt.Fprintf(w, "Note: %s\n", blob.Note)
	}
	fmt.Fprintf(w, "\nonion: %s\nsealed: %s\n", blob.Onion, blob.Sealed)
	fmt.Fprintln(w, "\nUnseal locally with the recipient PRIVATE key (PyNaCl SealedBox / libsodium crypto_box_seal_open).")
	return nil
}

var (
	platformOnionKeyRotateConfirm        bool
	platformOnionKeyRotateConfirmAddress string
)

var platformOnionKeyRotateCmd = &cobra.Command{
	Use:   "rotate <deployment-id>",
	Short: "Rotate the hidden-service key — the .onion address CHANGES (destructive).",
	Long: `Rotate the hidden-service key: the agent mints a fresh ed25519 key and
the .onion address CHANGES — the old address stops working for visitors,
permanently. Bookmarks, links and published references die with it. The
old key is parked on the host for support-assisted recovery only; no
command restores it. Export the key first (onion-key export) if you may
want the old address back on a future deploy.

Requires --confirm AND --confirm-address with the CURRENT .onion,
verbatim — proof you know which address you are killing. The new address
appears on the deployment row when the agent reports. Requires an agent
with onion-custody-v1 (422 on older agents).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !platformOnionKeyRotateConfirm {
			return fmt.Errorf("rotation is destructive (the address changes permanently) — re-run with --confirm")
		}
		if platformOnionKeyRotateConfirmAddress == "" {
			return fmt.Errorf("--confirm-address is required: the CURRENT .onion, verbatim (see: impreza platform deployments show %s)", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformRotateOnionKey(cmd.Context(), args[0], platformOnionKeyRotateConfirmAddress)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Rotation enqueued. command_id=%s\n", out.CommandID)
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
		fmt.Fprintf(w, "The new address lands on the deployment row; track with:\n  impreza platform deployments show %s\n", args[0])
		return nil
	},
}

var (
	platformOnionKeyPurgeConfirm        bool
	platformOnionKeyPurgeConfirmAddress string
)

var platformOnionKeyPurgeCmd = &cobra.Command{
	Use:   "purge <deployment-id>",
	Short: "Destroy a RETAINED onion identity — parked recovery copies and reservation (irreversible).",
	Long: `Destroy every parked recovery copy of one .onion address of a
deployment, and release the platform-side reservation of that address.

Agent success confirms deletion of retained copies on the current host.
Deletion is irreversible, but prior exports and external backups are unaffected.
A queued command is not confirmation that deletion has finished.

The address must be a RETAINED identity: a prior address left by
onion-key rotate, or the stale address of an uninstalled deployment.
Purging the CURRENT address of a running deployment is refused — export it
first (onion-key export) and rotate if you need it gone.

Requires --confirm AND --confirm-address with the address being destroyed,
verbatim. Agent mode requires an agent with onion-purge-v1 (422 on older
agents); addresses that only live in control-plane records are released
without an agent command.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !platformOnionKeyPurgeConfirm {
			return fmt.Errorf("deleting retained key copies is irreversible — re-run with --confirm")
		}
		if platformOnionKeyPurgeConfirmAddress == "" {
			return fmt.Errorf("--confirm-address is required: the retained .onion being destroyed, verbatim")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformPurgeOnionKey(cmd.Context(), args[0], platformOnionKeyPurgeConfirmAddress)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		w := cmd.OutOrStdout()
		if out.Mode == "agent" {
			fmt.Fprintf(w, "Purge enqueued. command_id=%s\n", out.CommandID)
		} else {
			fmt.Fprintf(w, "Reservation released only; key deletion on the former host is NOT verified.\n")
		}
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
		return nil
	},
}

func init() {
	platformOnionKeyExportCmd.Flags().StringVar(&platformOnionKeyExportRecipient, "recipient-pubkey", "", "Standard base64 of your locally generated 32-byte X25519 public key (required).")
	platformOnionKeyExportCmd.Flags().BoolVar(&platformOnionKeyExportFetch, "fetch", false, "Wait for the agent and read the sealed blob immediately (read-once).")
	platformOnionKeyFetchCmd.Flags().StringVar(&platformOnionKeyFetchCommandID, "command-id", "", "The cmd_... id from onion-key export (required).")
	platformOnionKeyRotateCmd.Flags().BoolVar(&platformOnionKeyRotateConfirm, "confirm", false, "Required gate — rotation changes the .onion address permanently.")
	platformOnionKeyRotateCmd.Flags().StringVar(&platformOnionKeyRotateConfirmAddress, "confirm-address", "", "The CURRENT .onion address, verbatim (required).")
	platformOnionKeyPurgeCmd.Flags().BoolVar(&platformOnionKeyPurgeConfirm, "confirm", false, "Required gate — purge destroys the retained identity irreversibly.")
	platformOnionKeyPurgeCmd.Flags().StringVar(&platformOnionKeyPurgeConfirmAddress, "confirm-address", "", "The retained .onion address being destroyed, verbatim (required).")

	platformOnionKeyCmd.AddCommand(
		platformOnionKeyExportCmd,
		platformOnionKeyFetchCmd,
		platformOnionKeyRotateCmd,
		platformOnionKeyPurgeCmd,
	)
	platformCmd.AddCommand(platformOnionKeyCmd)
}
