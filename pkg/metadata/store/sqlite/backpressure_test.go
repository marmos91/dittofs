package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// fixedAttemptCount is the attempt count of the fixed 3-attempt (10/20/30ms)
// retry loop this test guards against. The contending writer must still be
// retrying past it — that is what backpressure means here, and what the old loop
// could not do. The count is asserted rather than the time those attempts happen
// to take: the count is a property of the loop, the duration a property of the
// machine.
const fixedAttemptCount = 3

// TestSQLite_ConcurrentWritesBackpressureNoEIO is the #1769 regression guard.
//
// When a writer collides with the sqlite write lock, the metadata store must
// backpressure — block and retry until the lock frees — never surface
// metadata.ErrIOError (which maps to NFS3ErrIO / EIO) to the caller. Before the
// fix, WithTransaction gave up after 3 attempts (10/20/30ms) and returned
// ErrIOError.
//
// A single sqlite store pins MaxOpenConns(1), so writers through one store
// already serialize at the Go pool and never collide. Real SQLITE_BUSY
// contention needs multiple connections to the same file, so this test opens two
// store handles against one on-disk database: the holder keeps a write
// transaction open on the shared root inode while the contender writes the same
// row. The holder releases once the contender has out-retried the old loop, so
// the verdict rests on the retry loop's behaviour rather than on a wall-clock
// budget measured on a machine of unknown speed.
func TestSQLite_ConcurrentWritesBackpressureNoEIO(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "backpressure.db")

	newStore := func(autoMigrate bool) metadata.Store {
		t.Helper()
		cfg := &sqlite.SQLiteMetadataStoreConfig{
			Path:        dbPath,
			AutoMigrate: autoMigrate,
			// busy_timeout bounds how long the driver waits for the write lock
			// before returning SQLITE_BUSY up to WithTransaction's retry loop.
			// Small, so the contender reaches that loop promptly instead of
			// sitting in the driver.
			BusyTimeout: 20 * time.Millisecond,
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
	holder := newStore(true)
	if _, err := holder.CreateRootDirectory(ctx, share, &metadata.FileAttr{Mode: 0o755}); err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", share, err)
	}
	// Second connection to the SAME file, so the two genuinely collide on the
	// sqlite write lock rather than queueing behind one pool.
	contender := newStore(false)

	// Resolved before either transaction opens: each store pins MaxOpenConns(1),
	// so a store call made from inside its own transaction would wait forever for
	// the connection that transaction is holding.
	root, err := holder.GetRootHandle(ctx, share)
	if err != nil {
		t.Fatalf("GetRootHandle(%q): %v", share, err)
	}

	touchRoot := func(tx metadata.Transaction) error {
		f, err := tx.GetFile(ctx, root)
		if err != nil {
			return err
		}
		f.Mtime = time.Now()
		return tx.UpdateAttrs(ctx, f)
	}

	var (
		locked    = make(chan struct{}) // holder owns the write lock
		release   = make(chan struct{}) // holder may commit
		done      = make(chan struct{}) // holder returned; holderErr readable
		holderErr error
	)
	// Both callbacks re-run on every retry, and the holder retries too if its own
	// commit collides, so each close happens at most once.
	var lockedOnce, releaseOnce sync.Once
	signalLocked := func() { lockedOnce.Do(func() { close(locked) }) }
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	go func() {
		defer close(done)
		holderErr = holder.WithTransaction(ctx, func(tx metadata.Transaction) error {
			if err := touchRoot(tx); err != nil {
				return err
			}
			signalLocked()
			<-release
			return nil
		})
	}()

	select {
	case <-locked:
	case <-done:
		t.Fatalf("holder never took the write lock: %v", holderErr)
	}

	attempts := 0
	began := time.Now()
	contendErr := contender.WithTransaction(ctx, func(tx metadata.Transaction) error {
		attempts++
		// Past the old ceiling the loop has already proven it backpressures; free
		// the lock so this run terminates on the retry loop's own behaviour rather
		// than on a timer.
		if attempts > fixedAttemptCount {
			doRelease()
		}
		return touchRoot(tx)
	})
	blocked := time.Since(began)
	// However the contender ended — retried enough, or gave up — the holder must
	// not be left blocked.
	doRelease()
	<-done

	if holderErr != nil {
		t.Fatalf("holder transaction failed: %v", holderErr)
	}
	if contendErr != nil {
		var se *metadata.StoreError
		if errors.As(contendErr, &se) && se.Code == metadata.ErrIOError {
			t.Fatalf("write returned EIO after %d attempts in %v under contention (should backpressure, #1769): %v",
				attempts, blocked, contendErr)
		}
		t.Fatalf("unexpected error under contention: %v", contendErr)
	}
	if attempts <= fixedAttemptCount {
		t.Fatalf("contending write took %d attempts, never exercised the retry path past the fixed %d-attempt ceiling",
			attempts, fixedAttemptCount)
	}
	t.Logf("contending write succeeded after %d attempts blocked for %v", attempts, blocked)
}
