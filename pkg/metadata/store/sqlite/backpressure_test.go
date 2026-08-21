package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/txretry"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

const (
	// fixedAttemptCeiling is the total wait of the fixed 3-attempt (10/20/30ms)
	// retry loop this test guards against: a transaction that blocks longer than
	// this is one that loop would have given up on and returned EIO for.
	fixedAttemptCeiling = 60 * time.Millisecond

	// callLimit bounds a single transaction against the budget it retries
	// within, not against elapsed test time: WithTransaction returns once that
	// budget elapses, so no call can legitimately run far past it however slow
	// the machine is. The multiple is slack for scheduling and one final attempt
	// — large enough that reaching it means the call stopped terminating on its
	// own, not that the disk was slow.
	callLimit = 4 * txretry.Budget
)

// TestSQLite_ConcurrentWritesBackpressureNoEIO is the #1769 regression guard.
//
// Under concurrent writers contending on a single hot row (the shared parent
// directory inode), the metadata store must backpressure — block and retry
// until the transaction succeeds — never surface metadata.ErrIOError (which
// maps to NFS3ErrIO / EIO) to the caller. Before the fix, WithTransaction gave
// up after 3 attempts (10/20/30ms) and returned ErrIOError.
//
// A single sqlite store pins MaxOpenConns(1), so writers through one store
// already serialize at the Go pool and never collide. Real SQLITE_BUSY
// contention needs multiple connections to the same file, so this test opens
// several store handles against one on-disk database and hammers the same
// directory row across all of them with a tiny busy_timeout. All mutations
// must succeed.
func TestSQLite_ConcurrentWritesBackpressureNoEIO(t *testing.T) {
	// No test-wide deadline: each transaction is bounded by the store's own
	// retry budget instead. A ctx deadline would decide the verdict itself —
	// WithTransaction tightens its retry budget to an earlier ctx deadline, so
	// the budget shrinks to nothing as the run approaches it, and the deadline
	// aborts whatever statement is in flight when it fires.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "backpressure.db")

	newStore := func(autoMigrate bool) metadata.Store {
		t.Helper()
		cfg := &sqlite.SQLiteMetadataStoreConfig{
			Path:        dbPath,
			AutoMigrate: autoMigrate,
			// busy_timeout bounds how long the driver waits for the single write
			// lock before returning SQLITE_BUSY up to WithTransaction's retry loop.
			// It is kept small so sustained cross-connection contention still
			// surfaces SQLITE_BUSY and exercises that retry/backpressure path — but
			// not so small that every brief collision is handed to the retry loop,
			// which under heavy contention can exhaust the loop's per-call time
			// budget and turn a transient conflict into EIO. At this value the
			// driver absorbs sub-threshold collisions while only sustained
			// contention reaches the retry loop, so colliding writers backpressure
			// (run slow) rather than error.
			BusyTimeout: 50 * time.Millisecond,
		}
		store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
		if err != nil {
			t.Fatalf("NewSQLiteMetadataStore() failed: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	// First handle owns migration + creates the shared share/root row.
	const share = "/hot"
	primary := newStore(true)
	if _, err := primary.CreateRootDirectory(ctx, share, &metadata.FileAttr{Mode: 0o755}); err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", share, err)
	}

	// Several independent connections (one per store handle) to the SAME file so
	// concurrent writers genuinely collide on the sqlite write lock.
	const (
		stores          = 4
		writersPerStore = 4
		iters           = 60
	)
	handles := []metadata.Store{primary}
	for i := 1; i < stores; i++ {
		handles = append(handles, newStore(false))
	}

	var wg sync.WaitGroup
	var backpressured atomic.Int64
	errCh := make(chan error, stores*writersPerStore*iters)
	start := make(chan struct{})

	for _, store := range handles {
		for w := 0; w < writersPerStore; w++ {
			wg.Add(1)
			go func(store metadata.Store) {
				defer wg.Done()
				rootHandle, err := store.GetRootHandle(ctx, share)
				if err != nil {
					errCh <- err
					return
				}
				<-start // release all writers together to maximize contention
				for i := 0; i < iters; i++ {
					// Bound the single call, so a regression to an unbounded wait
					// fails here instead of hanging until the package timeout.
					// Later than the budget, so it never becomes the budget.
					callCtx, cancel := context.WithTimeout(ctx, callLimit)
					began := time.Now()
					err := store.WithTransaction(callCtx, func(tx metadata.Transaction) error {
						// Read + rewrite the SAME parent-directory inode: a genuine
						// hot-row write conflict across all connections.
						f, err := tx.GetFile(callCtx, rootHandle)
						if err != nil {
							return err
						}
						f.Mtime = time.Now()
						return tx.UpdateAttrs(callCtx, f)
					})
					cancel()
					blocked := time.Since(began)
					if err != nil {
						if blocked >= callLimit {
							t.Errorf("WithTransaction blocked %v without returning, past the %v budget it retries within (limit %v)", blocked, txretry.Budget, callLimit)
							return
						}
						errCh <- err
						return
					}
					if blocked > fixedAttemptCeiling {
						backpressured.Add(1)
					}
				}
			}(store)
		}
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		var se *metadata.StoreError
		if errors.As(err, &se) && se.Code == metadata.ErrIOError {
			t.Fatalf("write returned EIO under contention (should backpressure, #1769): %v", err)
		}
		t.Fatalf("unexpected error under contention: %v", err)
	}

	// How much of the retry path this run actually reached. Reported rather than
	// asserted on: the count is how often the driver's busy_timeout was fully
	// exhausted, which on fast idle hardware happens only a handful of times in
	// the whole run (measured across 200 runs: min 1, median 5), so any floor
	// would be a coin toss there. A slow or loaded machine — the one that
	// mattered for this guard — reaches it far more often.
	t.Logf("%d/%d transactions blocked past %v and still succeeded", backpressured.Load(), stores*writersPerStore*iters, fixedAttemptCeiling)
}
