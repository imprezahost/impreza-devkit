package cmd

import (
	"fmt"
	"strconv"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// `vps proxmox` is the Proxmox-only sub-namespace mounted on `vpsCmd`.
// 7.4 lands snapshots / backups / backup-schedules + the inline
// `vps proxmox network reconfigure` verb.

var vpsProxmoxCmd = &cobra.Command{
	Use:   "proxmox",
	Short: "Proxmox-only sub-resources (snapshots, backups, backup schedules, network).",
}

// ── snapshots ─────────────────────────────────────────────────────

var vpsProxmoxSnapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "VPS snapshot lifecycle (Proxmox only).",
}

var vpsProxmoxSnapshotsListCmd = &cobra.Command{
	Use:   "list <vps-id>",
	Short: "List existing snapshots.",
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
		snaps, err := c.ProxmoxSnapshotsList(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), snaps, f)
		}
		if len(snaps) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No snapshots.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"name", "description", "created_at", "parent"})
		for _, s := range snaps {
			t.AppendRow(table.Row{s.Name, s.Description, s.CreatedAt, s.Parent})
		}
		t.Render()
		return nil
	},
}

var (
	snapCreateDescription string
)

var vpsProxmoxSnapshotsCreateCmd = &cobra.Command{
	Use:   "create <vps-id> <snapshot-name>",
	Short: "Take a new snapshot.",
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
		snap, err := c.ProxmoxSnapshotCreate(cmd.Context(), id, args[1], snapCreateDescription)
		if err != nil {
			return err
		}
		output.Success("Snapshot %q created on VPS %d.", snap.Name, id)
		return nil
	},
}

var vpsProxmoxSnapshotsDeleteCmd = &cobra.Command{
	Use:     "delete <vps-id> <snapshot-name>",
	Aliases: []string{"rm", "remove"},
	Short:   "Remove a snapshot by name.",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete snapshot %q on VPS %d?", args[1], id), autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.ProxmoxSnapshotDelete(cmd.Context(), id, args[1]); err != nil {
			return err
		}
		output.Success("Snapshot %q on VPS %d deleted.", args[1], id)
		return nil
	},
}

var (
	snapRollbackWait    bool
	snapRollbackTimeout int
)

var vpsProxmoxSnapshotsRollbackCmd = &cobra.Command{
	Use:   "rollback <vps-id> <snapshot-name>",
	Short: "Roll the VPS back to a snapshot (**destructive**).",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Roll VPS %d back to snapshot %q? ALL CHANGES SINCE WILL BE LOST.", id, args[1]),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		op, err := c.ProxmoxSnapshotRollback(cmd.Context(), id, args[1])
		if err != nil {
			return err
		}
		output.Info("Rollback queued — operation %s.", op.UUID)
		if !snapRollbackWait {
			output.Info("Pass --wait to block until the operation completes.")
			return nil
		}
		return waitForOperation(cmd.Context(), cmd.ErrOrStderr(), op, snapRollbackTimeout)
	},
}

// ── backups ───────────────────────────────────────────────────────

var vpsProxmoxBackupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "VPS backup lifecycle (Proxmox only).",
}

var vpsProxmoxBackupsListCmd = &cobra.Command{
	Use:   "list <vps-id>",
	Short: "List existing backups.",
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
		bks, err := c.ProxmoxBackupsList(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), bks, f)
		}
		if len(bks) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No backups.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "filename", "size_mb", "created_at", "storage"})
		for _, b := range bks {
			sizeMB := ""
			if b.Size > 0 {
				sizeMB = fmt.Sprintf("%d", b.Size/(1024*1024))
			}
			t.AppendRow(table.Row{fmt.Sprintf("%v", b.ID), b.Filename, sizeMB, b.CreatedAt, b.Storage})
		}
		t.Render()
		return nil
	},
}

var (
	backupCreateWait    bool
	backupCreateTimeout int
)

var vpsProxmoxBackupsCreateCmd = &cobra.Command{
	Use:   "create <vps-id>",
	Short: "Trigger a new backup. Long-running — pair with --wait.",
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
		op, err := c.ProxmoxBackupCreate(cmd.Context(), id)
		if err != nil {
			return err
		}
		output.Info("Backup queued — operation %s.", op.UUID)
		if !backupCreateWait {
			output.Info("Pass --wait to block until the operation completes.")
			return nil
		}
		return waitForOperation(cmd.Context(), cmd.ErrOrStderr(), op, backupCreateTimeout)
	},
}

var (
	backupRestoreWait    bool
	backupRestoreTimeout int
)

var vpsProxmoxBackupsRestoreCmd = &cobra.Command{
	Use:   "restore <vps-id> <backup-id>",
	Short: "Restore from a backup (**destructive**).",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Restore VPS %d from backup %q? Current state will be overwritten.", id, args[1]),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		op, err := c.ProxmoxBackupRestore(cmd.Context(), id, args[1])
		if err != nil {
			return err
		}
		output.Info("Restore queued — operation %s.", op.UUID)
		if !backupRestoreWait {
			output.Info("Pass --wait to block until the operation completes.")
			return nil
		}
		return waitForOperation(cmd.Context(), cmd.ErrOrStderr(), op, backupRestoreTimeout)
	},
}

var vpsProxmoxBackupsDeleteCmd = &cobra.Command{
	Use:     "delete <vps-id> <backup-id>",
	Aliases: []string{"rm", "remove"},
	Short:   "Remove a backup by id.",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete backup %q on VPS %d?", args[1], id), autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.ProxmoxBackupDelete(cmd.Context(), id, args[1]); err != nil {
			return err
		}
		output.Success("Backup %q on VPS %d deleted.", args[1], id)
		return nil
	},
}

// ── backup-schedules ─────────────────────────────────────────────

var vpsProxmoxSchedulesCmd = &cobra.Command{
	Use:   "backup-schedules",
	Short: "Periodic backup schedule lifecycle (Proxmox only).",
}

var vpsProxmoxSchedulesListCmd = &cobra.Command{
	Use:   "list <vps-id>",
	Short: "List existing backup schedules.",
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
		scheds, err := c.ProxmoxBackupSchedulesList(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), scheds, f)
		}
		if len(scheds) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No schedules.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "dow", "starttime", "mode", "compress"})
		for _, s := range scheds {
			t.AppendRow(table.Row{fmt.Sprintf("%v", s.ID), s.Dow, s.Starttime, s.Mode, s.Compress})
		}
		t.Render()
		return nil
	},
}

var validBackupModes = map[string]bool{"snapshot": true, "suspend": true, "stop": true}
var validBackupCompress = map[string]bool{"zstd": true, "lzo": true, "gzip": true, "none": true}

var (
	schedCreateDow      string
	schedCreateHour     int
	schedCreateMinute   int
	schedCreateMode     string
	schedCreateCompress string
)

var vpsProxmoxSchedulesCreateCmd = &cobra.Command{
	Use:   "create <vps-id>",
	Short: "Create a periodic backup schedule.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if schedCreateDow == "" {
			return fmt.Errorf("--dow is required (e.g. 'mon,wed,fri')")
		}
		if schedCreateMode != "" && !validBackupModes[schedCreateMode] {
			return fmt.Errorf("--mode must be one of snapshot|suspend|stop (got %q)", schedCreateMode)
		}
		if schedCreateCompress != "" && !validBackupCompress[schedCreateCompress] {
			return fmt.Errorf("--compress must be one of zstd|lzo|gzip|none (got %q)", schedCreateCompress)
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		s, err := c.ProxmoxBackupScheduleCreate(cmd.Context(), id, client.BackupScheduleCreateRequest{
			Dow:      schedCreateDow,
			Hour:     schedCreateHour,
			Minute:   schedCreateMinute,
			Mode:     schedCreateMode,
			Compress: schedCreateCompress,
		})
		if err != nil {
			return err
		}
		// Server's POST response only echoes the new id (no dow/time);
		// print what the caller supplied so the success line is useful.
		output.Success("Backup schedule created on VPS %d (id=%v, dow=%s, time=%02d:%02d).",
			id, s.ID, schedCreateDow, schedCreateHour, schedCreateMinute)
		return nil
	},
}

var vpsProxmoxSchedulesDeleteCmd = &cobra.Command{
	Use:     "delete <vps-id> <schedule-id>",
	Aliases: []string{"rm", "remove"},
	Short:   "Remove a backup schedule.",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete backup schedule %q on VPS %d?", args[1], id), autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.ProxmoxBackupScheduleDelete(cmd.Context(), id, args[1]); err != nil {
			return err
		}
		output.Success("Backup schedule %q on VPS %d deleted.", args[1], id)
		return nil
	},
}

// ── network reconfigure ─────────────────────────────────────────

var vpsProxmoxNetworkCmd = &cobra.Command{
	Use:   "network",
	Short: "Proxmox VPS networking operations.",
}

var vpsProxmoxNetworkReconfigureCmd = &cobra.Command{
	Use:   "reconfigure <vps-id>",
	Short: "Apply pending network config (Guest Agent or reboot required).",
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
		if _, err := c.ProxmoxNetworkReconfigure(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Network reconfigure triggered on VPS %d.", id)
		output.Info("Guest Agent applies the change; a reboot may be required for full effect.")
		return nil
	},
}

func init() {
	// snapshots flags
	vpsProxmoxSnapshotsCreateCmd.Flags().StringVar(&snapCreateDescription, "description", "",
		"Optional description for the snapshot.")
	vpsProxmoxSnapshotsDeleteCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	vpsProxmoxSnapshotsRollbackCmd.Flags().BoolVar(&snapRollbackWait, "wait", false,
		"Block until the rollback completes.")
	vpsProxmoxSnapshotsRollbackCmd.Flags().IntVar(&snapRollbackTimeout, "timeout", 600,
		"Wait timeout in seconds (default 600).")
	vpsProxmoxSnapshotsRollbackCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	vpsProxmoxSnapshotsCmd.AddCommand(
		vpsProxmoxSnapshotsListCmd, vpsProxmoxSnapshotsCreateCmd,
		vpsProxmoxSnapshotsDeleteCmd, vpsProxmoxSnapshotsRollbackCmd,
	)

	// backups flags
	vpsProxmoxBackupsCreateCmd.Flags().BoolVar(&backupCreateWait, "wait", false,
		"Block until the backup completes.")
	vpsProxmoxBackupsCreateCmd.Flags().IntVar(&backupCreateTimeout, "timeout", 1800,
		"Wait timeout in seconds (default 1800).")
	vpsProxmoxBackupsRestoreCmd.Flags().BoolVar(&backupRestoreWait, "wait", false,
		"Block until the restore completes.")
	vpsProxmoxBackupsRestoreCmd.Flags().IntVar(&backupRestoreTimeout, "timeout", 1800,
		"Wait timeout in seconds (default 1800).")
	vpsProxmoxBackupsRestoreCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	vpsProxmoxBackupsDeleteCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	vpsProxmoxBackupsCmd.AddCommand(
		vpsProxmoxBackupsListCmd, vpsProxmoxBackupsCreateCmd,
		vpsProxmoxBackupsRestoreCmd, vpsProxmoxBackupsDeleteCmd,
	)

	// schedules flags — match the Python CLI's flag names (dow/hour/minute/
	// mode/compress) so the two CLIs are interchangeable.
	vpsProxmoxSchedulesCreateCmd.Flags().StringVar(&schedCreateDow, "dow", "",
		"Day-of-week selector: comma-separated tokens (e.g. 'mon,wed,fri'). "+
			"Tokens pass through to Proxmox verbatim.")
	vpsProxmoxSchedulesCreateCmd.Flags().IntVar(&schedCreateHour, "hour", 3,
		"Hour of day, 0-23 (default 3 = off-peak).")
	vpsProxmoxSchedulesCreateCmd.Flags().IntVar(&schedCreateMinute, "minute", 0,
		"Minute of the hour, 0-59 (default 0).")
	vpsProxmoxSchedulesCreateCmd.Flags().StringVar(&schedCreateMode, "mode", "",
		"Backup mode: snapshot | suspend | stop (default Proxmox: snapshot).")
	vpsProxmoxSchedulesCreateCmd.Flags().StringVar(&schedCreateCompress, "compress", "",
		"Compression: zstd | lzo | gzip | none (default Proxmox: zstd).")
	vpsProxmoxSchedulesDeleteCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	vpsProxmoxSchedulesCmd.AddCommand(
		vpsProxmoxSchedulesListCmd, vpsProxmoxSchedulesCreateCmd, vpsProxmoxSchedulesDeleteCmd,
	)

	// network
	vpsProxmoxNetworkCmd.AddCommand(vpsProxmoxNetworkReconfigureCmd)

	// proxmox root → 4 sub-groups
	vpsProxmoxCmd.AddCommand(
		vpsProxmoxSnapshotsCmd, vpsProxmoxBackupsCmd,
		vpsProxmoxSchedulesCmd, vpsProxmoxNetworkCmd,
	)
	vpsCmd.AddCommand(vpsProxmoxCmd)
}
