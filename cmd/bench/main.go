// Command dfsbench is the DittoFS-vs-competitors benchmark harness.
//
// This PR wires the core: a fio load-generator driver, a per-cell result schema with
// crash-safe resume, and a comparison-table report — driven locally via
// `run --local`/`run --smoke` (no cloud). Cloud provisioning, the competitor
// backend registry, protocol re-export, and ceiling baselines land in
// follow-up PRs (see issue #1602). exec.go carries the salvaged SSH executor
// the provisioning path will drive.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// cmdOut is where commands write their human-facing output. A package var keeps
// the plumbing out of every function signature; tests point it at a buffer.
var cmdOut io.Writer = os.Stdout

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dfsbench",
		Short:         "DittoFS benchmark harness — fio across DittoFS and competitors (#1602)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newRunCmd(), newReportCmd(), newListCmd())
	return root
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available backends, workloads, and size classes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmdOut, "backends (managed mode):")
			for _, n := range backendNames() {
				b := registry[n]
				_, _ = fmt.Fprintf(cmdOut, "  %-12s %s\n", n, backendProtos(b))
			}
			_, _ = fmt.Fprintln(cmdOut, "workloads:")
			for _, w := range knownWorkloads {
				_, _ = fmt.Fprintf(cmdOut, "  %s\n", w)
			}
			_, _ = fmt.Fprintln(cmdOut, "sizes:")
			for _, s := range sizeClassOrder() {
				_, _ = fmt.Fprintf(cmdOut, "  %-7s %s\n", s, sizeClasses[s])
			}
			return nil
		},
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
