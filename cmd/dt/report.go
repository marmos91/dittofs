package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/issues"
	"github.com/marmos91/dittofs/test/harness/report"
)

func newReport() *cobra.Command {
	var format, results string
	var failures, post bool
	cmd := &cobra.Command{
		Use:   "report [RUN...]",
		Short: "Show a run, the latest by default: how each cell went, and its new failures; with --post, post them",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case !slices.Contains(report.Formats, format):
				return fmt.Errorf("--format %q is not one of %s", format, strings.Join(report.Formats, ", "))
			case post && !failures:
				return errors.New("--post posts the new failures: give --failures too")
			case len(args) > 1 && !post:
				return errors.New("one run at a time: only --post takes several, the cells of one tier run")
			}
			root, err := repoRoot()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				args = []string{""} // the latest run
			}
			var runs []report.Run
			for _, run := range args {
				dir, err := runDir(resultsDir(root, results), run)
				if errors.Is(err, errNoRun) {
					// decision: a job whose dt run stopped before it started a
					// run gets a note in its summary, not a second failure. In
					// json it goes to stderr, so the output stays JSON or nothing.
					out := cmd.OutOrStdout()
					if format == "json" {
						out = cmd.ErrOrStderr()
					}
					_, _ = fmt.Fprintln(out, err)
					return nil
				}
				if err != nil {
					return err
				}
				r, err := report.Read(dir)
				if err != nil {
					return err
				}
				if err := r.Show(cmd.OutOrStdout(), format, failures); err != nil {
					return err
				}
				runs = append(runs, r)
			}
			if !post {
				return nil
			}
			c, err := issues.FromEnv()
			if err != nil {
				return err
			}
			return c.Post(cmd.Context(), runs, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "text, md or json")
	cmd.Flags().BoolVar(&failures, "failures", false, "only the new failures")
	cmd.Flags().BoolVar(&post, "post", false, "post the new failures of develop's merge or nightly runs to the develop-status issue, and give a test failing 2 runs in a row an issue of its own, with the token in GITHUB_TOKEN")
	cmd.Flags().StringVar(&results, "results", "", "where the runs are, relative to the checkout's root (default test/harness/results)")
	return cmd
}

// errNoRun is runDir's error when results holds no run.
var errNoRun = errors.New("no run")

// runDir is the directory of run: a run in results, or a path to one, as dt
// run prints it. Without run, it's the latest run in results, and errNoRun
// when there is none.
//
// decision: the latest run is the last by name, since a run's name begins
// with the time it started.
func runDir(results, run string) (string, error) {
	if run != "" {
		if filepath.Base(run) != run {
			return run, nil
		}
		return filepath.Join(results, run), nil
	}
	entries, err := os.ReadDir(results)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return !e.IsDir() })
	if len(entries) == 0 {
		return "", fmt.Errorf("%w in %s", errNoRun, results)
	}
	return filepath.Join(results, entries[len(entries)-1].Name()), nil
}
