package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/run"
)

// exitStatus is an error that carries a run's exit status to main.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newRun() *cobra.Command {
	var tier, results, place string
	var only, skip []string
	var ci bool
	var retry int
	options := map[string]bool{}
	cmd := &cobra.Command{
		Use:   "run SUITE|GROUP[/CELL] [--OPTION] [TEST...] | --place P",
		Short: "Run suites and write their results and logs; exit 0 when all passed, 1 when one failed, 2 when one didn't run",
		Args: func(cmd *cobra.Command, args []string) error {
			if place != "" {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			if place != "" {
				cells, err := placeCells(root, r, tier, place)
				if err != nil {
					return err
				}
				return runCells(cmd, root, cells, run.Options{Root: root, Results: resultsDir(root, results), CI: ci, Out: cmd.OutOrStdout(), Tier: tier, Retry: retry, Place: place})
			}
			var option []string
			cmd.Flags().Visit(func(f *pflag.Flag) {
				if options[f.Name] && f.Value.String() == "true" {
					option = append(option, f.Name)
				}
			})
			if len(option) > 1 {
				return fmt.Errorf("one option at a time, not --%s and --%s", option[0], option[1])
			}
			cells, err := run.Select(root, r, args[0], first(option), tier, only, skip, args[1:])
			if err != nil {
				return err
			}
			return runCells(cmd, root, cells, run.Options{Root: root, Results: resultsDir(root, results), CI: ci, Out: cmd.OutOrStdout(), Tier: tier, Retry: retry})
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "pr", "run at tier T, or at a suite's own tier when wider: it sets args, env and select")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these suites of the group")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "skip these suites of the group")
	cmd.Flags().BoolVar(&ci, "ci", false, "an unmet need fails the run instead of skipping the cell, and new failures are annotated")
	cmd.Flags().IntVar(&retry, "retry", 0, "run each test that failed new again, up to N times: a pass on a retry is flaky")
	cmd.Flags().StringVar(&results, "results", "", "where results go, relative to the checkout's root (default test/harness/results)")
	cmd.Flags().StringVar(&place, "place", "", "run the cells of the tier that dt plan gives place P, as the dedicated host does; that place's host need is met")
	// Each suite's options are flags: dt run smbtorture --kerberos.
	if _, r, err := load(); err == nil {
		for _, s := range r.Suites {
			for _, o := range s.Options {
				if cmd.Flags().Lookup(o.Name) == nil {
					cmd.Flags().Bool(o.Name, false, "run the suite's "+o.Name+" option")
					options[o.Name] = true
				}
			}
		}
	}
	return cmd
}

// runCells runs the cells, prints their outcomes and where the results went,
// and returns the run's exit status.
func runCells(cmd *cobra.Command, root string, cells []run.Cell, o run.Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := run.Run(ctx, cells, o)
	if rep.Dir == "" {
		return err // the run couldn't start
	}
	rows := [][]string{{"CELL", "RESULT", "TIME", "NOTE"}}
	for _, o := range rep.Outcomes {
		rows = append(rows, []string{o.Name, o.Result, o.Took.String(), o.Note})
	}
	if err := table(cmd.OutOrStdout(), rows); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "results:", rep.Dir)
	if err != nil {
		return err // after the outcomes: summary.json couldn't be written
	}
	if s := rep.Status(); s != 0 {
		return exitStatus(s)
	}
	return nil
}

// placeCells selects the cells of tier that dt plan gives place. An option's
// cells are named SUITE/OPTION/..., which Select takes as the suite, the
// option apart, and the rest.
func placeCells(root string, r *registry.Registry, tier, place string) ([]run.Cell, error) {
	planned, err := plan.Plan(root, r, tier, nil)
	if err != nil {
		return nil, err
	}
	var cells []run.Cell
	for _, c := range planned {
		if c.Place != place {
			continue
		}
		target := c.Name
		if c.Option != "" {
			target = c.Suite + strings.TrimPrefix(c.Name, c.Suite+"/"+c.Option)
		}
		selected, err := run.Select(root, r, target, c.Option, tier, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, s := range selected {
			if s.Name == c.Name {
				cells = append(cells, s)
			}
		}
	}
	if len(cells) == 0 {
		return nil, errors.New("tier " + tier + " plans no cell on place " + place)
	}
	return cells, nil
}

// resultsDir is where runs go: results, relative to the checkout's root, else
// test/harness/results there.
func resultsDir(root, results string) string {
	if results == "" {
		results = filepath.Join("test", "harness", "results")
	}
	if !filepath.IsAbs(results) {
		results = filepath.Join(root, results)
	}
	return results
}

// alias is a command people use today, `dt NAME ARGS`, which is
// `dt run NAME ARGS`.
func alias(name string) *cobra.Command {
	return &cobra.Command{
		Use:                name + " [ARGS...]",
		Short:              "dt run " + name,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			root.SetArgs(append([]string{"run", name}, args...))
			return root.Execute()
		},
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
