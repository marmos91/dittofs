// Command dfsbench is the DittoFS-vs-competitors benchmark harness.
//
// It wires a fio load-generator driver, a per-cell result schema with crash-safe
// resume, and a comparison-table report, plus a competitor backend registry,
// protocol re-export, and SCW cloud orchestration (see issue #1602). The command
// tree is assembled here from the packages under internal/dfsbench/.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/internal/dfsbench/backend"
	"github.com/marmos91/dittofs/internal/dfsbench/cloud"
	"github.com/marmos91/dittofs/internal/dfsbench/report"
	"github.com/marmos91/dittofs/internal/dfsbench/run"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dfsbench",
		Short:         "DittoFS benchmark harness — fio across DittoFS and competitors (#1602)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(run.NewRunCmd(), report.NewReportCmd(), backend.NewListCmd(), cloud.NewSetupCmd(), cloud.NewTeardownCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
