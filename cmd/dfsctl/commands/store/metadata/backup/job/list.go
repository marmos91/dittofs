package job

import "github.com/spf13/cobra"

// listCmd placeholder — fully implemented in Task 3 of plan 06-05.
var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup/restore job attempts",
	Args:  cobra.ExactArgs(1),
	RunE:  runList,
}

func runList(_ *cobra.Command, _ []string) error { return nil }
