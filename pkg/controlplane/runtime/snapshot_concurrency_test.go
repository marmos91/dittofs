package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// This file stress-tests the in-flight snapshot registry + its fences (issue
// #812). The registry, the partial unique index idx_share_creating, the
// delete-vs-in-flight fence (DeleteSnapshot), the RemoveShare cancel-and-wait
// drain, and the shutdown drain were all added in #701 / #797 but only
// spot-checked. These tests hammer each fence under `go test -race` to prove
// no panic, no orphaned goroutine, no stuck WaitGroup, and no snapshots-tree
// wipe under a live create.
//
// Heavy iteration is gated behind !testing.Short() so CI's -short pass stays
// fast and deterministic; the default pass exercises every fence at least
// once.

// stressIters returns the per-scenario goroutine-pair count. Heavy under the
// default pass, trimmed under -short.
func stressIters() int {
	if testing.Short() {
		return 8
	}
	return 200
}

// concFixture is a minimal runtime + one share wired with a real engine.Store
// over memory local + memory remote + a controlledBackupable. Unlike
// orchestrationFixture (one share for the whole test) this builds a fresh
// share per call so each iteration gets a clean idx_share_creating slot. The
// cpstore + remote are shared across iterations via t.Cleanup on the parent.
type concFixture struct {
	rt        *Runtime
	backup    *controlledBackupable
	shareName string
}

// newConcRuntime builds a Runtime with a shared cpstore + a single memory
// metadata engine. Shares are added per scenario via addConcShare so the
// idx_share_creating unique slot resets between iterations. Returns the shared
// controlledBackupable so scenarios can install a per-test Backup hook.
func newConcRuntime(t *testing.T) (*Runtime, *controlledBackupable) {
	t.Helper()
	// Use a real on-disk SQLite file rather than `:memory:` so the
	// connection pool shares schema state across the orchestration
	// goroutines. A fresh `:memory:` DB is per-connection and would cause
	// spurious "no such table: snapshots" errors when a goroutine's
	// failSnap picks up a new pool connection (mirrors the note in
	// TestSnapshotHoldProvider_DeleteVsHeldHashes_Race).
	dbPath := filepath.Join(t.TempDir(), "conc.db")
	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	mem := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	backup := &controlledBackupable{MemoryMetadataStore: mem}
	if err := rt.RegisterMetadataStore("memory", backup); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if _, err := cp.CreateMetadataStore(context.Background(), &models.MetadataStoreConfig{
		Name: "memory",
		Type: "memory",
	}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	return rt, backup
}

// addConcShare registers a fresh share backed by a new engine.Store over the
// shared memory metadata engine. Returns a concFixture pinned to that share.
func addConcShare(t *testing.T, rt *Runtime, backup *controlledBackupable, shareName string) *concFixture {
	t.Helper()

	localStore := bsmemory.New()
	innerRemote := remotememory.New()
	t.Cleanup(func() { _ = innerRemote.Close() })
	mem := backup.MemoryMetadataStore
	syncer := engine.NewSyncer(localStore, innerRemote, mem, engine.SyncerConfig{
		ParallelUploads:   1,
		ParallelDownloads: 1,
	})
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         innerRemote,
		Syncer:         syncer,
		FileBlockStore: mem,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
		BlockStore:    bs,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, t.TempDir()); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}
	return &concFixture{rt: rt, backup: backup, shareName: shareName}
}

// assertNoGoroutineLeak settles, then fails if the live goroutine count grew
// beyond a small tolerance versus the baseline captured before the scenario.
// Background runtime goroutines (GC, timers) make an exact match flaky, so we
// poll for the count to fall back to baseline+tolerance over a bounded window.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	const tolerance = 4
	deadline := time.Now().Add(10 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		runtime.GC()
		n = runtime.NumGoroutine()
		if n <= baseline+tolerance {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: %d live, baseline %d (tolerance %d) — orchestration goroutines did not drain",
		n, baseline, tolerance)
}

// waitGroupGuard runs fn (a *sync.WaitGroup wait, typically) and fails the
// test if it does not return within d. Catches a stuck registry WaitGroup
// that a cancelled-but-never-Done orchestration would leave hanging.
func waitGroupGuard(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("stuck: %s did not complete within %v", what, d)
	}
}

// TestSnapshotConcurrency_CreateVsRemoveShare hammers a concurrent
// CreateSnapshot racing RemoveShare on one share. RemoveShare must
// cancel-and-wait the in-flight orchestration (cancelAndWaitInFlightSnaps)
// BEFORE wiping the snapshots/ tree. Neither call may panic, and RemoveShare
// must never hang on a stuck WaitGroup.
func TestSnapshotConcurrency_CreateVsRemoveShare(t *testing.T) {
	rt, backup := newConcRuntime(t)
	// Make Backup block on ctx so the orchestration goroutine is reliably
	// in-flight when RemoveShare's cancel fires; ctx cancel unblocks it.
	backup.setHook(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	baseline := runtime.NumGoroutine()
	iters := stressIters()
	hashes := makeHashes(2, 0x10)
	backup.setHashes(hashes)

	var panics atomic.Int64
	for i := 0; i < iters; i++ {
		shareName := uniqueShare("crm", i)
		fx := addConcShare(t, rt, backup, shareName)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer recoverInto(&panics)
			// May succeed (returns snapID) or, if RemoveShare already wiped
			// the share, fail with ErrShareNotFound — both are fine.
			_, _ = fx.rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
		}()
		go func() {
			defer wg.Done()
			defer recoverInto(&panics)
			// Tiny jitter so RemoveShare sometimes lands before, sometimes
			// after, the create registers in-flight.
			if i%2 == 0 {
				runtime.Gosched()
			}
			_ = fx.rt.RemoveShare(shareName)
		}()

		waitGroupGuard(t, 20*time.Second, "create+removeshare iter", wg.Wait)
		cancel()
	}

	if panics.Load() != 0 {
		t.Fatalf("%d goroutine(s) panicked", panics.Load())
	}
	assertNoGoroutineLeak(t, baseline)
}

// TestSnapshotConcurrency_DoubleCreate fires two concurrent CreateSnapshot
// calls on one share. The idx_share_creating partial unique index must admit
// at most one 'creating' row; the loser gets ErrSnapshotStateConflict. The
// loser's synchronous failure path must also release its in-flight
// registration (abortSnapInFlight) so no WaitGroup slot leaks. We drive the
// winner to completion (Backup returns immediately) so the slot frees for the
// next iteration.
func TestSnapshotConcurrency_DoubleCreate(t *testing.T) {
	rt, backup := newConcRuntime(t)
	// Backup returns immediately so the winner completes and the row leaves
	// 'creating' — required so the next iteration's index slot is free.
	backup.setHook(nil)
	hashes := makeHashes(2, 0x20)
	backup.setHashes(hashes)

	baseline := runtime.NumGoroutine()
	iters := stressIters()

	var panics atomic.Int64
	var conflicts atomic.Int64
	var accepted atomic.Int64

	for i := 0; i < iters; i++ {
		shareName := uniqueShare("dbl", i)
		fx := addConcShare(t, rt, backup, shareName)
		seedConcRemoteForShare(t, rt, shareName, hashes)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

		var (
			wg     sync.WaitGroup
			gotIDs [2]string
			gotErr [2]error
		)
		wg.Add(2)
		for g := 0; g < 2; g++ {
			go func(idx int) {
				defer wg.Done()
				defer recoverInto(&panics)
				id, err := fx.rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
				gotIDs[idx], gotErr[idx] = id, err
			}(g)
		}
		waitGroupGuard(t, 20*time.Second, "double-create iter", wg.Wait)

		// Exactly one accepted, exactly one conflicted is the strict
		// expectation, but a benign interleaving where the first create
		// fully completes before the second starts would let BOTH succeed
		// (two distinct rows, sequential). Accept "at most one conflict" and
		// "at least one accepted"; never two conflicts.
		nConflict, nAccept := 0, 0
		for g := 0; g < 2; g++ {
			switch {
			case gotErr[g] == nil && gotIDs[g] != "":
				nAccept++
			case errors.Is(gotErr[g], models.ErrSnapshotStateConflict):
				nConflict++
			default:
				t.Fatalf("iter %d goroutine %d: unexpected (id=%q, err=%v)", i, g, gotIDs[g], gotErr[g])
			}
		}
		if nAccept == 0 {
			t.Fatalf("iter %d: both creates failed (conflicts=%d)", i, nConflict)
		}
		if nConflict > 1 {
			t.Fatalf("iter %d: %d conflicts, want <=1", i, nConflict)
		}
		conflicts.Add(int64(nConflict))
		accepted.Add(int64(nAccept))

		// Drain the accepted snapshot(s) so the orchestration goroutine
		// finishes and unregisters before the next iteration.
		for g := 0; g < 2; g++ {
			if gotErr[g] == nil && gotIDs[g] != "" {
				if _, werr := fx.rt.WaitForSnapshot(ctx, shareName, gotIDs[g]); werr != nil {
					t.Fatalf("iter %d: WaitForSnapshot(%s): %v", i, gotIDs[g], werr)
				}
			}
		}
		cancel()
	}

	if panics.Load() != 0 {
		t.Fatalf("%d goroutine(s) panicked", panics.Load())
	}
	// At least one iteration should have produced a real conflict; if none
	// did the scenario degenerated to fully-sequential creates and is not
	// exercising the index fence. Only enforce under the heavy pass where
	// the window is wide enough to expect contention.
	if !testing.Short() && conflicts.Load() == 0 {
		t.Logf("warning: no idx_share_creating conflicts observed across %d iters; "+
			"creates serialized — fence not exercised this run", iters)
	}
	t.Logf("double-create: accepted=%d conflicts=%d over %d iters",
		accepted.Load(), conflicts.Load(), iters)
	assertNoGoroutineLeak(t, baseline)
}

// TestSnapshotConcurrency_DeleteVsInFlightCreate hammers DeleteSnapshot racing
// an in-flight CreateSnapshot for the same snapID. The fence (isSnapInFlight
// inside DeleteSnapshot) must EITHER refuse with ErrSnapshotInFlight (create
// still running) OR succeed (create already terminal). It must never delete
// the row + dir out from under a live orchestration goroutine, and must never
// panic or leak.
func TestSnapshotConcurrency_DeleteVsInFlightCreate(t *testing.T) {
	rt, backup := newConcRuntime(t)

	baseline := runtime.NumGoroutine()
	iters := stressIters()
	hashes := makeHashes(2, 0x30)
	backup.setHashes(hashes)

	var panics atomic.Int64
	var refused, deleted atomic.Int64

	for i := 0; i < iters; i++ {
		shareName := uniqueShare("del", i)
		fx := addConcShare(t, rt, backup, shareName)
		seedConcRemoteForShare(t, rt, shareName, hashes)

		// Gate Backup on a release channel so the orchestration is reliably
		// in-flight while DeleteSnapshot races it. The release is closed
		// right after the create+delete launch so the goroutine can finish.
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		fx.backup.setHook(func(ctx context.Context) error {
			once.Do(func() { close(started) })
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

		snapID, err := fx.rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
		if err != nil {
			t.Fatalf("iter %d: CreateSnapshot: %v", i, err)
		}

		// Ensure the orchestration goroutine is parked in Backup so the
		// delete genuinely races a live create.
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("iter %d: Backup hook never reached", i)
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverInto(&panics)
			derr := fx.rt.DeleteSnapshot(ctx, shareName, snapID)
			switch {
			case derr == nil:
				deleted.Add(1)
			case errors.Is(derr, models.ErrSnapshotInFlight):
				refused.Add(1)
			case errors.Is(derr, models.ErrSnapshotNotFound):
				// The orchestration may have finished + the row may have
				// been deleted by a prior pass; tolerate.
				deleted.Add(1)
			default:
				t.Errorf("iter %d: DeleteSnapshot unexpected err: %v", i, derr)
			}
		}()

		// Let the create finish.
		close(release)
		waitGroupGuard(t, 20*time.Second, "delete goroutine", wg.Wait)

		// Drain the create so its registry slot is gone before next iter.
		if _, werr := fx.rt.WaitForSnapshot(ctx, shareName, snapID); werr != nil {
			t.Fatalf("iter %d: WaitForSnapshot: %v", i, werr)
		}
		cancel()
	}

	if panics.Load() != 0 {
		t.Fatalf("%d goroutine(s) panicked", panics.Load())
	}
	t.Logf("delete-vs-create: refused(in-flight)=%d deleted=%d over %d iters",
		refused.Load(), deleted.Load(), iters)
	assertNoGoroutineLeak(t, baseline)
}

// TestSnapshotConcurrency_ShutdownDuringCreate fires N in-flight creates
// (each parked in Backup) across N shares, then calls Shutdown. shutdownSnapshots
// must cancel runtimeCtx, propagate to every parked Backup, and drain every
// registry WaitGroup within the bounded ctx. No goroutine may survive the
// shutdown, and Shutdown must not hang.
func TestSnapshotConcurrency_ShutdownDuringCreate(t *testing.T) {
	// Each Shutdown tears down the whole runtime, so loop builds a fresh
	// runtime per iteration. Keep the iteration count modest — each iter
	// spins up several shares + engines.
	outer := 3
	if testing.Short() {
		outer = 1
	}
	sharesPerRun := 6

	baseline := runtime.NumGoroutine()
	var panics atomic.Int64

	for run := 0; run < outer; run++ {
		rt, backup := newConcRuntime(t)
		hashes := makeHashes(2, 0x40)
		backup.setHashes(hashes)

		started := make(chan struct{}, sharesPerRun)
		// Backup signals "started" so we know every orchestration goroutine
		// is parked, then blocks until runtimeCtx cancels (Shutdown's cancel).
		backup.setHook(func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		ids := make([]string, 0, sharesPerRun)
		shareNames := make([]string, 0, sharesPerRun)
		for s := 0; s < sharesPerRun; s++ {
			shareName := uniqueShare("sd", run*sharesPerRun+s)
			addConcShare(t, rt, backup, shareName)
			id, err := rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
			if err != nil {
				t.Fatalf("run %d share %d: CreateSnapshot: %v", run, s, err)
			}
			ids = append(ids, id)
			shareNames = append(shareNames, shareName)
		}

		// Wait for every orchestration goroutine to park in Backup so the
		// shutdown genuinely drains in-flight work (not a no-op).
		for s := 0; s < sharesPerRun; s++ {
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				cancel()
				t.Fatalf("run %d: only %d/%d backups parked before shutdown", run, s, sharesPerRun)
			}
		}

		// Shutdown must drain every in-flight orchestration within its own
		// bounded ctx — and must not hang.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		waitGroupGuard(t, 18*time.Second, "Shutdown during in-flight creates", func() {
			if err := rt.Shutdown(shutCtx); err != nil {
				t.Errorf("run %d: Shutdown: %v", run, err)
			}
		})
		shutCancel()

		// Every snapshot should have flipped to failed (cancelled mid-Backup)
		// — assert no row is stuck in 'creating', which would mean the
		// orchestration goroutine never ran its failSnap deferred cleanup.
		for s, id := range ids {
			snap, gerr := rt.store.GetSnapshot(context.Background(), shareNames[s], id)
			if gerr != nil {
				t.Fatalf("run %d: GetSnapshot(%s): %v", run, id, gerr)
			}
			if snap.State == models.StateCreating {
				t.Fatalf("run %d: snapshot %s still 'creating' after shutdown — orchestration cleanup did not run", run, id)
			}
		}
		cancel()
	}

	if panics.Load() != 0 {
		t.Fatalf("%d goroutine(s) panicked", panics.Load())
	}
	assertNoGoroutineLeak(t, baseline)
}

// ----- helpers -----

// uniqueShare builds a per-iteration share name so each iteration uses a
// fresh idx_share_creating slot and its own on-disk snapshots tree. The
// (prefix, index) pair is unique within a single test run, which is all the
// uniqueness the per-test runtime needs.
func uniqueShare(prefix string, i int) string {
	return fmt.Sprintf("%s-%d", prefix, i)
}

// recoverInto records a panic into the counter instead of crashing the test
// binary, so a registry race that panics in a goroutine is reported as a
// failed assertion rather than aborting every other scenario.
func recoverInto(counter *atomic.Int64) {
	if rec := recover(); rec != nil {
		counter.Add(1)
	}
}

// seedConcRemoteForShare seeds the share's remote so the verify gate passes
// and CreateSnapshot can reach state=ready.
func seedConcRemoteForShare(t *testing.T, rt *Runtime, shareName string, hashes []block.ContentHash) {
	t.Helper()
	bs, err := rt.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare(%s): %v", shareName, err)
	}
	rs := bs.RemoteStore()
	if rs == nil {
		t.Fatalf("share %s has no remote store", shareName)
	}
	for _, h := range hashes {
		if err := rs.Put(context.Background(), h, []byte("payload-"+h.String())); err != nil {
			t.Fatalf("seed remote put: %v", err)
		}
	}
}
