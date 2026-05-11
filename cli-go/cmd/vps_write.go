package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// autoYes is the shared --yes / -y flag. Bound from each command in
// init(). Mutation verbs read it from confirmOrExit().
var autoYes bool

// ── vps start / stop / reboot / shutdown ─────────────────────────

var vpsStartCmd = &cobra.Command{
	Use:   "start <id>",
	Short: "Boot a stopped VPS.",
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
		if err := c.VpsStart(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("VPS %d start signal sent.", id)
		return nil
	},
}

var vpsStopCmd = &cobra.Command{
	Use:   "stop <id>",
	Short: "Force-stop a VPS (no ACPI; may corrupt unwritten data). Prefer `shutdown`.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Force-stop VPS %d? (any unwritten data will be lost)", id),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.VpsStop(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("VPS %d force-stop signal sent.", id)
		return nil
	},
}

var vpsRebootCmd = &cobra.Command{
	Use:   "reboot <id>",
	Short: "Reboot a VPS.",
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
		if err := c.VpsReboot(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("VPS %d reboot signal sent.", id)
		return nil
	},
}

var vpsShutdownCmd = &cobra.Command{
	Use:   "shutdown <id>",
	Short: "Send ACPI shutdown; falls back to power-off after timeout.",
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
		if err := c.VpsShutdown(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("VPS %d shutdown signal sent.", id)
		return nil
	},
}

// ── vps set-hostname ─────────────────────────────────────────────

var (
	vpsHostnameNew string
)

var vpsSetHostnameCmd = &cobra.Command{
	Use:   "set-hostname <id>",
	Short: "Change the VPS hostname.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if vpsHostnameNew == "" {
			return fmt.Errorf("--hostname is required")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.VpsSetHostname(cmd.Context(), id, vpsHostnameNew); err != nil {
			return err
		}
		output.Success("VPS %d hostname set to %q.", id, vpsHostnameNew)
		output.Info("Some Cloud images apply the change only on next boot.")
		return nil
	},
}

// ── vps set-password ─────────────────────────────────────────────

var (
	vpsPasswordNew     string
	vpsPasswordPrompt  bool
)

var vpsSetPasswordCmd = &cobra.Command{
	Use:   "set-password <id>",
	Short: "Change the VPS root password.",
	Long: `Change the VPS root password.

Provide the new password via --password, OR via stdin (--prompt) so
it doesn't appear in shell history. Without either, the command
errors out.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}

		pw := vpsPasswordNew
		if pw == "" {
			if !vpsPasswordPrompt {
				return errors.New("--password is required, or pass --prompt to read it from stdin")
			}
			pw, err = readSecretFromTTY(cmd.ErrOrStderr(), "New root password: ")
			if err != nil {
				return err
			}
			confirm, err := readSecretFromTTY(cmd.ErrOrStderr(), "Confirm: ")
			if err != nil {
				return err
			}
			if pw != confirm {
				return errors.New("passwords do not match")
			}
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.VpsSetPassword(cmd.Context(), id, pw); err != nil {
			return err
		}
		output.Success("VPS %d root password updated.", id)
		return nil
	},
}

// readSecretFromTTY prompts on stderr, reads a line of bytes from
// stdin without echoing them, and returns the string.
func readSecretFromTTY(prompt interface{ Write(p []byte) (int, error) }, label string) (string, error) {
	_, _ = prompt.Write([]byte(label))
	defer func() { _, _ = prompt.Write([]byte("\n")) }()

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-TTY (CI pipe, etc.): fall back to line read.
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ── vps reinstall ────────────────────────────────────────────────

var (
	vpsReinstallTemplate string
	vpsReinstallPassword string
	vpsReinstallPrompt   bool
	vpsReinstallWait     bool
	vpsReinstallTimeout  int
)

var vpsReinstallCmd = &cobra.Command{
	Use:   "reinstall <id>",
	Short: "Reinstall the OS template (**destructive** — wipes everything).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if vpsReinstallTemplate == "" {
			return errors.New("--template is required")
		}
		pw := vpsReinstallPassword
		if pw == "" {
			if !vpsReinstallPrompt {
				return errors.New("--password is required, or pass --prompt to read it from stdin")
			}
			pw, err = readSecretFromTTY(cmd.ErrOrStderr(), "New root password: ")
			if err != nil {
				return err
			}
		}

		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Reinstall VPS %d with template %q? ALL DATA WILL BE LOST.",
				id, vpsReinstallTemplate),
			autoYes); err != nil {
			return err
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		op, err := c.VpsReinstall(cmd.Context(), id, client.VpsReinstallRequest{
			Template: vpsReinstallTemplate, Password: pw, Confirm: true,
		})
		if err != nil {
			return err
		}
		if op == nil {
			output.Success("VPS %d reinstall completed (Cloud is synchronous).", id)
			return nil
		}
		output.Info("Reinstall queued — Proxmox returned operation %s.", op.UUID)
		if !vpsReinstallWait {
			output.Info("Pass --wait to block until the operation completes.")
			return nil
		}
		return waitForOperation(cmd.Context(), cmd.ErrOrStderr(), op, vpsReinstallTimeout)
	},
}

// ── vps migrate ──────────────────────────────────────────────────

var (
	vpsMigrateTarget  string
	vpsMigrateWait    bool
	vpsMigrateTimeout int
)

var vpsMigrateCmd = &cobra.Command{
	Use:   "migrate <id>",
	Short: "Move a Proxmox VPS to another node (Proxmox-only).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if vpsMigrateTarget == "" {
			return errors.New("--target is required (server_id or group_id at the destination)")
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Migrate VPS %d to target %s? The guest will pause during migration.",
				id, vpsMigrateTarget),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		op, err := c.VpsMigrate(cmd.Context(), id, client.VpsMigrateRequest{Target: vpsMigrateTarget})
		if err != nil {
			return err
		}
		output.Info("Migration queued — Proxmox returned operation %s.", op.UUID)
		if !vpsMigrateWait {
			output.Info("Pass --wait to block until the operation completes.")
			return nil
		}
		return waitForOperation(cmd.Context(), cmd.ErrOrStderr(), op, vpsMigrateTimeout)
	},
}

// ── vps cancel ───────────────────────────────────────────────────

var (
	vpsCancelType   string
	vpsCancelReason string
)

var vpsCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Submit a cancellation request for a VPS (staff approves termination).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Submit %q cancel request for VPS %d?", vpsCancelType, id),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.VpsCancel(cmd.Context(), id, vpsCancelType, vpsCancelReason); err != nil {
			return err
		}
		output.Success("Cancel request submitted for VPS %d (type=%s). Staff will approve the actual termination.",
			id, vpsCancelType)
		return nil
	},
}

func init() {
	// Shared --yes flag on every mutation verb.
	for _, c := range []*cobra.Command{
		vpsStopCmd, domainDnsDeleteCmd,
		vpsSetPasswordCmd, vpsReinstallCmd, vpsMigrateCmd, vpsCancelCmd,
	} {
		c.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the interactive confirmation prompt.")
	}

	vpsSetHostnameCmd.Flags().StringVar(&vpsHostnameNew, "hostname", "",
		"New hostname for the VPS.")

	vpsSetPasswordCmd.Flags().StringVar(&vpsPasswordNew, "password", "",
		"New root password. Prefer --prompt to keep it out of shell history.")
	vpsSetPasswordCmd.Flags().BoolVar(&vpsPasswordPrompt, "prompt", false,
		"Read the new password from stdin (no echo).")

	vpsReinstallCmd.Flags().StringVar(&vpsReinstallTemplate, "template", "",
		"OS template id (e.g. debian-12). List via the Impreza Account panel.")
	vpsReinstallCmd.Flags().StringVar(&vpsReinstallPassword, "password", "",
		"New root password.")
	vpsReinstallCmd.Flags().BoolVar(&vpsReinstallPrompt, "prompt", false,
		"Read the root password from stdin.")
	vpsReinstallCmd.Flags().BoolVar(&vpsReinstallWait, "wait", false,
		"Block until the reinstall completes (Proxmox only).")
	vpsReinstallCmd.Flags().IntVar(&vpsReinstallTimeout, "timeout", 1800,
		"Wait timeout in seconds (default 1800 = 30 min).")

	vpsMigrateCmd.Flags().StringVar(&vpsMigrateTarget, "target", "",
		"Destination server_id or group_id.")
	vpsMigrateCmd.Flags().BoolVar(&vpsMigrateWait, "wait", false,
		"Block until the migration completes.")
	vpsMigrateCmd.Flags().IntVar(&vpsMigrateTimeout, "timeout", 1800,
		"Wait timeout in seconds (default 1800).")

	vpsCancelCmd.Flags().StringVar(&vpsCancelType, "type", "End of Billing Period",
		"Cancel type: 'Immediate' or 'End of Billing Period'.")
	vpsCancelCmd.Flags().StringVar(&vpsCancelReason, "reason", "",
		"Optional reason text the cancellation request carries.")

	vpsCmd.AddCommand(
		vpsStartCmd, vpsStopCmd, vpsRebootCmd, vpsShutdownCmd,
		vpsSetHostnameCmd, vpsSetPasswordCmd,
		vpsReinstallCmd, vpsMigrateCmd, vpsCancelCmd,
	)
}
