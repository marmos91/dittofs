package metadata_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// TestParentMtimeContention guards the invariant that concurrent same-directory
// CREATEs touch only the new child's disjoint keys and never the parent inode.
// If a parent mtime/ctime bump is reintroduced into the create transaction,
// every concurrent same-dir create reads+writes one shared key, which BadgerDB's
// SSI aborts as a conflict; the retry loop then serializes them on backoff.
//
// Rather than time that serialization (a wall-clock ratio that swings with
// runner load), the guard reads the store's SSI conflict counter directly: a
// healthy build commits every same-dir create first-try, so the counter stays
// at zero regardless of disk speed, while a reintroduced shared-key write drives
// it into the hundreds. Throughput is logged for diagnostics only.
// Run: go test ./pkg/metadata -run TestParentMtimeContention -v
func TestParentMtimeContention(t *testing.T) {
	if testing.Short() {
		t.Skip("perf diagnostic; skipped under -short")
	}

	const (
		concurrency = 16
		perWorker   = 40
	)

	// newSvc builds a fresh store (its conflict counter starts at zero) plus the
	// Service and root handle wired around it. The concrete badger store is
	// returned so the test can read the conflict counter after a workload.
	newSvc := func(t *testing.T) (*badger.BadgerMetadataStore, *metadata.Service, metadata.FileHandle, *metadata.AuthContext) {
		ctx := context.Background()
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })

		const share = "/test"
		root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory, Mode: 0o777,
		})
		require.NoError(t, err)
		rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
		require.NoError(t, err)

		svc := metadata.New()
		require.NoError(t, svc.RegisterStoreForShare(share, svc0store(store)))
		auth := &metadata.AuthContext{
			Context: ctx, AuthMethod: "unix",
			Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
			ClientAddr: "127.0.0.1",
		}
		return store, svc, rootHandle, auth
	}

	fileAttr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}

	// Case A: concurrent creates, ALL in the same dir. A shared parent-inode write
	// would make every one of these conflict on one key.
	sameDir := func() (float64, int64) {
		store, svc, root, auth := newSvc(t)
		var done int64
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					_, _, err := svc.CreateFile(auth, root, fmt.Sprintf("same-%d-%d", w, i), fileAttr)
					if err == nil {
						atomic.AddInt64(&done, 1)
					}
				}
			}(w)
		}
		wg.Wait()
		require.Equal(t, int64(concurrency*perWorker), done)
		return float64(done) / time.Since(start).Seconds(), store.TransactionConflictsForTest()
	}

	// Case B (control): concurrent creates, each worker in its OWN dir — no shared
	// key. Proves the harness itself generates no spurious conflicts.
	distinctDir := func() (float64, int64) {
		store, svc, root, auth := newSvc(t)
		dirs := make([]metadata.FileHandle, concurrency)
		for w := 0; w < concurrency; w++ {
			d, _, err := svc.CreateDirectory(auth, root, fmt.Sprintf("d%d", w),
				&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
			require.NoError(t, err)
			dirs[w], err = metadata.EncodeShareHandle("/test", d.ID)
			require.NoError(t, err)
		}
		// Directory creates above bump the parent link count under a per-parent
		// lock and may legitimately conflict; measure only the file-create phase.
		baseline := store.TransactionConflictsForTest()
		var done int64
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					_, _, err := svc.CreateFile(auth, dirs[w], fmt.Sprintf("f-%d", i), fileAttr)
					if err == nil {
						atomic.AddInt64(&done, 1)
					}
				}
			}(w)
		}
		wg.Wait()
		require.Equal(t, int64(concurrency*perWorker), done)
		return float64(done) / time.Since(start).Seconds(), store.TransactionConflictsForTest() - baseline
	}

	sameOps, sameConflicts := sameDir()
	distinctOps, distinctConflicts := distinctDir()

	t.Logf("concurrent  (1 dir, %2d threads): %8.0f creates/s, %d SSI conflicts", concurrency, sameOps, sameConflicts)
	t.Logf("concurrent  (%2d dirs,%2d threads): %8.0f creates/s, %d SSI conflicts", concurrency, concurrency, distinctOps, distinctConflicts)

	// Regression guard for #1573: with the parent-inode bump coalesced out of the
	// create transaction, concurrent same-dir creates write only disjoint child
	// keys and commit first-try, so the SSI conflict counter stays at zero. A
	// reintroduced shared-key parent write makes every concurrent create contend
	// on one key, driving the counter into the hundreds. The bound sits far above
	// zero (absorbing any incidental abort) yet far below that regressed level.
	require.Less(t, sameConflicts, int64(concurrency),
		"concurrent same-dir creates are conflicting on a shared key; a parent-inode write was reintroduced into the create txn")
	require.Less(t, distinctConflicts, int64(concurrency),
		"distinct-dir creates should never conflict; the create path touched an unexpected shared key")
}

// svc0store adapts *badger.BadgerMetadataStore to the metadata.Store interface
// the Service expects. It exists only so the test compiles against whatever the
// registration method takes; the concrete store already satisfies the interface.
func svc0store(s *badger.BadgerMetadataStore) metadata.Store { return s }
