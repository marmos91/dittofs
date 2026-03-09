package bench

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/bench"
	"github.com/spf13/cobra"
)

var compareCmd = &cobra.Command{
	Use:   "compare FILE [FILE...]",
	Short: "Compare benchmark results from multiple systems",
	Long: `Load two or more JSON result files and render a side-by-side comparison table.

Examples:
  # Compare DittoFS vs kernel NFS
  dfsctl bench compare results/dittofs.json results/kernel-nfs.json

  # Compare all results
  dfsctl bench compare results/*.json`,
	Args: cobra.MinimumNArgs(2),
	RunE: runCompare,
}

func runCompare(cmd *cobra.Command, args []string) error {
	results := make([]*bench.Result, 0, len(args))

	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		var r bench.Result
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		results = append(results, &r)
	}

	return cmdutil.PrintResource(os.Stdout, results, CompareTable{Results: results})
}
