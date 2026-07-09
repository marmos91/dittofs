package backend

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
	"github.com/marmos91/dittofs/internal/dfsbench/fio"
)

// NewListCmd builds the `list` subcommand, which prints the registered
// backends, workloads, and size classes.
func NewListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available backends, workloads, and size classes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(exec.CmdOut, "backends (managed mode):")
			for _, n := range backendNames() {
				b := registry[n]
				_, _ = fmt.Fprintf(exec.CmdOut, "  %-12s %s\n", n, backendProtos(b))
			}
			_, _ = fmt.Fprintln(exec.CmdOut, "workloads:")
			for _, w := range fio.KnownWorkloads {
				_, _ = fmt.Fprintf(exec.CmdOut, "  %s\n", w)
			}
			_, _ = fmt.Fprintln(exec.CmdOut, "sizes:")
			for _, s := range fio.SizeClassOrder() {
				_, _ = fmt.Fprintf(exec.CmdOut, "  %-7s %s\n", s, fio.SizeClasses[s])
			}
			return nil
		},
	}
}
