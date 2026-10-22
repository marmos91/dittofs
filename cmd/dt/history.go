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
	"github.com/marmos91/dittofs/test/harness/plan"
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
			// Each cell the suite and its options plan, by name, with its
			// unit and where it runs, but for the tier and OS of the run it
			// ran in. A cell no longer planned tells nothing.
			type planned struct {
				unit *registry.Suite
				at   map[string]string
			}
			cells := map[string]planned{}
			var files []string
			for _, u := range r.Units(s) {
				if u.Known != "" && !slices.Contains(files, u.Known) {
					files = append(files, u.Known)
				}
				list, err := plan.UnitCells(root, u, "manual", true)
				if err != nil {
					return err
				}
				for _, c := range list {
					at := map[string]string{"option": c.Option}
					maps.Copy(at, c.Matrix)
					cells[c.Name] = planned{u, at}
				}
			}
			if len(files) == 0 {
				return fmt.Errorf("%s has no known file", s.Name)
			}
			// Each known file's results: those of the cells it grades, each
			// where its cell ran, as dt run grades them: at the run's tier,
			// or the unit's own when wider, and on the run's OS, named as
			// the platform needs name it.
			results := map[string][]known.Result{}
			for _, run := range runs {
				for _, c := range run.Cells {
					p, ok := cells[c.Name]
					if !ok || p.unit.Known == "" {
						continue
					}
					at := maps.Clone(p.at)
					at["tier"], at["os"] = run.Tier, run.OS
					if !plan.RunsIn(p.unit.Tier, run.Tier) {
						at["tier"] = p.unit.Tier
					}
					if run.OS == "darwin" {
						at["os"] = "macos"
					}
					for _, t := range c.Tests {
						results[p.unit.Known] = append(results[p.unit.Known], known.Result{Name: t.Name, Outcome: history.Outcome(t), At: at})
					}
				}
			}
			out := cmd.OutOrStdout()
			for _, file := range files {
				if len(results[file]) == 0 {
					_, _ = fmt.Fprintf(out, "%s: left as is: no cell it grades has results in the runs read\n", file)
					continue
				}
				removed, err := known.Prune(filepath.Join(root, file), results[file])
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
