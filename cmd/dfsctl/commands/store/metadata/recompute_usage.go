package metadata

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// recomputeUsageCmd rebuilds a metadata store's used-bytes counters from its
// file rows and reports what the named share held before and after.
var recomputeUsageCmd = &cobra.Command{
	Use:   "recompute-usage <share>",
	Short: "Rebuild a share's used-bytes counter from its files",
	Long: `Rebuild the used-bytes counters of the metadata store backing the named share.

The counters are maintained transactionally as files are written and removed, so
they are normally already correct. This repairs a store where they are not: a
share carrying bytes it no longer holds reports itself fuller than it is, and
because that figure is what the share quota is checked against, it can refuse
writes to a share that is actually empty.

The rebuild scans every file row in the store, so it takes time in proportion to
the store's size, and it repairs every share that store serves — not only the
one named here. Nothing runs it automatically; a per-file walk on every server
start is a cost every share would pay forever to fix a number that is almost
always already right.

Examples:
  dfsctl store metadata recompute-usage myshare
  dfsctl store metadata recompute-usage myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runRecomputeUsage,
}

func runRecomputeUsage(_ *cobra.Command, args []string) error {
	share := args[0]
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	res, err := client.RecomputeShareUsage(share)
	if err != nil {
		return fmt.Errorf("failed to recompute usage: %w", err)
	}
	if res == nil || res.Result == nil {
		return fmt.Errorf("recompute usage: server returned empty response")
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, res)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, res)
	default:
		return printRecomputeUsageTable(res)
	}
}

// printRecomputeUsageTable renders the repair summary as a key/value table.
// The reclaimed row is what an operator is here for: how much of the share's
// reported usage was not backed by any file.
func printRecomputeUsageTable(res *apiclient.UsageRecomputeResult) error {
	r := res.Result
	// The rebuild normally only ever removes bytes the share does not hold, but
	// a write landing during it can leave the share genuinely larger than
	// before. ByteSize is unsigned, so render that as growth rather than
	// wrapping the subtraction.
	moved := "0 B"
	label := "Reclaimed"
	if r.BeforeBytes > r.AfterBytes {
		moved = bytesize.ByteSize(r.BeforeBytes - r.AfterBytes).String()
	} else if r.AfterBytes > r.BeforeBytes {
		label = "Added"
		moved = bytesize.ByteSize(r.AfterBytes - r.BeforeBytes).String()
	}
	pairs := [][2]string{
		{"Share", r.ShareName},
		{"Used before", bytesize.ByteSize(r.BeforeBytes).String()},
		{"Used after", bytesize.ByteSize(r.AfterBytes).String()},
		{label, moved},
		{"Duration", fmt.Sprintf("%dms", r.DurationMS)},
	}
	return output.SimpleTable(os.Stdout, pairs)
}
