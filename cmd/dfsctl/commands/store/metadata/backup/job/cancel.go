package job

import "github.com/spf13/cobra"

// cancelCmd placeholder — fully implemented in Task 3 of plan 06-05.
var cancelCmd = &cobra.Command{
	Use:   "cancel <job-id>",
	Short: "Cancel a running backup/restore job",
	Args:  cobra.ExactArgs(2),
	RunE:  runCancel,
}

func runCancel(_ *cobra.Command, _ []string) error { return nil }
