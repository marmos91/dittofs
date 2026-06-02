package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
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

// startProfileSession creates <root>/blockstore/[<phase>/]<workload>-<UTC-ts>/
// and begins CPU profiling. When phase is non-empty (e.g. "baseline" or
// "post-fix") it is inserted as a parent directory so before/after captures
// sit side by side. When full is set it also turns on the mutex and block
// profilers. The caller must invoke stop() exactly once, after the timed
// region, to flush every profile and restore runtime profiler state. On error
// the partially-started session is rolled back.
func startProfileSession(rootDir, phase, workload string, full bool) (*profileSession, error) {
	// phase is a single directory name, not a path: reject separators / "."/".."
	// so a stray --phase can't escape <rootDir>/blockstore via filepath.Join.
	if phase != "" && (phase != filepath.Base(phase) || phase == "." || phase == "..") {
		return nil, fmt.Errorf("invalid phase %q: must be a single path element", phase)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	parts := []string{rootDir, "blockstore"}
	if phase != "" {
		parts = append(parts, phase)
	}
	parts = append(parts, fmt.Sprintf("%s-%s", workload, ts))
	dir := filepath.Join(parts...)
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
// seed.txt — workload, ops, block size, working set, workers, seed, remote,
// full-profiles. Re-run via --replay to reproduce the captured profile set.
func (s *profileSession) writeSeed(opts bsbench.Opts) error {
	if s == nil {
		return nil
	}
	body := fmt.Sprintf(
		"workload=%s\nops=%d\nblock_size=%d\nworking_set=%d\nworkers=%d\nseed=%d\nremote=%s\nfull_profiles=%t\n",
		opts.Workload, opts.Ops, opts.BlockSize, opts.WorkingSet, opts.Workers, opts.Seed, opts.Remote, s.full,
	)
	if err := os.WriteFile(filepath.Join(s.dir, "seed.txt"), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write seed.txt: %w", err)
	}
	return nil
}

// loadSeed reads a seed.txt written by writeSeed and reconstructs the workload
// Opts plus the full-profiles flag, so `--replay <dir>` reproduces a recorded
// run exactly. ProfileDir is left to the caller (the replay writes a fresh
// capture dir). Unknown keys are ignored so older/newer seed files still load.
func loadSeed(dir string) (bsbench.Opts, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "seed.txt"))
	if err != nil {
		return bsbench.Opts{}, false, fmt.Errorf("read seed.txt: %w", err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			kv[k] = v
		}
	}
	opts := bsbench.Opts{
		Workload:   kv["workload"],
		Ops:        atoiDefault(kv["ops"], 0),
		BlockSize:  atoiDefault(kv["block_size"], 0),
		WorkingSet: atoiDefault(kv["working_set"], 1),
		Workers:    atoiDefault(kv["workers"], 1),
		// Seed is a uint64 — parse the full range, not via Atoi (which is
		// signed and would silently drop any seed > math.MaxInt64 to 0,
		// reproducing the wrong PRNG stream on replay).
		Seed:   parseUintDefault(kv["seed"], 0),
		Remote: kv["remote"],
	}
	if opts.Workload == "" || opts.Ops <= 0 {
		return bsbench.Opts{}, false, fmt.Errorf("seed.txt missing required workload/ops")
	}
	return opts, kv["full_profiles"] == "true", nil
}

// atoiDefault parses s as an int, returning def on any parse failure.
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// parseUintDefault parses s as a uint64, returning def on any parse failure.
func parseUintDefault(s string, def uint64) uint64 {
	if n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64); err == nil {
		return n
	}
	return def
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
