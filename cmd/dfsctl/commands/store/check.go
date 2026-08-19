package store

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// checkIncludeHoles adds the uncovered ranges no file claims to the detail
// table. They are always counted in the summary; they are off by default in
// the detail because a sparse file legitimately has them.
var checkIncludeHoles bool

var checkCmd = &cobra.Command{
	Use:   "check [share]",
	Short: "Scan manifests for ranges no chunk covers",
	Long: `Scan the store for files whose manifest does not describe the whole file.

Per payload the scan compares the byte ranges the manifest rows cover against
the file's recorded size and reports three structural defects:

  * ranges no manifest row covers
  * rows that exist but carry no parseable chunk offset, so no reader can place
    them — a read of such a range refuses rather than serving bytes
  * rows whose content hash the synced-hash store has no record of

The scan is metadata-only. No block is fetched, no file data is read and no
remote object is touched, so it costs a metadata walk regardless of how much
data the store holds and incurs no egress. It answers "how many files are
affected" without reading every file. It does NOT verify block contents —
remote fetches are already hash-verified on the read path.

An uncovered range is only reported as damage when the file's own block list
claims data lives there. A range nothing claims cannot be told apart from a
legitimate sparse hole or from bytes written but not yet rolled up into the
manifest, so it is counted separately and never fails the command. Pass
--include-holes to list those ranges too.

The unknown-hash check is skipped on a share with no remote store: nothing is
ever marked synced there, so every row would be reported.

With no argument every share is scanned. The command exits non-zero when
damage is found, so it can be scripted (` + "`dfsctl store check || alert`" + `).

Examples:
  dfsctl store check
  dfsctl store check myshare
  dfsctl store check myshare --include-holes
  dfsctl store check myshare -o json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStoreCheck,
}

func init() {
	checkCmd.Flags().BoolVar(&checkIncludeHoles, "include-holes", false,
		"list uncovered ranges that no file claims (legitimate for sparse files)")
}

func runStoreCheck(_ *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shareNames := args
	if len(shareNames) == 0 {
		shares, err := client.ListShares()
		if err != nil {
			return fmt.Errorf("failed to list shares: %w", err)
		}
		for _, s := range shares {
			shareNames = append(shareNames, s.Name)
		}
		if len(shareNames) == 0 {
			return fmt.Errorf("no shares configured")
		}
	}

	results := make([]*engine.ManifestCheckResult, 0, len(shareNames))
	for _, name := range shareNames {
		res, err := client.BlockStoreCheckManifests(name)
		if err != nil {
			return fmt.Errorf("failed to check share %q: %w", name, err)
		}
		if res == nil || res.Result == nil {
			return fmt.Errorf("store check for share %q returned no result", name)
		}
		results = append(results, res.Result)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}
	switch format {
	case output.FormatJSON:
		err = output.PrintJSON(os.Stdout, results)
	case output.FormatYAML:
		err = output.PrintYAML(os.Stdout, results)
	default:
		err = printCheckTables(results)
	}
	if err != nil {
		return err
	}

	var damaged int
	for _, r := range results {
		if r.Damaged() {
			damaged++
		}
	}
	if damaged > 0 {
		return fmt.Errorf("manifest damage found on %d of %d share(s)", damaged, len(results))
	}
	return nil
}

// printCheckTables renders a summary per share followed by the per-payload
// detail, mirroring printAuditTable's key/value shape for the summary half.
func printCheckTables(results []*engine.ManifestCheckResult) error {
	for i, r := range results {
		if i > 0 {
			fmt.Println()
		}
		unknown := fmt.Sprintf("%d", r.UnknownHashRows)
		if !r.SyncedHashesChecked {
			unknown = "not checked (share has no remote store)"
		}
		pairs := [][2]string{
			{"Share", r.Share},
			{"Duration", fmt.Sprintf("%dms", r.DurationMS)},
			{"Files scanned", fmt.Sprintf("%d", r.FilesScanned)},
			{"Damaged payloads", fmt.Sprintf("%d", r.DamagedPayloads)},
			{"Uncovered, claimed", fmt.Sprintf("%d range(s), %d bytes", r.ClaimedUncoveredRanges, r.ClaimedUncoveredBytes)},
			{"Uncovered, unclaimed", fmt.Sprintf("%d range(s), %d bytes", r.UncoveredRanges-r.ClaimedUncoveredRanges, r.UncoveredBytes-r.ClaimedUncoveredBytes)},
			{"Unplaceable rows", fmt.Sprintf("%d", r.UnplaceableRows)},
			{"Rows with unknown hash", unknown},
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		if err := printCheckDetail(r); err != nil {
			return err
		}
	}
	return nil
}

// printCheckDetail lists the affected payloads, one row per payload, with the
// findings joined into a single cell so a wide store still reads as a table.
func printCheckDetail(r *engine.ManifestCheckResult) error {
	table := output.NewTableData("PATH", "SIZE", "FINDINGS")
	var listed int
	for i := range r.Findings {
		f := &r.Findings[i]
		notes := checkFindingNotes(f)
		if len(notes) == 0 {
			continue
		}
		listed++
		table.AddRow(f.Path, fmt.Sprintf("%d", f.Size), strings.Join(notes, "; "))
	}

	if listed == 0 {
		if r.UncoveredRanges > r.ClaimedUncoveredRanges && !checkIncludeHoles {
			fmt.Println()
			fmt.Println("No damage found. Uncovered ranges no file claims were not listed; pass --include-holes to see them.")
		}
		return nil
	}

	fmt.Println()
	if err := output.PrintTable(os.Stdout, table); err != nil {
		return err
	}
	if r.FindingsTruncated {
		fmt.Println()
		fmt.Println("Detail list truncated; the counts above are complete.")
	}
	fmt.Println()
	fmt.Println("An uncovered range marked \"claimed\" is damage: the file's own block list says data")
	fmt.Println("lives there and no manifest row covers it. An unclaimed range cannot be told apart")
	fmt.Println("from a sparse hole or from bytes not yet rolled up, so it is reported, not judged.")
	return nil
}

// checkFindingNotes renders one payload's findings as short human-readable
// phrases. Returns nil when the payload has nothing worth listing, which is
// the case for a payload holding only unclaimed holes unless --include-holes
// is set.
func checkFindingNotes(f *engine.PayloadFinding) []string {
	var notes []string
	for _, rng := range f.Uncovered {
		if !rng.Claimed && !checkIncludeHoles {
			continue
		}
		kind := "hole, unclaimed"
		if rng.Claimed {
			kind = "uncovered, claimed"
		}
		notes = append(notes, fmt.Sprintf("[%d,%d) %s", rng.Start, rng.End, kind))
	}
	for _, id := range f.UnplaceableRows {
		notes = append(notes, "unplaceable row "+id)
	}
	for _, id := range f.UnknownHashRows {
		notes = append(notes, "hash unknown to the synced-hash store: row "+id)
	}
	if len(notes) > 0 && f.Truncated {
		notes = append(notes, "(more, truncated)")
	}
	return notes
}
