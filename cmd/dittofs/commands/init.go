package commands

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/spf13/cobra"
)

var initForce bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a sample configuration file",
	Long: `Initialize a sample DittoFS configuration file.

By default, the configuration file is created at $XDG_CONFIG_HOME/dittofs/config.yaml.
Use --config to specify a custom path.

Examples:
  # Initialize with default location
  dittofs init

  # Initialize with custom path
  dittofs init --config /etc/dittofs/config.yaml

  # Force overwrite existing config
  dittofs init --force`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().BoolVar(&initForce, "force", false, "Force overwrite existing config file")
}

func runInit(cmd *cobra.Command, args []string) error {
	configFile := GetConfigFile()

	var configPath string
	var err error

	if configFile != "" {
		// Use custom path
		err = config.InitConfigToPath(configFile, initForce)
		configPath = configFile
	} else {
		// Use default path
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
	fmt.Println("\nSecurity note:")
	fmt.Println("  A random JWT secret has been generated for development use.")
	fmt.Println("  For production, generate a secure secret and use an environment variable:")
	fmt.Println("    # Generates a 64-character hex string (32 bytes of entropy)")
	fmt.Printf("    export %s=$(openssl rand -hex 32)\n", api.EnvControlPlaneSecret)

	return nil
}
