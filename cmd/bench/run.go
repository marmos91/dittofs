package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// runFlags are the `run` command's flags; they override any --config values.
type runFlags struct {
	config    string
	local     bool
	smoke     bool
	target    string
	systems   []string
	workloads []string
	sizes     []string
	results   string
	threads   int
	runtime   int
	engine    string
	fioBin    string
	resume    bool
	dryRun    bool
}

func newRunCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run fio workloads against a mounted filesystem and record results",
		Long: `Run drives fio across a workload × size matrix and writes one JSON result
per cell under --results, then prints a comparison table.

This PR wires the local paths (no cloud):
  --local --target PATH   fio an already-mounted filesystem you supply
  --smoke                 self-contained tiny matrix on a temp dir (CI, secret-free)

fio must be installed and on PATH. Provisioning and competitor backends land in
follow-up PRs (see issue #1602).`,
		Example: `  dfsbench run --local --target /mnt/dittofs
  dfsbench run --local --target /mnt/juicefs --workloads rand-read-4k --sizes large
  dfsbench run --smoke
  dfsbench run --config dfsbench.yaml --resume`,
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
	return cmd
}

// cell is one unit of work in the matrix.
type cell struct {
	system   string
	workload string
	size     string // selector (class name or explicit)
	protocol string
	pass     string
	target   string // mount/target dir for this cell
}

func runBench(ctx context.Context, f *runFlags) error {
	cfg, err := loadConfig(f.config)
	if err != nil {
		return err
	}
	f.applyConfig(cfg)

	if f.smoke && f.local {
		return fmt.Errorf("--smoke and --local are mutually exclusive")
	}
	if !f.smoke && !f.local {
		return fmt.Errorf("choose a mode: --local --target PATH (or --smoke). Cloud provisioning lands in a later PR")
	}

	opts := LoadOpts{
		Threads: f.threads,
		Runtime: f.runtime,
		Engine:  f.engine,
		FioBin:  f.fioBin,
	}

	system := "local"
	if len(f.systems) > 0 {
		system = f.systems[0]
	}
	target := f.target

	if f.smoke {
		dir, err := os.MkdirTemp("", "dfsbench-smoke-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		target = dir
		system = "smoke"
		if opts.Runtime == 0 {
			opts.Runtime = 3 // keep CI fast
		}
		if len(f.workloads) == 0 {
			f.workloads = []string{"seq-write", "rand-read-4k"} // tiny but exercises both metric shapes
		}
		if len(f.sizes) == 0 {
			// medium (1 MiB) is the smallest class valid for both the 1 MiB-block
			// sequential jobs and the 4k random jobs (fio: file size ≥ block size).
			f.sizes = []string{"medium"}
		}
	}

	if target == "" {
		return fmt.Errorf("--target is required with --local")
	}
	if err := checkTarget(target); err != nil {
		return err
	}

	cells, err := f.buildMatrix(system, target)
	if err != nil {
		return err
	}
	if f.dryRun {
		printMatrix(cells)
		return nil
	}

	want := make(map[string]bool, len(cells))
	for _, c := range cells {
		res := CellResult{
			System: c.system, Workload: c.workload, Size: c.size,
			Protocol: c.protocol, Pass: c.pass,
		}
		want[res.slug()] = true
		if f.resume && resultExists(f.results, res.slug()) {
			_, _ = fmt.Fprintf(cmdOut, "skip (resume): %s\n", res.slug())
			continue
		}
		_, _ = fmt.Fprintf(cmdOut, "run: %s\n", res.slug())
		m, err := runFio(ctx, c.workload, c.target, withSize(opts, c.size))
		if err != nil {
			return err
		}
		m.System, m.Workload, m.Size, m.Protocol, m.Pass = c.system, c.workload, c.size, c.protocol, c.pass
		m.Timestamp = time.Now().UTC()
		if err := m.save(f.results); err != nil {
			return err
		}
	}

	// Render from disk so skipped (resumed) cells appear alongside fresh ones;
	// filter to this run's matrix so unrelated saved results don't leak in.
	all, err := loadResults(f.results)
	if err != nil {
		return err
	}
	rows := all[:0]
	for _, r := range all {
		if want[r.slug()] {
			rows = append(rows, r)
		}
	}
	_, _ = fmt.Fprintln(cmdOut)
	_, _ = fmt.Fprint(cmdOut, renderTable(rows))
	return nil
}

// withSize resolves the size selector to fio's --size for this cell.
func withSize(o LoadOpts, sizeSel string) LoadOpts {
	o.Size = resolveSize(sizeSel)
	return o
}

func (f *runFlags) buildMatrix(system, target string) ([]cell, error) {
	wls := f.workloads
	if len(wls) == 0 {
		wls = knownWorkloads
	}
	for _, w := range wls {
		if !validWorkload(w) {
			return nil, fmt.Errorf("unknown workload %q (see `dfsbench list`)", w)
		}
	}
	sizes := f.sizes
	if len(sizes) == 0 {
		sizes = []string{"medium"}
	}
	var cells []cell
	for _, w := range wls {
		for _, s := range sizes {
			cells = append(cells, cell{
				system: system, workload: w, size: s,
				protocol: "local", pass: "warm", target: target,
			})
		}
	}
	return cells, nil
}

// applyConfig fills unset flags from the config file (flags win).
func (f *runFlags) applyConfig(c Config) {
	if f.target == "" {
		f.target = c.Target
	}
	if f.results == "./bench-results" && c.Results != "" {
		f.results = c.Results
	}
	if len(f.systems) == 0 {
		f.systems = c.Systems
	}
	if len(f.workloads) == 0 {
		f.workloads = c.Workloads
	}
	if len(f.sizes) == 0 {
		f.sizes = c.Sizes
	}
	if f.threads == 0 {
		f.threads = c.Threads
	}
	if f.runtime == 0 {
		f.runtime = c.Runtime
	}
	if f.engine == "" {
		f.engine = c.Engine
	}
	if f.fioBin == "" {
		f.fioBin = c.FioBin
	}
}

func printMatrix(cells []cell) {
	_, _ = fmt.Fprintf(cmdOut, "matrix (%d cells):\n", len(cells))
	for _, c := range cells {
		_, _ = fmt.Fprintf(cmdOut, "  %s | %s | %s | %s | %s\n", c.system, c.workload, c.size, c.protocol, c.pass)
	}
}
