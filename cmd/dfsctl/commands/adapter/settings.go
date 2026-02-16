package adapter

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// settingsAdapterType stores the adapter type parsed from the parent command args.
var settingsAdapterType string

// syncGlobalFlags replicates the rootCmd PersistentPreRun flag syncing.
// This is needed because Cobra's PersistentPreRunE does not chain
// when overridden by a subcommand, so each settings command must
// explicitly sync the inherited global flags.
func syncGlobalFlags(cmd *cobra.Command) {
	cmdutil.Flags.ServerURL, _ = cmd.Flags().GetString("server")
	cmdutil.Flags.Token, _ = cmd.Flags().GetString("token")
	cmdutil.Flags.Output, _ = cmd.Flags().GetString("output")
	cmdutil.Flags.NoColor, _ = cmd.Flags().GetBool("no-color")
	cmdutil.Flags.Verbose, _ = cmd.Flags().GetBool("verbose")
}

// settingsCmd is the parent command for adapter settings management.
var settingsCmd = &cobra.Command{
	Use:   "settings <type>",
	Short: "Manage adapter settings",
	Long: `Manage protocol adapter settings on the DittoFS server.

The adapter type (nfs or smb) must be specified as the first argument.

Examples:
  # Show NFS adapter settings
  dfsctl adapter settings nfs show

  # Update NFS lease time
  dfsctl adapter settings nfs update --lease-time 120

  # Reset all NFS settings to defaults
  dfsctl adapter settings nfs reset

  # Reset a specific NFS setting
  dfsctl adapter settings nfs reset --setting lease_time`,
	Args: cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		syncGlobalFlags(cmd)
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

// settingsNFSCmd groups settings subcommands for NFS adapter.
var settingsNFSCmd = &cobra.Command{
	Use:   "nfs",
	Short: "Manage NFS adapter settings",
	Long:  `Manage NFS protocol adapter settings.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		settingsAdapterType = "nfs"
		syncGlobalFlags(cmd)
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

// settingsSMBCmd groups settings subcommands for SMB adapter.
var settingsSMBCmd = &cobra.Command{
	Use:   "smb",
	Short: "Manage SMB adapter settings",
	Long:  `Manage SMB protocol adapter settings.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		settingsAdapterType = "smb"
		syncGlobalFlags(cmd)
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

// settingsShowCmd shows current adapter settings.
var settingsShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current adapter settings",
	Long: `Show current adapter settings with defaults comparison.

Non-default values are marked with '*'.

Examples:
  # Show NFS settings
  dfsctl adapter settings nfs show

  # Show SMB settings as JSON
  dfsctl adapter settings smb show -o json`,
	RunE: runSettingsShow,
}

// settingsUpdateCmd updates adapter settings.
var settingsUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update adapter settings",
	Long: `Update adapter settings with partial changes.

Only specified flags are included in the update. Unspecified settings are not changed.

Examples:
  # Update NFS lease time
  dfsctl adapter settings nfs update --lease-time 120

  # Validate without applying
  dfsctl adapter settings nfs update --lease-time 120 --dry-run

  # Bypass range validation
  dfsctl adapter settings nfs update --lease-time 999 --force`,
	RunE: runSettingsUpdate,
}

// settingsResetCmd resets adapter settings to defaults.
var settingsResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset adapter settings to defaults",
	Long: `Reset adapter settings to their default values.

If --setting is specified, only that setting is reset. Otherwise, all settings
are reset to defaults.

Examples:
  # Reset all NFS settings
  dfsctl adapter settings nfs reset

  # Reset only lease_time
  dfsctl adapter settings nfs reset --setting lease_time`,
	RunE: runSettingsReset,
}

// Cloned show/update/reset for SMB so both nfs and smb subcommand trees exist.
var settingsShowCmdSMB = &cobra.Command{
	Use:   "show",
	Short: "Show current adapter settings",
	Long:  settingsShowCmd.Long,
	RunE:  runSettingsShow,
}

var settingsUpdateCmdSMB = &cobra.Command{
	Use:   "update",
	Short: "Update adapter settings",
	Long:  settingsUpdateCmd.Long,
	RunE:  runSettingsUpdate,
}

var settingsResetCmdSMB = &cobra.Command{
	Use:   "reset",
	Short: "Reset adapter settings to defaults",
	Long:  settingsResetCmd.Long,
	RunE:  runSettingsReset,
}

var (
	// NFS update flags
	settingsLeaseTime               int
	settingsGracePeriod             int
	settingsDelegationRecallTimeout int
	settingsCallbackTimeout         int
	settingsLeaseBreakTimeout       int
	settingsMaxConnections          int
	settingsMaxClients              int
	settingsMaxCompoundOps          int
	settingsMaxReadSize             int
	settingsMaxWriteSize            int
	settingsPreferredTransferSize   int
	settingsMinVersion              string
	settingsMaxVersion              string
	settingsDelegationsEnabled      bool
	settingsBlockedOperations       string

	// SMB update flags
	settingsMinDialect         string
	settingsMaxDialect         string
	settingsSessionTimeout     int
	settingsOplockBreakTimeout int
	settingsMaxSessions        int
	settingsEnableEncryption   bool

	// Common flags
	settingsDryRun       bool
	settingsForce        bool
	settingsResetSetting string
	settingsResetForce   bool

	// SMB-specific flags (separate to avoid cobra duplicate flag errors)
	smbSettingsDryRun       bool
	smbSettingsForce        bool
	smbSettingsResetSetting string
	smbSettingsResetForce   bool
)

func init() {
	// NFS subcommands
	settingsNFSCmd.AddCommand(settingsShowCmd)
	settingsNFSCmd.AddCommand(settingsUpdateCmd)
	settingsNFSCmd.AddCommand(settingsResetCmd)

	// SMB subcommands
	settingsSMBCmd.AddCommand(settingsShowCmdSMB)
	settingsSMBCmd.AddCommand(settingsUpdateCmdSMB)
	settingsSMBCmd.AddCommand(settingsResetCmdSMB)

	// Register both adapter types under settings
	settingsCmd.AddCommand(settingsNFSCmd)
	settingsCmd.AddCommand(settingsSMBCmd)

	// NFS update flags
	registerNFSUpdateFlags(settingsUpdateCmd)

	// SMB update flags
	registerSMBUpdateFlags(settingsUpdateCmdSMB)

	// Reset flags
	settingsResetCmd.Flags().StringVar(&settingsResetSetting, "setting", "", "Reset a specific setting (omit to reset all)")
	settingsResetCmd.Flags().BoolVarP(&settingsResetForce, "force", "f", false, "Skip confirmation prompt")

	settingsResetCmdSMB.Flags().StringVar(&smbSettingsResetSetting, "setting", "", "Reset a specific setting (omit to reset all)")
	settingsResetCmdSMB.Flags().BoolVarP(&smbSettingsResetForce, "force", "f", false, "Skip confirmation prompt")
}

func registerNFSUpdateFlags(cmd *cobra.Command) {
	cmd.Flags().IntVar(&settingsLeaseTime, "lease-time", 0, "NFSv4 lease time in seconds")
	cmd.Flags().IntVar(&settingsGracePeriod, "grace-period", 0, "NFSv4 grace period in seconds")
	cmd.Flags().IntVar(&settingsDelegationRecallTimeout, "delegation-recall-timeout", 0, "Delegation recall timeout in seconds")
	cmd.Flags().IntVar(&settingsCallbackTimeout, "callback-timeout", 0, "Callback timeout in seconds")
	cmd.Flags().IntVar(&settingsLeaseBreakTimeout, "lease-break-timeout", 0, "Lease break timeout in seconds")
	cmd.Flags().IntVar(&settingsMaxConnections, "max-connections", 0, "Maximum concurrent connections")
	cmd.Flags().IntVar(&settingsMaxClients, "max-clients", 0, "Maximum concurrent clients")
	cmd.Flags().IntVar(&settingsMaxCompoundOps, "max-compound-ops", 0, "Maximum compound operations per request")
	cmd.Flags().IntVar(&settingsMaxReadSize, "max-read-size", 0, "Maximum read size in bytes")
	cmd.Flags().IntVar(&settingsMaxWriteSize, "max-write-size", 0, "Maximum write size in bytes")
	cmd.Flags().IntVar(&settingsPreferredTransferSize, "preferred-transfer-size", 0, "Preferred transfer size in bytes")
	cmd.Flags().StringVar(&settingsMinVersion, "min-version", "", "Minimum NFS version (e.g., 3)")
	cmd.Flags().StringVar(&settingsMaxVersion, "max-version", "", "Maximum NFS version (e.g., 4.1)")
	cmd.Flags().BoolVar(&settingsDelegationsEnabled, "delegations-enabled", false, "Enable NFSv4 delegations")
	cmd.Flags().StringVar(&settingsBlockedOperations, "blocked-operations", "", "Comma-separated list of blocked operations")
	cmd.Flags().BoolVar(&settingsDryRun, "dry-run", false, "Validate without applying changes")
	cmd.Flags().BoolVar(&settingsForce, "force", false, "Bypass range validation")
}

func registerSMBUpdateFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&settingsMinDialect, "min-dialect", "", "Minimum SMB dialect")
	cmd.Flags().StringVar(&settingsMaxDialect, "max-dialect", "", "Maximum SMB dialect")
	cmd.Flags().IntVar(&settingsSessionTimeout, "session-timeout", 0, "SMB session timeout in seconds")
	cmd.Flags().IntVar(&settingsOplockBreakTimeout, "oplock-break-timeout", 0, "Oplock break timeout in seconds")
	cmd.Flags().IntVar(&settingsMaxConnections, "max-connections", 0, "Maximum concurrent connections")
	cmd.Flags().IntVar(&settingsMaxSessions, "max-sessions", 0, "Maximum concurrent SMB sessions")
	cmd.Flags().BoolVar(&settingsEnableEncryption, "enable-encryption", false, "Enable SMB encryption")
	cmd.Flags().StringVar(&settingsBlockedOperations, "blocked-operations", "", "Comma-separated list of blocked operations")
	cmd.Flags().BoolVar(&smbSettingsDryRun, "dry-run", false, "Validate without applying changes")
	cmd.Flags().BoolVar(&smbSettingsForce, "force", false, "Bypass range validation")
}

func runSettingsShow(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch settingsAdapterType {
	case "nfs":
		return showNFSSettings(client, format)
	case "smb":
		return showSMBSettings(client, format)
	default:
		return fmt.Errorf("unsupported adapter type: %s (valid: nfs, smb)", settingsAdapterType)
	}
}

func showNFSSettings(client *apiclient.Client, format output.Format) error {
	settings, err := client.GetNFSSettings()
	if err != nil {
		return fmt.Errorf("failed to get NFS settings: %w", err)
	}

	defaults, err := client.GetNFSSettingsDefaults()
	if err != nil {
		return fmt.Errorf("failed to get NFS settings defaults: %w", err)
	}

	if format != output.FormatTable {
		return cmdutil.PrintResource(os.Stdout, settings, nil)
	}

	// Config-style grouped output
	d := defaults.Defaults
	printSettingsGroup("Version Negotiation", []settingRow{
		newSettingRow("min_version", settings.MinVersion, d.MinVersion),
		newSettingRow("max_version", settings.MaxVersion, d.MaxVersion),
	})
	printSettingsGroup("Timeouts", []settingRow{
		newSettingRowInt("lease_time", settings.LeaseTime, d.LeaseTime, "s"),
		newSettingRowInt("grace_period", settings.GracePeriod, d.GracePeriod, "s"),
		newSettingRowInt("delegation_recall_timeout", settings.DelegationRecallTimeout, d.DelegationRecallTimeout, "s"),
		newSettingRowInt("callback_timeout", settings.CallbackTimeout, d.CallbackTimeout, "s"),
		newSettingRowInt("lease_break_timeout", settings.LeaseBreakTimeout, d.LeaseBreakTimeout, "s"),
	})
	printSettingsGroup("Connection Limits", []settingRow{
		newSettingRowInt("max_connections", settings.MaxConnections, d.MaxConnections, ""),
		newSettingRowInt("max_clients", settings.MaxClients, d.MaxClients, ""),
	})
	printSettingsGroup("Transport", []settingRow{
		newSettingRowInt("max_compound_ops", settings.MaxCompoundOps, d.MaxCompoundOps, ""),
		newSettingRowInt("max_read_size", settings.MaxReadSize, d.MaxReadSize, ""),
		newSettingRowInt("max_write_size", settings.MaxWriteSize, d.MaxWriteSize, ""),
		newSettingRowInt("preferred_transfer_size", settings.PreferredTransferSize, d.PreferredTransferSize, ""),
	})
	printSettingsGroup("Delegation", []settingRow{
		newSettingRowBool("delegations_enabled", settings.DelegationsEnabled, d.DelegationsEnabled),
	})
	printSettingsGroup("Operations", []settingRow{
		newSettingRowStrSlice("blocked_operations", settings.BlockedOperations, d.BlockedOperations),
	})

	return nil
}

func showSMBSettings(client *apiclient.Client, format output.Format) error {
	settings, err := client.GetSMBSettings()
	if err != nil {
		return fmt.Errorf("failed to get SMB settings: %w", err)
	}

	defaults, err := client.GetSMBSettingsDefaults()
	if err != nil {
		return fmt.Errorf("failed to get SMB settings defaults: %w", err)
	}

	if format != output.FormatTable {
		return cmdutil.PrintResource(os.Stdout, settings, nil)
	}

	d := defaults.Defaults
	printSettingsGroup("Version Negotiation", []settingRow{
		newSettingRow("min_dialect", settings.MinDialect, d.MinDialect),
		newSettingRow("max_dialect", settings.MaxDialect, d.MaxDialect),
	})
	printSettingsGroup("Timeouts", []settingRow{
		newSettingRowInt("session_timeout", settings.SessionTimeout, d.SessionTimeout, "s"),
		newSettingRowInt("oplock_break_timeout", settings.OplockBreakTimeout, d.OplockBreakTimeout, "s"),
	})
	printSettingsGroup("Connection Limits", []settingRow{
		newSettingRowInt("max_connections", settings.MaxConnections, d.MaxConnections, ""),
		newSettingRowInt("max_sessions", settings.MaxSessions, d.MaxSessions, ""),
	})
	printSettingsGroup("Security", []settingRow{
		newSettingRowBool("enable_encryption", settings.EnableEncryption, d.EnableEncryption),
	})
	printSettingsGroup("Operations", []settingRow{
		newSettingRowStrSlice("blocked_operations", settings.BlockedOperations, d.BlockedOperations),
	})

	return nil
}

// settingRow represents a single setting for display.
type settingRow struct {
	name       string
	value      string
	isDefault  bool
	defaultStr string
}

func newSettingRow(name, current, defaultVal string) settingRow {
	return settingRow{
		name:       name,
		value:      current,
		isDefault:  current == defaultVal,
		defaultStr: defaultVal,
	}
}

func newSettingRowInt(name string, current, defaultVal int, suffix string) settingRow {
	valStr := fmt.Sprintf("%d%s", current, suffix)
	defStr := fmt.Sprintf("%d%s", defaultVal, suffix)
	return settingRow{
		name:       name,
		value:      valStr,
		isDefault:  current == defaultVal,
		defaultStr: defStr,
	}
}

func newSettingRowBool(name string, current, defaultVal bool) settingRow {
	return settingRow{
		name:       name,
		value:      fmt.Sprintf("%v", current),
		isDefault:  current == defaultVal,
		defaultStr: fmt.Sprintf("%v", defaultVal),
	}
}

func newSettingRowStrSlice(name string, current, defaultVal []string) settingRow {
	curStr := strings.Join(current, ", ")
	if len(current) == 0 {
		curStr = "(none)"
	}
	defStr := strings.Join(defaultVal, ", ")
	if len(defaultVal) == 0 {
		defStr = "(none)"
	}
	return settingRow{
		name:       name,
		value:      curStr,
		isDefault:  curStr == defStr,
		defaultStr: defStr,
	}
}

func printSettingsGroup(groupName string, rows []settingRow) {
	fmt.Printf("\n# %s\n", groupName)
	for _, r := range rows {
		marker := " "
		suffix := ""
		if !r.isDefault {
			marker = "*"
			suffix = fmt.Sprintf("  (default: %s)", r.defaultStr)
		}
		fmt.Printf("%s %-30s = %s%s\n", marker, r.name, r.value, suffix)
	}
}

func runSettingsUpdate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	switch settingsAdapterType {
	case "nfs":
		return updateNFSSettings(cmd, client)
	case "smb":
		return updateSMBSettings(cmd, client)
	default:
		return fmt.Errorf("unsupported adapter type: %s (valid: nfs, smb)", settingsAdapterType)
	}
}

func updateNFSSettings(cmd *cobra.Command, client *apiclient.Client) error {
	// Build options
	var opts []apiclient.SettingsOption
	if settingsDryRun {
		opts = append(opts, apiclient.WithDryRun())
	}
	if settingsForce {
		opts = append(opts, apiclient.WithForce())
	}

	req := &apiclient.PatchNFSSettingsRequest{}
	hasChanges := false

	if cmd.Flags().Changed("lease-time") {
		req.LeaseTime = &settingsLeaseTime
		hasChanges = true
	}
	if cmd.Flags().Changed("grace-period") {
		req.GracePeriod = &settingsGracePeriod
		hasChanges = true
	}
	if cmd.Flags().Changed("delegation-recall-timeout") {
		req.DelegationRecallTimeout = &settingsDelegationRecallTimeout
		hasChanges = true
	}
	if cmd.Flags().Changed("callback-timeout") {
		req.CallbackTimeout = &settingsCallbackTimeout
		hasChanges = true
	}
	if cmd.Flags().Changed("lease-break-timeout") {
		req.LeaseBreakTimeout = &settingsLeaseBreakTimeout
		hasChanges = true
	}
	if cmd.Flags().Changed("max-connections") {
		req.MaxConnections = &settingsMaxConnections
		hasChanges = true
	}
	if cmd.Flags().Changed("max-clients") {
		req.MaxClients = &settingsMaxClients
		hasChanges = true
	}
	if cmd.Flags().Changed("max-compound-ops") {
		req.MaxCompoundOps = &settingsMaxCompoundOps
		hasChanges = true
	}
	if cmd.Flags().Changed("max-read-size") {
		req.MaxReadSize = &settingsMaxReadSize
		hasChanges = true
	}
	if cmd.Flags().Changed("max-write-size") {
		req.MaxWriteSize = &settingsMaxWriteSize
		hasChanges = true
	}
	if cmd.Flags().Changed("preferred-transfer-size") {
		req.PreferredTransferSize = &settingsPreferredTransferSize
		hasChanges = true
	}
	if cmd.Flags().Changed("min-version") {
		req.MinVersion = &settingsMinVersion
		hasChanges = true
	}
	if cmd.Flags().Changed("max-version") {
		req.MaxVersion = &settingsMaxVersion
		hasChanges = true
	}
	if cmd.Flags().Changed("delegations-enabled") {
		req.DelegationsEnabled = &settingsDelegationsEnabled
		hasChanges = true
	}
	if cmd.Flags().Changed("blocked-operations") {
		ops := cmdutil.ParseCommaSeparatedList(settingsBlockedOperations)
		req.BlockedOperations = &ops
		hasChanges = true
	}

	if !hasChanges {
		return fmt.Errorf("no settings specified. Use flags like --lease-time, --grace-period, etc.")
	}

	result, err := client.PatchNFSSettings(req, opts...)
	if err != nil {
		return fmt.Errorf("failed to update NFS settings: %w", err)
	}

	if settingsDryRun {
		fmt.Println("Dry run: settings validated successfully (not applied)")
		fmt.Println()
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, result, "NFS adapter settings updated successfully")
}

func updateSMBSettings(cmd *cobra.Command, client *apiclient.Client) error {
	// Build options
	var opts []apiclient.SettingsOption
	if smbSettingsDryRun {
		opts = append(opts, apiclient.WithDryRun())
	}
	if smbSettingsForce {
		opts = append(opts, apiclient.WithForce())
	}

	req := &apiclient.PatchSMBSettingsRequest{}
	hasChanges := false

	if cmd.Flags().Changed("min-dialect") {
		req.MinDialect = &settingsMinDialect
		hasChanges = true
	}
	if cmd.Flags().Changed("max-dialect") {
		req.MaxDialect = &settingsMaxDialect
		hasChanges = true
	}
	if cmd.Flags().Changed("session-timeout") {
		req.SessionTimeout = &settingsSessionTimeout
		hasChanges = true
	}
	if cmd.Flags().Changed("oplock-break-timeout") {
		req.OplockBreakTimeout = &settingsOplockBreakTimeout
		hasChanges = true
	}
	if cmd.Flags().Changed("max-connections") {
		req.MaxConnections = &settingsMaxConnections
		hasChanges = true
	}
	if cmd.Flags().Changed("max-sessions") {
		req.MaxSessions = &settingsMaxSessions
		hasChanges = true
	}
	if cmd.Flags().Changed("enable-encryption") {
		req.EnableEncryption = &settingsEnableEncryption
		hasChanges = true
	}
	if cmd.Flags().Changed("blocked-operations") {
		ops := cmdutil.ParseCommaSeparatedList(settingsBlockedOperations)
		req.BlockedOperations = &ops
		hasChanges = true
	}

	if !hasChanges {
		return fmt.Errorf("no settings specified. Use flags like --min-dialect, --session-timeout, etc.")
	}

	result, err := client.PatchSMBSettings(req, opts...)
	if err != nil {
		return fmt.Errorf("failed to update SMB settings: %w", err)
	}

	if smbSettingsDryRun {
		fmt.Println("Dry run: settings validated successfully (not applied)")
		fmt.Println()
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, result, "SMB adapter settings updated successfully")
}

func runSettingsReset(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Get the right setting/force flags based on adapter type
	resetSetting := settingsResetSetting
	resetForce := settingsResetForce
	if settingsAdapterType == "smb" {
		resetSetting = smbSettingsResetSetting
		resetForce = smbSettingsResetForce
	}

	// Confirmation
	resetWhat := "all settings"
	if resetSetting != "" {
		resetWhat = fmt.Sprintf("setting '%s'", resetSetting)
	}

	confirmed, err := prompt.ConfirmWithForce(
		fmt.Sprintf("Reset %s %s to defaults?", settingsAdapterType, resetWhat),
		resetForce,
	)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if !confirmed {
		fmt.Println("Aborted.")
		return nil
	}

	if err := client.ResetAdapterSettings(settingsAdapterType, resetSetting); err != nil {
		return fmt.Errorf("failed to reset %s settings: %w", settingsAdapterType, err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("%s adapter %s reset to defaults", strings.ToUpper(settingsAdapterType), resetWhat))
	return nil
}
