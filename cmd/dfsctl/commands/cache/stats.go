package cache

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show cache statistics",
	Long: `Display block store cache statistics.

Without --share, shows aggregated totals across all shares with a per-share breakdown.
With --share, shows statistics for a single share only.

Examples:
  # Show aggregated cache stats
  dfsctl cache stats

  # Show stats for a specific share
  dfsctl cache stats --share /export

  # Output as JSON
  dfsctl cache stats -o json`,
	RunE: runCacheStats,
}

func init() {
	statsCmd.Flags().String("share", "", "Show stats for a specific share only")
}

func runCacheStats(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shareName, _ := cmd.Flags().GetString("share")

	var resp *apiclient.CacheStatsResponse
	if shareName != "" {
		resp, err = client.CacheStatsForShare(shareName)
	} else {
		resp, err = client.CacheStatsAll()
	}
	if err != nil {
		return fmt.Errorf("failed to get cache stats: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, resp)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, resp)
	default:
		return printCacheStatsTable(resp)
	}
}

func printCacheStatsTable(resp *apiclient.CacheStatsResponse) error {
	// Print totals summary
	t := resp.Totals
	pairs := [][2]string{
		{"Files", fmt.Sprintf("%d", t.FileCount)},
		{"Blocks Total", fmt.Sprintf("%d", t.BlocksTotal)},
		{"Blocks Dirty", fmt.Sprintf("%d", t.BlocksDirty)},
		{"Blocks Local", fmt.Sprintf("%d", t.BlocksLocal)},
		{"Blocks Remote", fmt.Sprintf("%d", t.BlocksRemote)},
		{"Local Disk Used", formatBytes(t.LocalDiskUsed)},
		{"Local Disk Max", formatBytes(t.LocalDiskMax)},
		{"Local Mem Used", formatBytes(t.LocalMemUsed)},
		{"Local Mem Max", formatBytes(t.LocalMemMax)},
		{"L1 Entries", fmt.Sprintf("%d", t.L1Entries)},
		{"L1 Used", formatBytes(t.L1CurBytes)},
		{"L1 Max", formatBytes(t.L1MaxBytes)},
		{"Has Remote", fmt.Sprintf("%v", t.HasRemote)},
		{"Pending Syncs", fmt.Sprintf("%d", t.PendingSyncs)},
		{"Pending Uploads", fmt.Sprintf("%d", t.PendingUploads)},
		{"Completed Syncs", fmt.Sprintf("%d", t.CompletedSyncs)},
		{"Failed Syncs", fmt.Sprintf("%d", t.FailedSyncs)},
	}

	if err := output.SimpleTable(os.Stdout, pairs); err != nil {
		return err
	}

	// Print per-share breakdown if multiple shares
	if len(resp.PerShare) > 1 {
		fmt.Println()
		fmt.Println("Per-Share Breakdown:")
		table := output.NewTableData(
			"SHARE", "BLOCKS", "DIRTY", "LOCAL", "REMOTE",
			"DISK USED", "L1 ENTRIES", "PENDING",
		)
		for _, s := range resp.PerShare {
			table.AddRow(
				s.ShareName,
				fmt.Sprintf("%d", s.Stats.BlocksTotal),
				fmt.Sprintf("%d", s.Stats.BlocksDirty),
				fmt.Sprintf("%d", s.Stats.BlocksLocal),
				fmt.Sprintf("%d", s.Stats.BlocksRemote),
				formatBytes(s.Stats.LocalDiskUsed),
				fmt.Sprintf("%d", s.Stats.L1Entries),
				fmt.Sprintf("%d", s.Stats.PendingSyncs),
			)
		}
		return output.PrintTable(os.Stdout, table)
	}

	return nil
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b == 0:
		return "0"
	case b < KB:
		return fmt.Sprintf("%d B", b)
	case b < MB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	case b < GB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	}
}
