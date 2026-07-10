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
	"github.com/marmos91/dittofs/internal/dfsbench/sysstat"
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

	// Measure the local-disk ceiling once, up front, so the scorecard opens with
	// the "is the bottleneck the FS or the hardware?" anchor. Best-effort: a
	// failed/absent baseline warns and the comparison still runs.
	var baseline report.Baseline
	if !f.skipBaseline {
		if b, err := measureLocalDiskCeiling(ctx, maxReadSize(sizes), opts); err != nil {
			_, _ = fmt.Fprintf(exec.CmdOut, "warn: baseline: %v\n", err)
		} else {
			baseline = b
		}
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
	_, _ = fmt.Fprint(exec.CmdOut, report.RenderBaseline(baseline))
	_, _ = fmt.Fprint(exec.CmdOut, report.RenderTable(rows))
	_, _ = fmt.Fprint(exec.CmdOut, report.RenderPairing(rows))
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
	// Register teardown BEFORE Setup so a Setup that fails partway — e.g. after
	// starting the server but before the share is ready — still cleans up. Otherwise
	// a half-started dfs leaks, keeps holding its metadata-store (Badger) directory
	// lock, and wedges every later run's store-create. Teardown is idempotent.
	if b.Teardown != nil {
		defer func() {
			if e := b.Teardown(ctx); e != nil && err == nil {
				err = fmt.Errorf("teardown: %w", e)
			}
		}()
	}
	if err := b.Setup(ctx, env); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	mnt, err := b.Mount(ctx, p.Protocol)
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	if b.Unmount != nil {
		defer func() { _ = b.Unmount(ctx, p.Protocol) }()
	}

	// Lay down the read target before any pass, so seq-read reads a real,
	// full-size, durable file (not one fio implicitly creates then can't read
	// cold from a writeback backend).
	if err := layoutReadTarget(ctx, wls, sizes, mnt, opts); err != nil {
		return err
	}

	if err := runPass(ctx, p, "warm", wls, sizes, mnt, opts, f); err != nil {
		return err
	}
	if f.evictCache {
		var reads []string
		for _, w := range wls {
			if backend.ReadWorkloads[w] {
				reads = append(reads, w)
			}
		}
		// Re-establish a cold cache before EACH read workload, not once for all of
		// them. The first cold read pulls the whole file back from S3 into the
		// local cache, so a later cold cell reading the same file would be served
		// warm — that's what made the old single-evict cold rand-read implausibly
		// fast. A per-workload barrier keeps every cold number genuinely cold.
		for _, w := range reads {
			newMnt, err := coldBarrier(ctx, b, p, mnt)
			if err != nil {
				return err
			}
			mnt = newMnt
			if err := runPass(ctx, p, "cold", []string{w}, sizes, mnt, opts, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// coldBarrier forces the next read to come from the backend, not a warm cache:
// FUSE backends bounce the mount stack (unmount re-export → flush+remount the
// FUSE mount so the writeback fully uploads and the cache empties → re-export),
// others evict; then the OS page cache is dropped. Returns the (possibly new)
// client mountpoint. Run before EACH cold read cell so cells don't warm each other.
func coldBarrier(ctx context.Context, b *backend.Backend, p backend.Plan, mnt string) (string, error) {
	if b.FlushFUSE != nil {
		if b.Unmount != nil {
			_ = b.Unmount(ctx, p.Protocol)
		}
		if err := b.FlushFUSE(ctx); err != nil {
			return mnt, fmt.Errorf("flush: %w", err)
		}
		newMnt, err := b.Mount(ctx, p.Protocol)
		if err != nil {
			return mnt, fmt.Errorf("remount: %w", err)
		}
		mnt = newMnt
	} else if b.Evict != nil {
		if err := b.Evict(ctx); err != nil {
			return mnt, fmt.Errorf("evict: %w", err)
		}
	}
	// Drop the OS page cache too, so reads come from the backend, not client RAM.
	if err := exec.DropOSCache(ctx); err != nil {
		_, _ = fmt.Fprintf(exec.CmdOut, "warn: drop OS cache: %v\n", err)
	}
	return mnt, nil
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
			// Meter server-side ctxsw + CPU across just the fio pass — the FUSE-tax
			// evidence (system-wide; on the single-tenant bench VM that's the
			// serving stack). Zero off Linux.
			before := sysstat.Now()
			m, err := fio.RunFio(ctx, w, mnt, withSize(opts, s))
			if err != nil {
				return err
			}
			r := before.RatesTo(sysstat.Now())
			m.CtxSwPerSec, m.CPUPct = r.CtxSwPerSec, r.CPUPct
			m.DiskWrMBps, m.NetRxMBps = r.DiskWrMBps, r.NetRxMBps
			m.Metered = r.Metered
			// "native" | "reexport" — the pairing axis. Guard the Support.String()
			// "na" sentinel to empty so an unexpected value never leaks into the
			// ACCESS column or the pairing grouping.
			if m.AccessMode = p.Support.String(); m.AccessMode == "na" {
				m.AccessMode = ""
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

// layoutReadTarget writes the file seq-read reads and records its byte count so
// a writeback backend's Evict can wait for the full upload before the cold read.
// It lays down one file at the largest requested size — a smaller-size read cell
// just reads a prefix of it. No-op when no read workload is scheduled.
func layoutReadTarget(ctx context.Context, wls, sizes []string, mnt string, opts fio.LoadOpts) error {
	hasRead := false
	for _, w := range wls {
		if backend.ReadWorkloads[w] {
			hasRead = true
			break
		}
	}
	if !hasRead {
		return nil
	}
	size := maxReadSize(sizes)
	if _, err := fio.RunFio(ctx, "layout", mnt, withSize(opts, size)); err != nil {
		return fmt.Errorf("lay down read target: %w", err)
	}
	return nil
}

// maxReadSize returns the size selector resolving to the most bytes.
func maxReadSize(sizes []string) string {
	best, bestN := sizes[0], int64(-1)
	for _, s := range sizes {
		if n := fio.SizeBytes(fio.ResolveSize(s)); n > bestN {
			best, bestN = s, n
		}
	}
	return best
}

func printMatrix(cells []backend.Cell) {
	_, _ = fmt.Fprintf(exec.CmdOut, "matrix (%d cells):\n", len(cells))
	for _, c := range cells {
		_, _ = fmt.Fprintf(exec.CmdOut, "  %s | %s | %s | %s | %s\n", c.System, c.Workload, c.Size, c.Protocol, c.Pass)
	}
}
