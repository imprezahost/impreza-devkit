package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// `vps cloud` is the Cloud-only sub-namespace mounted on `vpsCmd`.
// 7.4 lands images / rescue / iso / ssh-keys sub-groups + 5 inline
// verbs (vnc / vnc-password / resize / boot-order / ipv6).
//
// rdns get/set/delete: removed from the CLI surface in 7.7 — the
// /vps/cloud/rdns/{dotted-ipv4} endpoint hits a WAF/mod_security
// rule on the public edge and returns the maintenance HTML page
// instead of a JSON response. Server-side fix is owned by whoever
// configures the WAF rules; until that lands, the CLI hides the
// broken verbs rather than ship a known-broken surface. The Python
// SDK + Go client methods stay intact (CloudRdnsGet / CloudRdnsSet /
// CloudRdnsDelete) so library users can still call them and handle
// the WAF response on their side.

var vpsCloudCmd = &cobra.Command{
	Use:   "cloud",
	Short: "Cloud-only sub-resources (images, rescue, iso, ssh-keys + inline verbs).",
}

// ── images ────────────────────────────────────────────────────────

var vpsCloudImagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Saved Cloud VM images (account-scoped catalog).",
}

var vpsCloudImagesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List saved images on the account.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		imgs, err := c.CloudImagesList(cmd.Context())
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), imgs, f)
		}
		if len(imgs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No saved images on this account.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "name", "status", "size_mb", "vm_id", "created_at"})
		for _, i := range imgs {
			t.AppendRow(table.Row{fmt.Sprintf("%v", i.ID), i.Name, i.Status, i.SizeMB,
				fmt.Sprintf("%v", i.VmID), i.CreatedAt})
		}
		t.Render()
		return nil
	},
}

var vpsCloudImagesCreateCmd = &cobra.Command{
	Use:   "create <vps-id>",
	Short: "Snapshot the bound VM's current state into a saved image.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudImageCreate(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Image creation queued for VPS %d.", id)
		output.Info("Run `impreza vps cloud images list` after a few minutes — the upstream queue takes time.")
		return nil
	},
}

var vpsCloudImagesRestoreCmd = &cobra.Command{
	Use:   "restore <vps-id> <image-id>",
	Short: "Restore the bound VM from a saved image (**destructive**).",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Restore VPS %d from image %q? Current state will be overwritten.", id, args[1]),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, err := c.CloudImageRestore(cmd.Context(), id, args[1]); err != nil {
			return err
		}
		output.Success("Restore initiated on VPS %d from image %q.", id, args[1])
		return nil
	},
}

var vpsCloudImagesDeleteCmd = &cobra.Command{
	Use:     "delete <image-id>",
	Aliases: []string{"rm", "remove"},
	Short:   "Delete a saved image from the account.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete image %q from the account? Irreversible.", args[0]), autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudImageDelete(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("Image %q deleted.", args[0])
		return nil
	},
}

// ── rescue ────────────────────────────────────────────────────────

var vpsCloudRescueCmd = &cobra.Command{
	Use:   "rescue",
	Short: "Cloud VPS rescue-mode boot.",
}

var rescuePassword string

var vpsCloudRescueEnableCmd = &cobra.Command{
	Use:   "enable <vps-id>",
	Short: "Enable rescue mode (reboot to enter).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		pw := rescuePassword
		if pw == "" {
			pw, err = readSecretFromTTY(cmd.ErrOrStderr(), "Rescue-mode root password: ")
			if err != nil {
				return err
			}
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, err := c.CloudRescueEnable(cmd.Context(), id, pw); err != nil {
			return err
		}
		output.Success("Rescue mode enabled on VPS %d. Reboot the VM to enter rescue.", id)
		return nil
	},
}

var vpsCloudRescueDisableCmd = &cobra.Command{
	Use:   "disable <vps-id>",
	Short: "Disable rescue mode (reboot to resume normal boot).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudRescueDisable(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Rescue mode disabled on VPS %d. Reboot to resume normal boot.", id)
		return nil
	},
}

// ── iso ───────────────────────────────────────────────────────────

var vpsCloudIsoCmd = &cobra.Command{
	Use:   "iso",
	Short: "Cloud VPS ISO mount / unmount.",
}

var vpsCloudIsoMountCmd = &cobra.Command{
	Use:   "mount <vps-id> <iso-name>",
	Short: "Mount an ISO. Available ISOs vary per location.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, err := c.CloudIsoMount(cmd.Context(), id, args[1]); err != nil {
			return err
		}
		output.Success("ISO %q mounted on VPS %d.", args[1], id)
		return nil
	},
}

var vpsCloudIsoUnmountCmd = &cobra.Command{
	Use:   "unmount <vps-id>",
	Short: "Unmount the currently-mounted ISO.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudIsoUnmount(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("ISO unmounted from VPS %d.", id)
		return nil
	},
}

// ── rdns ──────────────────────────────────────────────────────────
//
// Removed from CLI in 7.7 (WAF-blocked on /vps/cloud/rdns/{ip}).
// Client methods CloudRdnsGet / CloudRdnsSet / CloudRdnsDelete stay
// available for library consumers. Re-add the cobra wrappers here
// when the server-side WAF rule is fixed.

// ── ssh-keys ─────────────────────────────────────────────────────

var vpsCloudSshKeysCmd = &cobra.Command{
	Use:   "ssh-keys",
	Short: "Account-level SSH keys (Cloud VPS).",
}

var vpsCloudSshKeysListCmd = &cobra.Command{
	Use:   "list",
	Short: "List SSH keys registered on the account.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		keys, err := c.CloudSshKeysList(cmd.Context())
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), keys, f)
		}
		if len(keys) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No SSH keys on this account.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "name", "fingerprint", "created_at"})
		for _, k := range keys {
			t.AppendRow(table.Row{fmt.Sprintf("%v", k.ID), k.Name, k.Fingerprint, k.CreatedAt})
		}
		t.Render()
		return nil
	},
}

var sshKeysAssignFlag string

var vpsCloudSshKeysAssignCmd = &cobra.Command{
	Use:   "assign <vps-id>",
	Short: "Attach one or more existing account-level keys to the bound VPS.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if sshKeysAssignFlag == "" {
			return errors.New("--keys is required (comma-separated key ids or names)")
		}
		keys := []string{}
		for _, k := range strings.Split(sshKeysAssignFlag, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				keys = append(keys, k)
			}
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, err := c.CloudSshKeysAssign(cmd.Context(), id, keys); err != nil {
			return err
		}
		output.Success("Assigned %d SSH key(s) to VPS %d.", len(keys), id)
		return nil
	},
}

// ── inline Cloud verbs (vps cloud vnc / vnc-password / resize /
//                       boot-order / ipv6) ──────────────────────

var vpsCloudVncCmd = &cobra.Command{
	Use:   "vnc <vps-id>",
	Short: "Show VNC client credentials (ip / port / password).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		v, err := c.CloudVnc(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), v, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"ip", v.IP})
		t.AppendRow(table.Row{"port", v.Port})
		t.AppendRow(table.Row{"password", v.Password})
		t.Render()
		return nil
	},
}

var vncPasswordNew string

var vpsCloudVncPasswordCmd = &cobra.Command{
	Use:   "vnc-password <vps-id>",
	Short: "Rotate the VNC password.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		pw := vncPasswordNew
		if pw == "" {
			pw, err = readSecretFromTTY(cmd.ErrOrStderr(), "New VNC password: ")
			if err != nil {
				return err
			}
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudVncPassword(cmd.Context(), id, pw); err != nil {
			return err
		}
		output.Success("VNC password rotated on VPS %d.", id)
		return nil
	},
}

var resizeInstanceSize string

var vpsCloudResizeCmd = &cobra.Command{
	Use:   "resize <vps-id>",
	Short: "Resize the Cloud VPS to a new instance size. Reboot required.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if resizeInstanceSize == "" {
			return errors.New("--size is required (instance size identifier from the Cloud backend)")
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Resize VPS %d to %q? Pro-rata cost is charged from balance; reboot required.",
				id, resizeInstanceSize),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if _, err := c.CloudResize(cmd.Context(), id, resizeInstanceSize); err != nil {
			return err
		}
		output.Success("VPS %d resize to %q queued. Reboot to apply.", id, resizeInstanceSize)
		return nil
	},
}

var bootOrder string

var vpsCloudBootOrderCmd = &cobra.Command{
	Use:   "boot-order <vps-id>",
	Short: "Set the boot order: cda (disk→cdrom→network) or dca (cdrom→disk→network).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if bootOrder == "" {
			return errors.New("--order is required: 'cda' or 'dca'")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudBootOrder(cmd.Context(), id, bootOrder); err != nil {
			return err
		}
		output.Success("Boot order on VPS %d set to %q.", id, bootOrder)
		return nil
	},
}

var vpsCloudIpv6Cmd = &cobra.Command{
	Use:   "ipv6 <vps-id>",
	Short: "Enable IPv6 on the bound VPS.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.CloudIpv6Enable(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("IPv6 enabled on VPS %d.", id)
		return nil
	},
}

func init() {
	// images
	vpsCloudImagesRestoreCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	vpsCloudImagesDeleteCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	vpsCloudImagesCmd.AddCommand(
		vpsCloudImagesListCmd, vpsCloudImagesCreateCmd,
		vpsCloudImagesRestoreCmd, vpsCloudImagesDeleteCmd,
	)

	// rescue
	vpsCloudRescueEnableCmd.Flags().StringVar(&rescuePassword, "password", "",
		"Rescue-mode root password. Prompted from stdin if omitted.")
	vpsCloudRescueCmd.AddCommand(vpsCloudRescueEnableCmd, vpsCloudRescueDisableCmd)

	// iso
	vpsCloudIsoCmd.AddCommand(vpsCloudIsoMountCmd, vpsCloudIsoUnmountCmd)

	// ssh-keys
	vpsCloudSshKeysAssignCmd.Flags().StringVar(&sshKeysAssignFlag, "keys", "",
		"Comma-separated SSH key ids or names to assign.")
	vpsCloudSshKeysCmd.AddCommand(vpsCloudSshKeysListCmd, vpsCloudSshKeysAssignCmd)

	// inline verbs
	vpsCloudVncPasswordCmd.Flags().StringVar(&vncPasswordNew, "password", "",
		"New VNC password. Prompted from stdin if omitted.")
	vpsCloudResizeCmd.Flags().StringVar(&resizeInstanceSize, "size", "",
		"New instance size identifier (from the Cloud backend's available sizes list).")
	vpsCloudResizeCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	vpsCloudBootOrderCmd.Flags().StringVar(&bootOrder, "order", "",
		"Boot order: 'cda' or 'dca'.")

	// vps cloud → 4 sub-groups + 5 inline verbs (rdns sub-group
	// removed in 7.7; see comment block above the cmd definitions).
	vpsCloudCmd.AddCommand(
		vpsCloudImagesCmd, vpsCloudRescueCmd, vpsCloudIsoCmd,
		vpsCloudSshKeysCmd,
		vpsCloudVncCmd, vpsCloudVncPasswordCmd, vpsCloudResizeCmd,
		vpsCloudBootOrderCmd, vpsCloudIpv6Cmd,
	)
	vpsCmd.AddCommand(vpsCloudCmd)
}
