// Package commands implements the CLI commands for dittofs server management.
package commands

import (
	"os"

	"github.com/marmos91/dittofs/cmd/dittofs/commands/backup"
	"github.com/marmos91/dittofs/cmd/dittofs/commands/config"
	"github.com/marmos91/dittofs/cmd/dittofs/commands/restore"
	"github.com/spf13/cobra"
)

var (
	// Version information injected at build time.
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"

	// Global flags.
	cfgFile string
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "dittofs",
	Short: "DittoFS - Modular virtual filesystem",
	Long: `DittoFS is an experimental modular virtual filesystem that decouples
file interfaces from storage backends. It implements NFSv3 and SMB protocols
in pure Go (userspace, no FUSE required) with pluggable metadata and content stores.

Use "dittofs [command] --help" for more information about a command.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() error {
	return rootCmd.Execute()
}

// GetRootCmd returns the root command for testing purposes.
func GetRootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	// Global persistent flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(config.Cmd)
	rootCmd.AddCommand(backup.Cmd)
	rootCmd.AddCommand(restore.Cmd)
	rootCmd.AddCommand(completionCmd)

	// Hide the default completion command (we provide our own)
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

// GetConfigFile returns the config file path from the global flag.
func GetConfigFile() string {
	return cfgFile
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
