// bench is the unified DittoFS benchmark orchestrator. It covers
// six areas long-term: blockstore (local/remote/syncer), gc,
// snapshots, metadata, adapters (NFS/SMB framing perf), and e2e
// (real client driving fio / iozone / smbtorture-perf). Only the
// blockstore subcommand is wired today; the rest are stubs that
// point at bench/<area>/README.md.
//
// Example: bench blockstore --workload sequential-write --ops 10000
package main

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/bench/blockstore"
	"github.com/spf13/cobra"
)

// Global flags shared by every subcommand. Defaults match the
// legacy cmd/blockstore-perf shape so existing scripts keep working.
var (
	flagProfileDir   string
	flagOutputFormat string
	flagEnvFile      string
)

var rootCmd = &cobra.Command{
	Use:   "bench",
	Short: "DittoFS benchmark orchestrator",
	Long: `bench drives the DittoFS benchmark suite across six areas:

  blockstore   local fs + remote + syncer (implemented)
  gc           garbage collection (stub)
  snapshots    reference-CAS snapshots (stub)
  metadata     listings, rename, links, ACL (stub)
  adapters     NFS / SMB framing perf (stub)
  e2e          real NFS / SMB clients driving fio, iozone, smbtorture-perf (stub)

Each subcommand owns its own flags. Global flags below apply to all areas.
The library functions powering each area live in bench/<area>/ and are
callable from both this CLI and Go Benchmark* tests.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
		// Envfile is loaded once for every subcommand so S3 creds /
		// shared knobs can be sourced uniformly. Missing files are
		// silently tolerated so the flag is safe to leave set.
		return blockstore.ParseEnvFile(flagEnvFile)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagProfileDir, "profile-dir", "_profiles", "parent directory for per-run pprof output")
	rootCmd.PersistentFlags().StringVar(&flagOutputFormat, "output-format", "text", "output format: text (today) | json (future)")
	rootCmd.PersistentFlags().StringVar(&flagEnvFile, "env-file", "", "optional KEY=VALUE file sourced before backend setup (real env wins)")

	rootCmd.AddCommand(blockstoreCmd)
	rootCmd.AddCommand(gcCmd)
	rootCmd.AddCommand(snapshotsCmd)
	rootCmd.AddCommand(metadataCmd)
	rootCmd.AddCommand(adaptersCmd)
	rootCmd.AddCommand(e2eCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(1)
	}
}
