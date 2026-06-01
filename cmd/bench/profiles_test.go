package main

import (
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
	sess, err := startProfileSession(root, "unit", false)
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
	sess, err := startProfileSession(root, "unit-full", true)
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
	sess, err := startProfileSession(t.TempDir(), "unit-idem", false)
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
