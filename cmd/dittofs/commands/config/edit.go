package config

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var editCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open configuration in editor",
	Long: `Open the configuration file in your default editor.

Uses the EDITOR environment variable, falling back to 'vi' if not set.

Examples:
  # Edit default config
  dittofs config edit

  # Edit specific config file
  dittofs config edit --config /etc/dittofs/config.yaml`,
	RunE: runConfigEdit,
}

func runConfigEdit(cmd *cobra.Command, args []string) error {
	// Get config path from parent's persistent flag
	configPath, _ := cmd.Flags().GetString("config")
	if configPath == "" {
		configPath = config.GetDefaultConfigPath()
	}

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("configuration file not found: %s\n\n"+
			"Create it first with:\n"+
			"  dittofs config init --config %s",
			configPath, configPath)
	}

	// Get editor from environment
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi" // Default fallback
	}

	// Open editor
	editorCmd := exec.Command(editor, configPath)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr

	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("failed to run editor: %w", err)
	}

	return nil
}
