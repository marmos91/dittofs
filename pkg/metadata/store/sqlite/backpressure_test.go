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
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "backpressure.db")

	newStore := func(autoMigrate bool) metadata.Store {
		t.Helper()
		cfg := &sqlite.SQLiteMetadataStoreConfig{
			Path:        dbPath,
			AutoMigrate: autoMigrate,
			// A tiny busy_timeout makes colliding writers surface SQLITE_BUSY
			// almost immediately instead of queueing inside the driver — that is
			// exactly the transient conflict WithTransaction must backpressure
			// over, not EIO. With the pre-fix 3-attempt / 10-20-30ms budget this
			// reliably EIO'd under cross-connection contention.
			BusyTimeout: 1 * time.Millisecond,
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
					err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
						// Read + rewrite the SAME parent-directory inode: a genuine
						// hot-row write conflict across all connections.
						f, err := tx.GetFile(ctx, rootHandle)
						if err != nil {
							return err
						}
						f.Mtime = time.Now()
						return tx.PutFile(ctx, f)
					})
					if err != nil {
						errCh <- err
						return
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
}
