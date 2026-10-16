package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/report"
)

func newReport() *cobra.Command {
	var format, results string
	var failures bool
	cmd := &cobra.Command{
		Use:   "report [RUN]",
		Short: "Show a run, the latest by default: how each cell went, and its new failures",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(report.Formats, format) {
				return fmt.Errorf("--format %q is not one of %s", format, strings.Join(report.Formats, ", "))
			}
			root, err := repoRoot()
			if err != nil {
				return err
			}
			dir, err := runDir(resultsDir(root, results), first(args))
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
			return r.Show(cmd.OutOrStdout(), format, failures)
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "text, md or json")
	cmd.Flags().BoolVar(&failures, "failures", false, "only the new failures")
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
