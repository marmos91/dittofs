package run

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/backend"
	"github.com/marmos91/dittofs/internal/dfsbench/config"
	"github.com/marmos91/dittofs/internal/dfsbench/exec"
	"github.com/marmos91/dittofs/internal/dfsbench/fio"
	"github.com/marmos91/dittofs/internal/dfsbench/report"
)

// runManaged drives the backend-provisioning path: resolve --systems into
// (backend, protocol) plans, then set up / mount / fio / evict / tear down each.
func runManaged(ctx context.Context, f *runFlags, opts fio.LoadOpts, cfg config.Config) error {
	plans, err := backend.ResolveSystems(f.systems)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return fmt.Errorf("no runnable (backend, protocol) plans from --systems %v", f.systems)
	}
	wls := f.workloads
	if len(wls) == 0 {
		wls = fio.KnownWorkloads
	}
	for _, w := range wls {
		if !fio.ValidWorkload(w) {
			return fmt.Errorf("unknown workload %q (see `dfsbench list`)", w)
		}
	}
	sizes := f.sizes
	if len(sizes) == 0 {
		sizes = []string{"medium"}
	}

	cells := backend.ManagedMatrix(plans, wls, sizes, f.evictCache)
	if f.dryRun {
		printMatrix(cells)
		return nil
	}
	want := make(map[string]bool, len(cells))
	for _, c := range cells {
		want[(fio.CellResult{System: c.System, Workload: c.Workload, Size: c.Size, Protocol: c.Protocol, Pass: c.Pass}).Slug()] = true
	}

	env := backend.BackendEnv{Bucket: cfg.Bucket, Endpoint: cfg.Endpoint}
	// A comparison shouldn't die because one competitor's recipe breaks — record
	// the failure and keep going, so one run surfaces every backend's state.
	var failures []string
	for _, p := range plans {
		if err := runPlan(ctx, p, env, wls, sizes, opts, f); err != nil {
			_, _ = fmt.Fprintf(exec.CmdOut, "FAIL %s: %v\n", p.SystemLabel(), err)
			failures = append(failures, fmt.Sprintf("%s: %v", p.SystemLabel(), err))
		}
	}

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
	if len(failures) > 0 {
		_, _ = fmt.Fprintf(exec.CmdOut, "\n%d backend(s) failed:\n", len(failures))
		for _, fl := range failures {
			_, _ = fmt.Fprintf(exec.CmdOut, "  - %s\n", fl)
		}
	}
	return nil
}

// runPlan runs one (backend, protocol) through its full lifecycle. Setup is
// idempotent, so re-running it per protocol is wasteful but correct — matches
// the plan's "one competitor at a time, full teardown between" model.
func runPlan(ctx context.Context, p backend.Plan, env backend.BackendEnv, wls, sizes []string, opts fio.LoadOpts, f *runFlags) (err error) {
	b := p.Backend
	if b.Setup == nil || b.Mount == nil {
		_, _ = fmt.Fprintf(exec.CmdOut, "skip %s: no recipe yet\n", p.SystemLabel())
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

	mnt, err := b.Mount(ctx, p.Protocol)
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	if b.Unmount != nil {
		defer func() { _ = b.Unmount(ctx, p.Protocol) }()
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
		if err := exec.DropOSCache(ctx); err != nil {
			_, _ = fmt.Fprintf(exec.CmdOut, "warn: drop OS cache: %v\n", err)
		}
		var reads []string
		for _, w := range wls {
			if backend.ReadWorkloads[w] {
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
func runPass(ctx context.Context, p backend.Plan, pass string, wls, sizes []string, mnt string, opts fio.LoadOpts, f *runFlags) error {
	for _, w := range wls {
		for _, s := range sizes {
			res := fio.CellResult{System: p.SystemLabel(), Workload: w, Size: s, Protocol: string(p.Protocol), Pass: pass}
			if f.resume && fio.ResultExists(f.results, res.Slug()) {
				_, _ = fmt.Fprintf(exec.CmdOut, "skip (resume): %s\n", res.Slug())
				continue
			}
			_, _ = fmt.Fprintf(exec.CmdOut, "run: %s\n", res.Slug())
			m, err := fio.RunFio(ctx, w, mnt, withSize(opts, s))
			if err != nil {
				return err
			}
			m.System, m.Workload, m.Size, m.Protocol, m.Pass = res.System, w, s, res.Protocol, pass
			m.Timestamp = time.Now().UTC()
			if err := m.Save(f.results); err != nil {
				return err
			}
		}
	}
	return nil
}

func printMatrix(cells []backend.Cell) {
	_, _ = fmt.Fprintf(exec.CmdOut, "matrix (%d cells):\n", len(cells))
	for _, c := range cells {
		_, _ = fmt.Fprintf(exec.CmdOut, "  %s | %s | %s | %s | %s\n", c.System, c.Workload, c.Size, c.Protocol, c.Pass)
	}
}
