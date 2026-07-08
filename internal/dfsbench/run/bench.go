package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/backend"
	"github.com/marmos91/dittofs/internal/dfsbench/config"
	"github.com/marmos91/dittofs/internal/dfsbench/exec"
	"github.com/marmos91/dittofs/internal/dfsbench/fio"
	"github.com/marmos91/dittofs/internal/dfsbench/report"
)

func runBench(ctx context.Context, f *runFlags) error {
	cfg, err := config.LoadConfig(f.config)
	if err != nil {
		return err
	}
	f.applyConfig(cfg)

	// Remote: orchestrate the managed run on the provisioned VM instead of here.
	// --remote only makes sense for managed mode, so reject the local/smoke modes.
	if f.remote {
		if f.smoke || f.local || f.target != "" {
			return errors.New("--remote runs the managed matrix only; drop --smoke/--local/--target")
		}
		return runRemote(ctx, f)
	}

	if f.smoke && f.local {
		return fmt.Errorf("--smoke and --local are mutually exclusive")
	}
	opts := fio.LoadOpts{
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
			return errors.New("choose a mode: --local --target PATH, --smoke, or --systems <backend>")
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
	if err := fio.CheckTarget(target); err != nil {
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
		res := fio.CellResult{
			System: c.System, Workload: c.Workload, Size: c.Size,
			Protocol: c.Protocol, Pass: c.Pass,
		}
		want[res.Slug()] = true
		if f.resume && fio.ResultExists(f.results, res.Slug()) {
			_, _ = fmt.Fprintf(exec.CmdOut, "skip (resume): %s\n", res.Slug())
			continue
		}
		_, _ = fmt.Fprintf(exec.CmdOut, "run: %s\n", res.Slug())
		m, err := fio.RunFio(ctx, c.Workload, c.Target, withSize(opts, c.Size))
		if err != nil {
			return err
		}
		m.System, m.Workload, m.Size, m.Protocol, m.Pass = c.System, c.Workload, c.Size, c.Protocol, c.Pass
		m.Timestamp = time.Now().UTC()
		if err := m.Save(f.results); err != nil {
			return err
		}
	}

	// Render from disk so skipped (resumed) cells appear alongside fresh ones;
	// filter to this run's matrix so unrelated saved results don't leak in.
	all, err := fio.LoadResults(f.results)
	if err != nil {
		return err
	}
	rows := all[:0]
	for _, r := range all {
		if want[r.Slug()] {
			rows = append(rows, r)
		}
	}
	_, _ = fmt.Fprintln(exec.CmdOut)
	_, _ = fmt.Fprint(exec.CmdOut, report.RenderTable(rows))
	return nil
}

// withSize resolves the size selector to fio's --size for this cell.
func withSize(o fio.LoadOpts, sizeSel string) fio.LoadOpts {
	o.Size = fio.ResolveSize(sizeSel)
	return o
}

func (f *runFlags) buildMatrix(system, target string) ([]backend.Cell, error) {
	wls := f.workloads
	if len(wls) == 0 {
		wls = fio.KnownWorkloads
	}
	for _, w := range wls {
		if !fio.ValidWorkload(w) {
			return nil, fmt.Errorf("unknown workload %q (see `dfsbench list`)", w)
		}
	}
	sizes := f.sizes
	if len(sizes) == 0 {
		sizes = []string{"medium"}
	}
	var cells []backend.Cell
	for _, w := range wls {
		for _, s := range sizes {
			cells = append(cells, backend.Cell{
				System: system, Workload: w, Size: s,
				Protocol: "local", Pass: "warm", Target: target,
			})
		}
	}
	return cells, nil
}

// applyConfig fills unset flags from the config file (flags win).
func (f *runFlags) applyConfig(c config.Config) {
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
