// Package config implements configuration management subcommands.
package config

import (
	"github.com/spf13/cobra"
)

// Cmd is the config subcommand.
var Cmd = &cobra.Command{
	Use:   "config",
	Short: "Configuration management",
	Long: `Manage DittoFS configuration files.

Subcommands:
  init      Initialize a new configuration file
  edit      Open configuration in editor
  validate  Validate configuration file
  show      Display current configuration`,
}

func init() {
	Cmd.AddCommand(initCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(validateCmd)
	Cmd.AddCommand(showCmd)
}
