package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
)

// profileSession is the shared pprof capture envelope wrapped around a
// workload's timed region. It always captures cpu, heap, and goroutine.
// When full=true it also enables the runtime mutex/block profilers
// before the timed region and emits mutex.pprof + block.pprof on stop.
//
// mutex/block profiling is opt-in because it adds per-contention-event
// runtime accounting overhead that would skew throughput numbers and is
// not wanted for routine macro runs. The normal `go test` suite never
// constructs a session, so capture has zero effect on it.
//
// Without runtime.SetMutexProfileFraction / SetBlockProfileRate the
// mutex and block profiles are silently empty (see #671) — enabling
// them here is what makes those two profiles meaningful.
type profileSession struct {
	dir     string
	cpuFile *os.File
	full    bool
	stopped bool
}

const (
	// mutexProfileFraction reports 1/N mutex contention events. 1 = report
	// every event; the most detailed setting, fine for a bounded bench run.
	mutexProfileFraction = 1
	// blockProfileRate samples one blocking event per N nanoseconds of
	// blocking. 1 = sample every blocking event.
	blockProfileRate = 1
)

// startProfileSession creates <root>/blockstore/<workload>-<UTC-ts>/ and
// begins CPU profiling. When full is set it also turns on the mutex and
// block profilers. The caller must invoke stop() exactly once, after the
// timed region, to flush every profile and restore runtime profiler
// state. On error the partially-started session is rolled back.
func startProfileSession(rootDir, workload string, full bool) (*profileSession, error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(rootDir, "blockstore", fmt.Sprintf("%s-%s", workload, ts))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir profiles: %w", err)
	}

	f, err := os.Create(filepath.Join(dir, "cpu.pprof"))
	if err != nil {
		return nil, fmt.Errorf("create cpu.pprof: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("StartCPUProfile: %w", err)
	}

	if full {
		runtime.SetMutexProfileFraction(mutexProfileFraction)
		runtime.SetBlockProfileRate(blockProfileRate)
	}

	return &profileSession{dir: dir, cpuFile: f, full: full}, nil
}

// stop finalizes the session: stops CPU profiling and writes heap and
// goroutine profiles (plus mutex/block when full). It is idempotent —
// the deferred safety call after an early error is a no-op once the
// happy path has already stopped. The first encountered error is
// returned; later profiles are still attempted so a single failure does
// not silently drop the rest.
func (s *profileSession) stop() error {
	if s == nil || s.stopped {
		return nil
	}
	s.stopped = true

	pprof.StopCPUProfile()
	err := s.cpuFile.Close()

	// Heap reflects live allocations after a forced GC so the profile is
	// not dominated by not-yet-collected garbage from the timed region.
	runtime.GC()
	err = firstErr(err, writeNamedProfile(s.dir, "heap", "heap"))
	err = firstErr(err, writeNamedProfile(s.dir, "goroutine", "goroutine"))

	if s.full {
		err = firstErr(err, writeNamedProfile(s.dir, "mutex", "mutex"))
		err = firstErr(err, writeNamedProfile(s.dir, "block", "block"))
		// Restore default profiler state so a subsequent run in the same
		// process is not influenced by this session.
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	}
	return err
}

// writeSeed records the exact replay parameters next to the profiles as
// seed.txt — workload, ops, block size, working set, seed, remote. Re-run
// with the same values to reproduce the captured profile set.
func (s *profileSession) writeSeed(opts bsbench.Opts) error {
	if s == nil {
		return nil
	}
	body := fmt.Sprintf(
		"workload=%s\nops=%d\nblock_size=%d\nworking_set=%d\nseed=%d\nremote=%s\nfull_profiles=%t\n",
		opts.Workload, opts.Ops, opts.BlockSize, opts.WorkingSet, opts.Seed, opts.Remote, s.full,
	)
	if err := os.WriteFile(filepath.Join(s.dir, "seed.txt"), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write seed.txt: %w", err)
	}
	return nil
}

// writeNamedProfile writes the named runtime profile to <dir>/<file>.pprof.
func writeNamedProfile(dir, file, profile string) error {
	p := pprof.Lookup(profile)
	if p == nil {
		return fmt.Errorf("unknown profile %q", profile)
	}
	f, err := os.Create(filepath.Join(dir, file+".pprof"))
	if err != nil {
		return fmt.Errorf("create %s.pprof: %w", file, err)
	}
	defer func() { _ = f.Close() }()
	// debug=0 emits the binary protobuf form consumable by `go tool pprof`.
	if err := p.WriteTo(f, 0); err != nil {
		return fmt.Errorf("write %s profile: %w", profile, err)
	}
	return nil
}

func firstErr(prev, next error) error {
	if prev != nil {
		return prev
	}
	return next
}
