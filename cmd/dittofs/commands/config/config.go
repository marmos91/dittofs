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

Use 'dittofs init' to create a new configuration file.

Subcommands:
  edit      Open configuration in editor
  validate  Validate configuration file
  show      Display current configuration
  schema    Generate JSON schema for IDE/validation`,
}

func init() {
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(validateCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(schemaCmd)
}
