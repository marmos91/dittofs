package job

import "github.com/spf13/cobra"

// showCmd placeholder — fully implemented in Task 3 of plan 06-05.
var showCmd = &cobra.Command{
	Use:   "show <job-id>",
	Short: "Show backup/restore job detail",
	Args:  cobra.ExactArgs(2),
	RunE:  runShow,
}

func runShow(_ *cobra.Command, _ []string) error { return nil }
