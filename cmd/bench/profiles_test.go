package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
)

// TestProfileSession_BasicProfilesNonEmpty verifies the default capture
// (no --full-profiles) writes non-empty cpu/heap/goroutine profiles and
// does NOT write mutex/block.
func TestProfileSession_BasicProfilesNonEmpty(t *testing.T) {
	root := t.TempDir()
	sess, err := startProfileSession(root, "", "unit", false)
	if err != nil {
		t.Fatalf("startProfileSession: %v", err)
	}
	if err := sess.writeSeed(bsbench.Opts{Workload: "unit", Ops: 1, BlockSize: 4096, Seed: 7}); err != nil {
		t.Fatalf("writeSeed: %v", err)
	}
	burnCPUAndAlloc()
	if err := sess.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	for _, name := range []string{"cpu", "heap", "goroutine"} {
		requireNonEmpty(t, sess.dir, name)
	}
	seed, err := os.ReadFile(filepath.Join(sess.dir, "seed.txt"))
	if err != nil {
		t.Fatalf("read seed.txt: %v", err)
	}
	if !strings.Contains(string(seed), "seed=7") {
		t.Errorf("seed.txt missing seed param: %q", seed)
	}
	for _, name := range []string{"mutex", "block"} {
		if _, err := os.Stat(filepath.Join(sess.dir, name+".pprof")); !os.IsNotExist(err) {
			t.Errorf("%s.pprof should not exist without --full-profiles (err=%v)", name, err)
		}
	}
}

// TestProfileSession_FullProfilesNonEmpty verifies --full-profiles adds
// non-empty mutex + block profiles. Mutex/block are empty unless the
// runtime profilers are enabled (the #671 wiring) — exercising real
// contention here proves the session enables them.
func TestProfileSession_FullProfilesNonEmpty(t *testing.T) {
	root := t.TempDir()
	sess, err := startProfileSession(root, "", "unit-full", true)
	if err != nil {
		t.Fatalf("startProfileSession: %v", err)
	}
	burnCPUAndAlloc()
	contendMutexAndBlock()
	if err := sess.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	for _, name := range []string{"cpu", "heap", "goroutine", "mutex", "block"} {
		requireNonEmpty(t, sess.dir, name)
	}
}

// TestProfileSession_StopIdempotent confirms the deferred safety stop is
// a no-op once the happy-path stop has run.
func TestProfileSession_StopIdempotent(t *testing.T) {
	sess, err := startProfileSession(t.TempDir(), "", "unit-idem", false)
	if err != nil {
		t.Fatalf("startProfileSession: %v", err)
	}
	if err := sess.stop(); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := sess.stop(); err != nil {
		t.Fatalf("second stop should be no-op: %v", err)
	}
}

func requireNonEmpty(t *testing.T, dir, name string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name+".pprof"))
	if err != nil {
		t.Fatalf("stat %s.pprof: %v", name, err)
	}
	if info.Size() == 0 {
		t.Errorf("%s.pprof is empty", name)
	}
}

// cpuSink keeps burnCPUAndAlloc's results live so the compiler cannot
// elide the work, and so the heap profile has retained allocations.
var cpuSink [][]byte

func burnCPUAndAlloc() {
	sink := make([][]byte, 0, 64)
	var acc uint64
	for i := 0; i < 1<<16; i++ {
		acc += uint64(i * i)
		if i%1024 == 0 {
			sink = append(sink, make([]byte, 4096))
		}
	}
	if acc != 0 {
		cpuSink = sink
	}
}

// contendMutexAndBlock generates real lock contention and channel
// blocking so the mutex/block profiles have samples to record.
func contendMutexAndBlock() {
	var mu sync.Mutex
	ch := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				mu.Lock()
				mu.Unlock() //nolint:staticcheck // intentional tight contention
			}
			ch <- 1
		}()
	}
	for i := 0; i < 8; i++ {
		<-ch
	}
	wg.Wait()
}

// TestStartProfileSession_PhaseSubdir verifies a non-empty phase is inserted
// as a parent dir so baseline/post-fix captures sit side by side, and that an
// empty phase preserves the original flat layout.
func TestStartProfileSession_PhaseSubdir(t *testing.T) {
	root := t.TempDir()
	// Only one CPU profile can be active per process, so each session is
	// stopped before the next starts.
	sess, err := startProfileSession(root, "baseline", "wl", false)
	if err != nil {
		t.Fatalf("startProfileSession: %v", err)
	}
	sessDir := sess.dir
	_ = sess.stop()
	if !strings.Contains(sessDir, filepath.Join("blockstore", "baseline")+string(filepath.Separator)+"wl-") {
		t.Errorf("phase not in path: %s", sessDir)
	}

	flat, err := startProfileSession(root, "", "wl", false)
	if err != nil {
		t.Fatalf("startProfileSession (flat): %v", err)
	}
	flatDir := flat.dir
	_ = flat.stop()
	if !strings.Contains(flatDir, filepath.Join("blockstore", "wl-")) || strings.Contains(flatDir, "baseline") {
		t.Errorf("empty phase should keep flat layout: %s", flatDir)
	}
}

// TestLoadSeed_RoundTrip writes a seed.txt via the session and reloads it,
// asserting every replay-relevant field (including workers) round-trips so
// --replay reproduces a recorded run.
func TestLoadSeed_RoundTrip(t *testing.T) {
	// The large-seed case guards the uint64 parse: a seed above math.MaxInt64
	// must survive the round-trip (signed Atoi would silently drop it to 0).
	cases := []struct {
		name string
		want bsbench.Opts
	}{
		{"small-seed", bsbench.Opts{
			Workload: "mixed-ops-storm", Ops: 1234, BlockSize: 8192,
			WorkingSet: 16, Workers: 8, Seed: 99, Remote: "memory",
		}},
		{"max-uint64-seed", bsbench.Opts{
			Workload: "mixed-ops-storm", Ops: 10, BlockSize: 4096,
			WorkingSet: 1, Workers: 2, Seed: math.MaxUint64, Remote: "memory",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := startProfileSession(t.TempDir(), "", "mixed-ops-storm", true)
			if err != nil {
				t.Fatalf("startProfileSession: %v", err)
			}
			defer func() { _ = sess.stop() }()
			if err := sess.writeSeed(tc.want); err != nil {
				t.Fatalf("writeSeed: %v", err)
			}
			got, full, err := loadSeed(sess.dir)
			if err != nil {
				t.Fatalf("loadSeed: %v", err)
			}
			if !full {
				t.Error("full_profiles should round-trip as true")
			}
			if got.Workload != tc.want.Workload || got.Ops != tc.want.Ops || got.BlockSize != tc.want.BlockSize ||
				got.WorkingSet != tc.want.WorkingSet || got.Workers != tc.want.Workers || got.Seed != tc.want.Seed ||
				got.Remote != tc.want.Remote {
				t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestStartProfileSession_RejectsBadPhase guards against a --phase that would
// escape <profile-dir>/blockstore via filepath.Join.
func TestStartProfileSession_RejectsBadPhase(t *testing.T) {
	for _, phase := range []string{"..", ".", "../evil", "a/b", "/abs"} {
		if _, err := startProfileSession(t.TempDir(), phase, "wl", false); err == nil {
			t.Errorf("phase %q: expected error, got nil", phase)
		}
	}
}

// TestLoadSeed_Missing surfaces a clear error when the dir has no seed.txt.
func TestLoadSeed_Missing(t *testing.T) {
	if _, _, err := loadSeed(t.TempDir()); err == nil {
		t.Fatal("expected error for missing seed.txt")
	}
}
