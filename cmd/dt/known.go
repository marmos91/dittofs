package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/known"
	"github.com/marmos91/dittofs/test/harness/report"
)

func newKnown() *cobra.Command {
	cmd := &cobra.Command{Use: "known", Short: "Work on the suites' known files"}
	cmd.AddCommand(newKnownImport(), newKnownBaseline())
	return cmd
}

func newKnownImport() *cobra.Command {
	var where string
	cmd := &cobra.Command{
		Use:   "import FILE.md KNOWN.yaml",
		Short: "Convert a Markdown known-failure list into a known file, once",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var w map[string][]string
			if where != "" {
				axis, list, ok := strings.Cut(where, "=")
				values := strings.Split(list, ",")
				for i := range values {
					values[i] = strings.TrimSpace(values[i])
				}
				if !ok || axis == "" || slices.Contains(values, "") {
					return fmt.Errorf("--where %q: expected AXIS=V or AXIS=V1,V2", where)
				}
				w = map[string][]string{axis: values}
			}
			left, err := known.Import(args[0], args[1], w)
			for _, name := range left {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "left out %q: not in a table of tests\n", name)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&where, "where", "", "AXIS=V,...: where the list's entries apply")
	return cmd
}

// baselineFile is smbtorture's baseline file, the only suite's.
const baselineFile = "test/smb-conformance/smbtorture/baseline-results.md"

func newKnownBaseline() *cobra.Command {
	return &cobra.Command{
		Use:   "baseline SUITE/CELL [RUN]",
		Short: "Write a cell's results, the latest run's by default, as its baseline file (smbtorture's)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			if !strings.HasPrefix(args[0], "smbtorture/") {
				return fmt.Errorf("%s: only smbtorture has a baseline file", args[0])
			}
			root, err := repoRoot()
			if err != nil {
				return err
			}
			dir, err := runDir(resultsDir(root, ""), first(args[1:]))
			if err != nil {
				return err
			}
			data, err := report.Baseline(dir, args[0])
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, baselineFile), data, 0o644)
		},
	}
}
