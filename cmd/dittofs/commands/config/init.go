package config

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var (
	initConfigPath string
	initForce      bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new configuration file",
	Long: `Initialize a sample DittoFS configuration file.

By default, the configuration file is created at $XDG_CONFIG_HOME/dittofs/config.yaml.
Use --config to specify a custom path.

Examples:
  # Initialize with default location
  dittofs config init

  # Initialize with custom path
  dittofs config init --config /etc/dittofs/config.yaml

  # Force overwrite existing config
  dittofs config init --force`,
	RunE: runConfigInit,
}

func init() {
	initCmd.Flags().StringVar(&initConfigPath, "config", "", "Path to config file")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Force overwrite existing config file")
}

func runConfigInit(cmd *cobra.Command, args []string) error {
	configPath := initConfigPath
	var err error

	if configPath != "" {
		err = config.InitConfigToPath(configPath, initForce)
	} else {
		configPath, err = config.InitConfig(initForce)
	}

	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}

	fmt.Printf("Configuration file created at: %s\n", configPath)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Edit the configuration file to customize your setup")
	fmt.Println("  2. Start the server with: dittofs start")
	fmt.Printf("  3. Or specify custom config: dittofs start --config %s\n", configPath)

	return nil
}
