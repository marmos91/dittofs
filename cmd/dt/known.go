package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/test/harness/known"
)

func newKnown() *cobra.Command {
	cmd := &cobra.Command{Use: "known", Short: "Work on the suites' known files"}
	cmd.AddCommand(newKnownImport())
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
				axis, values, ok := strings.Cut(where, "=")
				if !ok || axis == "" || values == "" {
					return fmt.Errorf("--where %q: expected AXIS=V or AXIS=V1,V2", where)
				}
				w = map[string][]string{axis: strings.Split(values, ",")}
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
