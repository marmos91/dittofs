// Package commands implements the CLI commands for dfsctl client.
package commands

import (
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	adaptercmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/adapter"
	clientcmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/client"
	ctxcmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/context"
	gracecmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/grace"
	groupcmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/group"
	idmapcmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/idmap"
	netgroupcmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/netgroup"
	settingscmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/settings"
	sharecmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/share"
	storecmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/store"
	usercmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/user"
	"github.com/spf13/cobra"
)

var (
	// Version information injected at build time.
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "dfsctl",
	Short: "DittoFS Control - Remote management client",
	Long: `dfsctl is the command-line client for managing DittoFS servers remotely.

Use this tool to manage users, groups, shares, stores, and server settings
through the DittoFS REST API.

Use "dfsctl [command] --help" for more information about a command.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Sync flags to cmdutil.Flags for subcommands
		cmdutil.Flags.ServerURL, _ = cmd.Flags().GetString("server")
		cmdutil.Flags.Token, _ = cmd.Flags().GetString("token")
		cmdutil.Flags.Output, _ = cmd.Flags().GetString("output")
		cmdutil.Flags.NoColor, _ = cmd.Flags().GetBool("no-color")
		cmdutil.Flags.Verbose, _ = cmd.Flags().GetBool("verbose")
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

// GetRootCmd returns the root command for testing purposes.
func GetRootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	// Global persistent flags
	rootCmd.PersistentFlags().String("server", "", "Server URL (overrides stored credential)")
	rootCmd.PersistentFlags().String("token", "", "Bearer token (overrides stored credential)")
	rootCmd.PersistentFlags().StringP("output", "o", "table", "Output format (table|json|yaml)")
	rootCmd.PersistentFlags().Bool("no-color", false, "Disable colored output")
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)
	rootCmd.AddCommand(ctxcmd.Cmd)
	rootCmd.AddCommand(usercmd.Cmd)
	rootCmd.AddCommand(groupcmd.Cmd)
	rootCmd.AddCommand(sharecmd.Cmd)
	rootCmd.AddCommand(storecmd.Cmd)
	rootCmd.AddCommand(adaptercmd.Cmd)
	rootCmd.AddCommand(clientcmd.Cmd)
	rootCmd.AddCommand(gracecmd.Cmd)
	rootCmd.AddCommand(netgroupcmd.Cmd)
	rootCmd.AddCommand(idmapcmd.Cmd)
	rootCmd.AddCommand(settingscmd.Cmd)
	rootCmd.AddCommand(completionCmd)

	// Hide the default completion command (we provide our own)
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

// PrintErr prints an error message to stderr.
func PrintErr(format string, args ...any) {
	rootCmd.PrintErrf(format+"\n", args...)
}

// Exit prints an error and exits with code 1.
func Exit(format string, args ...any) {
	PrintErr(format, args...)
	os.Exit(1)
}
