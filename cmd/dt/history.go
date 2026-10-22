package main

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/history"
	"github.com/marmos91/dittofs/test/harness/known"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// lastRuns is how many past runs dt history and dt known prune read unless
// --last says otherwise.
//
// decision: 10 runs, about a day of develop's. Raise it when flake rates or
// prune need a longer window by default.
const lastRuns = 10

// historyFlags are the flags of the commands that read past runs: --ci and
// --last.
func historyFlags(cmd *cobra.Command, ci *bool, last *int) {
	cmd.Flags().BoolVar(ci, "ci", false, "read the last N develop runs of tests.yml, from their artifacts through the GitHub API, with the token in GITHUB_TOKEN, instead of this checkout's runs")
	cmd.Flags().IntVar(last, "last", lastRuns, "read the last N runs")
}

// pastRuns are the runs dt history reads, oldest first: this checkout's last
// ones, or with ci develop's, each with the cells of its jobs' first attempt.
// With suite, only develop's artifacts of its cells are read.
func pastRuns(ctx context.Context, suite string, ci bool, last int) ([]report.Run, error) {
	if last < 1 {
		return nil, fmt.Errorf("--last %d: expected 1 or more", last)
	}
	if ci {
		return history.CI(ctx, suite, last)
	}
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	return history.Local(resultsDir(root, ""), last)
}

func newHistory() *cobra.Command {
	var ci bool
	var last int
	cmd := &cobra.Command{
		Use:   "history [SUITE[/TEST]]",
		Short: "Pass, fail, skip and flaky counts over past runs, with p50 and p90 durations: per suite, per test of SUITE, and each result of TEST",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			suite, test, _ := strings.Cut(first(args), "/")
			runs, err := pastRuns(cmd.Context(), suite, ci, last)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			counts, what := history.Suites(runs), "SUITE"
			if suite != "" {
				counts, what = history.Tests(runs, suite, test), "TEST"
			}
			if len(counts) == 0 {
				of := ""
				if len(args) > 0 {
					of = " of " + args[0]
				}
				_, err := fmt.Fprintf(out, "no results%s in the runs read\n", of)
				return err
			}
			rows := [][]string{{what, "RUNS", "PASS", "FAIL", "SKIP", "FLAKY", "P50", "P90"}}
			for _, key := range slices.Sorted(maps.Keys(counts)) {
				n := counts[key]
				rows = append(rows, []string{key, strconv.Itoa(n.Runs), strconv.Itoa(n.Pass), strconv.Itoa(n.Fail), strconv.Itoa(n.Skip), strconv.Itoa(n.Flaky), duration(n.P50()), duration(n.P90())})
			}
			if test != "" {
				// Each result, oldest first, says since when it fails.
				rows = append(rows, nil, []string{"RUN", "CELL", "TEST", "RESULT", "TIME"})
				for _, r := range runs {
					for _, c := range r.Cells {
						for _, t := range c.Tests {
							if history.Suite(c) == suite && t.Name == test {
								rows = append(rows, []string{r.Name, c.Name, t.Title(), result(t), duration(t.Duration)})
							}
						}
					}
				}
			}
			return table(out, rows)
		},
	}
	historyFlags(cmd, &ci, &last)
	return cmd
}

// result is a result's outcome, with its grade or that it was flaky.
func result(t report.Test) string {
	switch {
	case t.Flaky:
		return history.Outcome(t) + ", flaky"
	case t.Grade != "":
		return history.Outcome(t) + " (" + t.Grade + ")"
	}
	return history.Outcome(t)
}

func duration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

func newKnownPrune() *cobra.Command {
	var ci bool
	var last int
	cmd := &cobra.Command{
		Use:   "prune SUITE",
		Short: "Remove the known entries that matched no failing or skipping test in the runs dt history reads",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			s := r.Suite(args[0])
			if s == nil {
				return fmt.Errorf("no suite %q", args[0])
			}
			runs, err := pastRuns(cmd.Context(), s.Name, ci, last)
			if err != nil {
				return err
			}
			// Each known file of the suite and its options, with the results
			// of the cells it grades.
			units := r.Units(s)
			var files []string
			results := map[string][]report.Test{}
			for _, u := range units {
				if u.Known != "" && !slices.Contains(files, u.Known) {
					files = append(files, u.Known)
				}
			}
			if len(files) == 0 {
				return fmt.Errorf("%s has no known file", s.Name)
			}
			for _, run := range runs {
				for _, c := range run.Cells {
					if u := unitOf(units, c.Name); u != nil && u.Known != "" {
						results[u.Known] = append(results[u.Known], c.Tests...)
					}
				}
			}
			out := cmd.OutOrStdout()
			for _, file := range files {
				tests := results[file]
				if len(tests) == 0 {
					_, _ = fmt.Fprintf(out, "%s: left as is: no cell it grades has results in the runs read\n", file)
					continue
				}
				// decision: an entry is kept when its test matched a result of
				// the same kind, a failure or a skip, in any cell the file
				// grades, whatever its where. That keeps every entry a run
				// needed, and maybe one whose where no longer applies
				// anywhere, as a dropped matrix value leaves. Judge each
				// entry in the cells it applies to when such entries
				// outlive prune.
				removed, err := known.Prune(filepath.Join(root, file), func(e known.Entry) bool {
					want := map[string]string{"fail": report.Failed, "skip": report.Skipped}[e.Status]
					return slices.ContainsFunc(tests, func(t report.Test) bool {
						return history.Outcome(t) == want && known.Match(e.Test, t.Name)
					})
				})
				if err != nil {
					return err
				}
				if len(removed) == 0 {
					_, _ = fmt.Fprintf(out, "%s: nothing to remove\n", file)
				}
				for _, e := range removed {
					_, _ = fmt.Fprintf(out, "%s:%d: removed %s, a %s entry\n", file, e.Line, e.Test, e.Status)
				}
			}
			return nil
		},
	}
	historyFlags(cmd, &ci, &last)
	return cmd
}

// unitOf is the suite or option of units whose cell is named cell, or nil
// when the cell is another suite's. The options come first, since the names
// of their cells begin with the suite's too.
func unitOf(units []*registry.Suite, cell string) *registry.Suite {
	for _, u := range slices.Backward(units) {
		if cell == u.Name || strings.HasPrefix(cell, u.Name+"/") {
			return u
		}
	}
	return nil
}
