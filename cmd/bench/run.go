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

func newRunCmd() *cobra.Command {
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
SCW provisioning + resume land in a follow-up PR (see issue #1602).`,
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
	fl.BoolVar(&f.remote, "remote", false, "drive the run on the SCW VM from .bench-vm.json (needs `dfsbench setup`)")
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

	// Remote: orchestrate the run on the provisioned VM instead of here.
	if f.remote {
		return runRemote(ctx, f)
	}

	if f.smoke && f.local {
		return fmt.Errorf("--smoke and --local are mutually exclusive")
	}
	opts := LoadOpts{
		Threads: f.threads,
		Runtime: f.runtime,
		Engine:  f.engine,
		FioBin:  f.fioBin,
	}

	// Managed mode: the harness itself sets up/mounts each --systems backend
	// over its protocols. Needs Linux (knfsd/Samba/mount); the resolution and
	// matrix logic is unit-tested, the recipes are exercised on the VM.
	if !f.smoke && !f.local {
		if len(f.systems) == 0 {
			return fmt.Errorf("choose a mode: --local --target PATH, --smoke, or --systems <backend>...")
		}
		return runManaged(ctx, f, opts, cfg)
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

// runManaged drives the backend-provisioning path: resolve --systems into
// (backend, protocol) plans, then set up / mount / fio / evict / tear down each.
func runManaged(ctx context.Context, f *runFlags, opts LoadOpts, cfg Config) error {
	plans, err := resolveSystems(f.systems)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return fmt.Errorf("no runnable (backend, protocol) plans from --systems %v", f.systems)
	}
	wls := f.workloads
	if len(wls) == 0 {
		wls = knownWorkloads
	}
	for _, w := range wls {
		if !validWorkload(w) {
			return fmt.Errorf("unknown workload %q (see `dfsbench list`)", w)
		}
	}
	sizes := f.sizes
	if len(sizes) == 0 {
		sizes = []string{"medium"}
	}

	cells := managedMatrix(plans, wls, sizes, f.evictCache)
	if f.dryRun {
		printMatrix(cells)
		return nil
	}
	want := make(map[string]bool, len(cells))
	for _, c := range cells {
		want[(CellResult{System: c.system, Workload: c.workload, Size: c.size, Protocol: c.protocol, Pass: c.pass}).slug()] = true
	}

	env := BackendEnv{Bucket: cfg.Bucket, Endpoint: cfg.Endpoint}
	for _, p := range plans {
		if err := runPlan(ctx, p, env, wls, sizes, opts, f); err != nil {
			return fmt.Errorf("%s: %w", p.systemLabel(), err)
		}
	}

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

// runPlan runs one (backend, protocol) through its full lifecycle. Setup is
// idempotent, so re-running it per protocol is wasteful but correct — matches
// the plan's "one competitor at a time, full teardown between" model.
func runPlan(ctx context.Context, p plan, env BackendEnv, wls, sizes []string, opts LoadOpts, f *runFlags) (err error) {
	b := p.backend
	if b.Setup == nil || b.Mount == nil {
		_, _ = fmt.Fprintf(cmdOut, "skip %s: no recipe yet\n", p.systemLabel())
		return nil
	}
	if err := b.Setup(ctx, env); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if b.Teardown != nil {
		defer func() {
			if e := b.Teardown(ctx); e != nil && err == nil {
				err = fmt.Errorf("teardown: %w", e)
			}
		}()
	}

	mnt, err := b.Mount(ctx, p.protocol)
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	if b.Unmount != nil {
		defer func() { _ = b.Unmount(ctx, p.protocol) }()
	}

	if err := runPass(ctx, p, "warm", wls, sizes, mnt, opts, f); err != nil {
		return err
	}
	if f.evictCache {
		if b.Evict != nil {
			if err := b.Evict(ctx); err != nil {
				return fmt.Errorf("evict: %w", err)
			}
		}
		// Universal cold step: drop the OS page cache so reads come from the
		// backend, not RAM. Best-effort — a warn beats aborting the whole run.
		if err := dropOSCache(ctx); err != nil {
			_, _ = fmt.Fprintf(cmdOut, "warn: drop OS cache: %v\n", err)
		}
		var reads []string
		for _, w := range wls {
			if readWorkloads[w] {
				reads = append(reads, w)
			}
		}
		if err := runPass(ctx, p, "cold", reads, sizes, mnt, opts, f); err != nil {
			return err
		}
	}
	return nil
}

// runPass fios one pass (warm|cold) of a plan's workload×size cells against mnt.
func runPass(ctx context.Context, p plan, pass string, wls, sizes []string, mnt string, opts LoadOpts, f *runFlags) error {
	for _, w := range wls {
		for _, s := range sizes {
			res := CellResult{System: p.systemLabel(), Workload: w, Size: s, Protocol: string(p.protocol), Pass: pass}
			if f.resume && resultExists(f.results, res.slug()) {
				_, _ = fmt.Fprintf(cmdOut, "skip (resume): %s\n", res.slug())
				continue
			}
			_, _ = fmt.Fprintf(cmdOut, "run: %s\n", res.slug())
			m, err := runFio(ctx, w, mnt, withSize(opts, s))
			if err != nil {
				return err
			}
			m.System, m.Workload, m.Size, m.Protocol, m.Pass = res.System, w, s, res.Protocol, pass
			m.Timestamp = time.Now().UTC()
			if err := m.save(f.results); err != nil {
				return err
			}
		}
	}
	return nil
}

func printMatrix(cells []cell) {
	_, _ = fmt.Fprintf(cmdOut, "matrix (%d cells):\n", len(cells))
	for _, c := range cells {
		_, _ = fmt.Fprintf(cmdOut, "  %s | %s | %s | %s | %s\n", c.system, c.workload, c.size, c.protocol, c.pass)
	}
}
