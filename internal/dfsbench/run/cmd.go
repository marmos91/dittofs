package run

import (
	"github.com/spf13/cobra"
)

// runFlags are the `run` command's flags; they override any --config values.
type runFlags struct {
	config     string
	local      bool
	smoke      bool
	target     string
	systems    []string
	workloads  []string
	sizes      []string
	results    string
	threads    int
	runtime    int
	engine     string
	fioBin     string
	resume     bool
	dryRun     bool
	evictCache bool
	remote     bool
}

// NewRunCmd builds the `run` subcommand, which drives fio across the workload ×
// size matrix and writes one JSON result per cell.
func NewRunCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run fio workloads against a mounted filesystem and record results",
		Long: `Run drives fio across a workload × size matrix and writes one JSON result
per cell under --results, then prints a comparison table.

Modes:
  --local --target PATH   fio an already-mounted filesystem you supply
  --smoke                 self-contained tiny matrix on a temp dir (CI, secret-free)
  --systems A,B,...       managed: the harness sets up/mounts each backend over
                          its protocols, runs a warm then cold (post-evict) pass,
                          and tears it down (needs Linux + knfsd/Samba/mount)

See registered backends with 'dfsbench list'. fio must be installed and on PATH.
Add --remote to run the managed matrix on a provisioned VM (see 'dfsbench setup').`,
		Example: `  dfsbench run --local --target /mnt/dittofs
  dfsbench run --smoke
  dfsbench run --systems local-disk,dittofs-s3 --sizes large
  dfsbench run --systems dittofs-s3-nfs3 --workloads seq-read --resume`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBench(cmd.Context(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", "", "dfsbench YAML config (CLI flags override)")
	fl.BoolVar(&f.local, "local", false, "benchmark an already-mounted FS at --target")
	fl.BoolVar(&f.smoke, "smoke", false, "self-contained tiny run on a temp dir (CI)")
	fl.StringVar(&f.target, "target", "", "mounted path to benchmark (with --local)")
	fl.StringSliceVar(&f.systems, "systems", nil, "system labels (default: one, from mode)")
	fl.StringSliceVar(&f.workloads, "workloads", nil, "workloads to run (default: all)")
	fl.StringSliceVar(&f.sizes, "sizes", nil, "sizes: small|medium|large or explicit (default: medium)")
	fl.StringVar(&f.results, "results", "./bench-results", "results directory")
	fl.IntVar(&f.threads, "threads", 0, "fio numjobs (default 4)")
	fl.IntVar(&f.runtime, "runtime", 0, "fio runtime seconds (default 60; smoke uses 3)")
	fl.StringVar(&f.engine, "fio-engine", "", "fio ioengine (default libaio on Linux, psync elsewhere)")
	fl.StringVar(&f.fioBin, "fio-bin", "", "fio binary (default: fio on PATH)")
	fl.BoolVar(&f.resume, "resume", false, "skip cells whose result JSON already exists")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the cell matrix and exit")
	fl.BoolVar(&f.evictCache, "evict-cache", true, "run a cold (post-evict) read pass in managed mode")
	fl.BoolVar(&f.remote, "remote", false, "drive the managed run on the provisioned VM from .bench-vm.json (needs `dfsbench setup`)")
	return cmd
}
