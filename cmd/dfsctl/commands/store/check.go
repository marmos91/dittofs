package store

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// checkIncludeHoles adds the uncovered ranges no file claims to the detail
// table. They are always counted in the summary; they are off by default in
// the detail because a sparse file legitimately has them.
var checkIncludeHoles bool

// checkRepair turns the scan into a repair run, and checkRepairYes skips the
// confirmation. Without --yes the run is a dry run that ends at the prompt.
var (
	checkRepair    bool
	checkRepairYes bool
)

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

--repair adds a second half: for every finding, work out whether the store
holds enough evidence to put the manifest back, list what that would take, and
apply it once you confirm. Nothing is written before the prompt, so --repair on
its own is a dry run; --yes answers the prompt for scripted use. Only two kinds
of finding are repairable, and both restore a row the file already claims:

  * a row with no parseable offset whose hash and length match exactly one
    range the file claims and no row covers — the row is moved to that offset,
    keeping everything else about it
  * a range the file claims that no row covers, whose hash the synced-hash
    store resolves — a row is written for it, and the remote already holds the
    bytes it names

A repair only ever adds coverage the file already asked for. It never drops a
row, never widens or narrows a claim, and never marks a hash synced. Findings
it cannot pair with that evidence — a row matching no claim, a claim whose hash
nothing resolves, a hash the synced-hash store does not know — are reported and
left alone: those need the bytes re-synced, not the metadata rewritten.

With no argument every share is scanned. The command exits non-zero when
damage is found, so it can be scripted (` + "`dfsctl store check || alert`" + `).

Examples:
  dfsctl store check
  dfsctl store check myshare
  dfsctl store check myshare --include-holes
  dfsctl store check myshare -o json
  dfsctl store check myshare --repair
  dfsctl store check myshare --repair --yes`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStoreCheck,
}

func init() {
	checkCmd.Flags().BoolVar(&checkIncludeHoles, "include-holes", false,
		"list uncovered ranges that no file claims (legitimate for sparse files)")
	checkCmd.Flags().BoolVar(&checkRepair, "repair", false,
		"list the findings the store can put back, and apply them once confirmed")
	checkCmd.Flags().BoolVar(&checkRepairYes, "yes", false,
		"Skip confirmation prompt")
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

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	// With --yes there is nothing to confirm, so the first pass is already
	// the repair pass. Without it the first pass only plans, and the prompt
	// decides whether a second one writes.
	opts := apiclient.BlockStoreManifestCheckOptions{
		PlanRepairs:  checkRepair,
		ApplyRepairs: checkRepair && checkRepairYes,
	}
	results, err := scanShares(client, shareNames, opts)
	if err != nil {
		return err
	}
	if err := printCheckResults(results, format); err != nil {
		return err
	}

	if checkRepair {
		applied := opts.ApplyRepairs
		if applied {
			if format == output.FormatTable {
				printRepairOutcome(results)
			}
		} else {
			applied, err = confirmAndRepair(client, shareNames, format, results)
			if err != nil {
				return err
			}
		}
		if applied {
			// Re-scan read-only so the verdict below describes the store as
			// it stands now, not as the repair pass found it.
			if results, err = scanShares(client, shareNames, apiclient.BlockStoreManifestCheckOptions{}); err != nil {
				return err
			}
			if format == output.FormatTable {
				fmt.Println()
				fmt.Println("Store as it stands after the repair:")
			}
			if err := printCheckResults(results, format); err != nil {
				return err
			}
		}
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

// scanShares runs one scan per share with the same options.
func scanShares(
	client *apiclient.Client,
	shareNames []string,
	opts apiclient.BlockStoreManifestCheckOptions,
) ([]*engine.ManifestCheckResult, error) {
	results := make([]*engine.ManifestCheckResult, 0, len(shareNames))
	for _, name := range shareNames {
		res, err := client.BlockStoreCheckManifests(name, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to check share %q: %w", name, err)
		}
		if res == nil || res.Result == nil {
			return nil, fmt.Errorf("store check for share %q returned no result", name)
		}
		results = append(results, res.Result)
	}
	return results, nil
}

func printCheckResults(results []*engine.ManifestCheckResult, format output.Format) error {
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, results)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, results)
	default:
		return printCheckTables(results)
	}
}

// confirmAndRepair prompts for the plan the dry run just printed and, on a
// yes, runs the scan again with the writes enabled. Reports whether it wrote.
func confirmAndRepair(
	client *apiclient.Client,
	shareNames []string,
	format output.Format,
	planned []*engine.ManifestCheckResult,
) (bool, error) {
	var total uint64
	for _, r := range planned {
		total += r.RepairsPlanned
	}
	if format != output.FormatTable {
		// Neither a prompt nor a note may land in the middle of the
		// machine-readable document the caller just received.
		if total == 0 {
			return false, nil
		}
		return false, fmt.Errorf("nothing written: %d repair(s) planned, re-run with --yes to apply them", total)
	}

	if total == 0 {
		fmt.Println()
		fmt.Println("Nothing to repair: no finding carries the evidence needed to put a row back.")
		return false, nil
	}

	prompt := fmt.Sprintf("Apply %d manifest repair(s)? This writes to the metadata store.", total)
	ok, err := cmdutil.ConfirmDestructive(prompt, false)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Println("Aborted. Nothing was written.")
		return false, nil
	}

	applied, err := scanShares(client, shareNames, apiclient.BlockStoreManifestCheckOptions{
		PlanRepairs:  true,
		ApplyRepairs: true,
	})
	if err != nil {
		return false, err
	}
	printRepairOutcome(applied)
	return true, nil
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
			// Say which of the two suppressing conditions applied: nothing
			// to check, or a check that could not be run.
			reason := r.SyncedCheckSkipped
			if reason == "" {
				reason = "reason not recorded"
			}
			unknown = "not checked (" + reason + ")"
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
		if r.RepairsPlanned > 0 {
			pairs = append(pairs, [2]string{"Repairs", repairSummary(r)})
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		if err := printCheckDetail(r); err != nil {
			return err
		}
		if err := printRepairPlan(r); err != nil {
			return err
		}
	}
	return nil
}

// printCheckDetail lists the affected payloads, one row per payload, with the
// findings joined into a single cell so a wide store still reads as a table.
func printCheckDetail(r *engine.ManifestCheckResult) error {
	table := output.NewTableData("PATH", "SIZE", "FINDINGS")
	var listed bool
	for i := range r.Findings {
		f := &r.Findings[i]
		notes := checkFindingNotes(f)
		if len(notes) == 0 {
			continue
		}
		listed = true
		table.AddRow(f.Path, fmt.Sprintf("%d", f.Size), strings.Join(notes, "; "))
	}

	if !listed {
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
	if f.Truncated {
		notes = append(notes, "(more, truncated)")
	}
	if len(notes) == 0 && f.Damaged {
		// The payload is damaged but every listed range is an unclaimed
		// hole that --include-holes would suppress. Never drop it.
		notes = append(notes, "damaged; detail truncated")
	}
	return notes
}

// printRepairPlan lists the writes a repair run would make for one share, so
// the operator sees every row before answering the prompt.
func printRepairPlan(r *engine.ManifestCheckResult) error {
	if len(r.Repairs) == 0 || repairRunWrote(r) {
		// Nothing planned, or the run already wrote — printRepairOutcome
		// reports what a run that wrote actually did.
		return nil
	}
	table := output.NewTableData("PATH", "ACTION", "RANGE", "ROW")
	for i := range r.Repairs {
		a := &r.Repairs[i]
		table.AddRow(a.Path, repairActionLabel(a.Kind),
			fmt.Sprintf("[%d,%d)", a.Offset, a.Offset+uint64(a.Size)),
			repairRowNote(a))
	}
	fmt.Println()
	if err := output.PrintTable(os.Stdout, table); err != nil {
		return err
	}
	if r.RepairsTruncated {
		fmt.Println()
		fmt.Println("Repair plan truncated; re-run after applying these to see the rest.")
	}
	return nil
}

// repairSummary renders one share's repair counters, reading as a plan before
// the writes and as an outcome after them.
func repairSummary(r *engine.ManifestCheckResult) string {
	if !repairRunWrote(r) {
		return fmt.Sprintf("%d planned", r.RepairsPlanned)
	}
	return fmt.Sprintf("%d planned, %d applied, %d skipped",
		r.RepairsPlanned, r.RepairsApplied, r.RepairsSkipped)
}

// repairRunWrote reports whether this result came from a pass that wrote,
// as opposed to one that only planned. Every action of a pass that wrote is
// either applied or skipped, so the two counters are zero only before it ran.
func repairRunWrote(r *engine.ManifestCheckResult) bool {
	return r.RepairsApplied+r.RepairsSkipped > 0
}

// repairActionLabel names a repair kind in the words the help text uses.
func repairActionLabel(k engine.RepairKind) string {
	switch k {
	case engine.RepairReplaceRow:
		return "move row to offset"
	case engine.RepairRecreateRow:
		return "write row for claim"
	default:
		return string(k)
	}
}

// repairRowNote renders the manifest keys one action touches.
func repairRowNote(a *engine.RepairAction) string {
	if a.FromRowID != "" {
		return a.FromRowID + " -> " + a.ToRowID
	}
	return a.ToRowID
}

// printRepairOutcome reports what a repair pass wrote, naming every action it
// declined so a skipped repair is never mistaken for a done one.
func printRepairOutcome(results []*engine.ManifestCheckResult) {
	var applied, skipped uint64
	for _, r := range results {
		applied += r.RepairsApplied
		skipped += r.RepairsSkipped
	}
	fmt.Println()
	fmt.Printf("Applied %d repair(s), skipped %d.\n", applied, skipped)
	if skipped == 0 {
		return
	}
	fmt.Println("A skipped repair is one whose evidence changed between the scan and the write;")
	fmt.Println("the store moved under it and nothing was forced. Re-run to pick it up again.")
	for _, r := range results {
		for i := range r.Repairs {
			a := &r.Repairs[i]
			if a.Applied || a.SkipReason == "" {
				continue
			}
			fmt.Printf("  %s [%d,%d): %s\n", a.Path, a.Offset, a.Offset+uint64(a.Size), a.SkipReason)
		}
	}
}
