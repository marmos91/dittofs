package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/marmos91/dittofs/test/harness/run"
)

// exitStatus is an error that carries a run's exit status to main.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newRun() *cobra.Command {
	var tier, results string
	var only, skip []string
	var ci bool
	options := map[string]bool{}
	cmd := &cobra.Command{
		Use:   "run SUITE|GROUP[/CELL] [--OPTION] [TEST...]",
		Short: "Run suites and write their logs; exit 0 when all passed, 1 when one failed, 2 when one didn't run",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := load()
			if err != nil {
				return err
			}
			var option []string
			cmd.Flags().Visit(func(f *pflag.Flag) {
				if options[f.Name] {
					option = append(option, f.Name)
				}
			})
			if len(option) > 1 {
				return fmt.Errorf("one option at a time, not --%s and --%s", option[0], option[1])
			}
			root, _ := repoRoot()
			cells, err := run.Select(root, r, args[0], first(option), tier, only, skip, args[1:])
			if err != nil {
				return err
			}
			if results == "" {
				results = filepath.Join(root, "test", "harness", "results")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			rep, err := run.Run(ctx, cells, run.Options{Root: root, Results: results, CI: ci, Out: cmd.OutOrStdout()})
			if err != nil {
				return err
			}
			rows := [][]string{{"CELL", "RESULT", "TIME", "NOTE"}}
			for _, o := range rep.Outcomes {
				rows = append(rows, []string{o.Cell, o.Result, o.Took.String(), o.Note})
			}
			if err := table(cmd.OutOrStdout(), rows); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "results:", rep.Dir)
			if s := rep.Status(); s != 0 {
				return exitStatus(s)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "pr", "run at tier T, or at a suite's own tier when wider: it sets args, env and select")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these suites of the group")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "skip these suites of the group")
	cmd.Flags().BoolVar(&ci, "ci", false, "an unmet need fails the run instead of skipping the cell, and problems are annotated")
	cmd.Flags().StringVar(&results, "results", "", "where results go (default test/harness/results)")
	// Each suite's options are flags: dt run smbtorture --kerberos.
	if r, err := load(); err == nil {
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

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
