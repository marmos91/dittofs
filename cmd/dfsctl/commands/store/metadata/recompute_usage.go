package metadata

import (
	"fmt"
	"os"
	"strconv"

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

--dry-run answers "are these numbers actually wrong" without repairing
anything. It derives the same figures from the file rows, writes nothing, and
names every usage bucket whose counter disagrees with them, with both numbers.
The repair replaces the counters, so running it to find out destroys the
evidence of what was wrong.

A dry run against a store that is taking writes reports small transient deltas:
the file rows and the counters are read at different instants, so a write in
between shows up as a difference. A drift bug does not look like that — it
persists across runs and does not track live traffic.

Examples:
  dfsctl store metadata recompute-usage myshare --dry-run
  dfsctl store metadata recompute-usage myshare
  dfsctl store metadata recompute-usage myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runRecomputeUsage,
}

// recomputeUsageDryRun reports drift without repairing it.
var recomputeUsageDryRun bool

func init() {
	recomputeUsageCmd.Flags().BoolVar(&recomputeUsageDryRun, "dry-run", false,
		"Report which usage counters disagree with the file rows, and repair nothing")
}

func runRecomputeUsage(_ *cobra.Command, args []string) error {
	share := args[0]
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	res, err := client.RecomputeShareUsage(share, recomputeUsageDryRun)
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
	if r.DryRun {
		return printRecomputeUsageDrift(r)
	}
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

// printRecomputeUsageDrift renders what a dry run found. Every disagreeing
// bucket is named with both numbers: telling an operator only that something
// drifted leaves them no option but the repair, which is the destructive path
// they asked this question to avoid.
func printRecomputeUsageDrift(r *apiclient.ShareUsageRecompute) error {
	out := os.Stdout
	if len(r.Drift) == 0 {
		fmt.Fprintf(out, "No drift: every usage counter agrees with the file rows (scanned in %dms).\n", r.DurationMS)
		fmt.Fprintln(out, "Nothing to repair. The counters were not modified.")
		return nil
	}

	fmt.Fprintf(out, "%d usage counter(s) disagree with the file rows (scanned in %dms).\n",
		len(r.Drift), r.DurationMS)
	fmt.Fprintln(out, "The counters were NOT modified. Re-run without --dry-run to repair them.")
	fmt.Fprintln(out)

	table := output.NewTableData("SHARE", "SCOPE", "ID", "COUNTER (bytes/files)", "ROWS (bytes/files)", "DIFF")
	for _, d := range r.Drift {
		table.AddRow(
			d.Share,
			d.Scope,
			strconv.FormatUint(uint64(d.ID), 10),
			fmt.Sprintf("%s / %d", bytesize.ByteSize(d.Counter.Bytes), d.Counter.Files),
			fmt.Sprintf("%s / %d", bytesize.ByteSize(d.Derived.Bytes), d.Derived.Files),
			fmt.Sprintf("%s / %+d", signedBytes(d.Derived.Bytes-d.Counter.Bytes), d.Derived.Files-d.Counter.Files),
		)
	}
	if err := output.PrintTable(out, table); err != nil {
		return err
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "A store taking writes reports small transient deltas here: the file rows and")
	fmt.Fprintln(out, "the counters are read at different instants. Drift from a bug persists across")
	fmt.Fprintln(out, "runs and does not track live traffic.")
	return nil
}

// signedBytes renders a byte difference with its direction. ByteSize is
// unsigned, so a shortfall is formatted from its magnitude.
func signedBytes(n int64) string {
	if n < 0 {
		return "-" + bytesize.ByteSize(-n).String()
	}
	return "+" + bytesize.ByteSize(n).String()
}
