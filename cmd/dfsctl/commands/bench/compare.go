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
		r, err := loadResult(path)
		if err != nil {
			return err
		}
		results = append(results, r)
	}

	return cmdutil.PrintResource(os.Stdout, results, CompareTable{Results: results})
}

// loadResult streams and decodes a single benchmark result file, avoiding
// buffering the raw JSON bytes alongside the decoded struct.
func loadResult(path string) (*bench.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var r bench.Result
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &r, nil
}
