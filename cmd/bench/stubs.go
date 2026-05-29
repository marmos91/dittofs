package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newStubCmd builds a placeholder subcommand for an area whose
// workloads have not been wired yet. It prints a one-line notice
// pointing at bench/<area>/README.md and exits 0 so CI scripts can
// invoke `dfsbench <area>` unconditionally during the migration.
func newStubCmd(area, short string) *cobra.Command {
	return &cobra.Command{
		Use:   area,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"%s benchmarks: not yet implemented. See bench/%s/README.md\n",
				area, area,
			)
			return nil
		},
	}
}

var (
	gcCmd        = newStubCmd("gc", "Garbage collection benchmarks (stub)")
	snapshotsCmd = newStubCmd("snapshots", "Reference-CAS snapshot benchmarks (stub)")
	metadataCmd  = newStubCmd("metadata", "Metadata store benchmarks (listings, rename, links, ACL) (stub)")
	adaptersCmd  = newStubCmd("adapters", "NFS / SMB framing benchmarks (stub)")
	e2eCmd       = newStubCmd("e2e", "External-client end-to-end benchmarks (fio, iozone, smbtorture-perf) (stub)")
)
