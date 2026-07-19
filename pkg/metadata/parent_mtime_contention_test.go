package metadata_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// TestParentMtimeContention reproduces #1573: concurrent same-directory CREATEs
// serialize because every create reads+writes the parent inode (mtime/ctime/atime
// bump in createEntry), which badger's SSI aborts as a conflict, then the retry
// loop sleeps 1-5ms/attempt. Distinct-directory creates share no key and scale.
//
// Absolute ops/s depend on local fsync latency (not comparable to the SCW bench);
// the SAME-dir vs DISTINCT-dir RATIO is the disk-speed-independent fingerprint.
// Run: go test ./pkg/metadata -run TestParentMtimeContention -v
func TestParentMtimeContention(t *testing.T) {
	if testing.Short() {
		t.Skip("perf diagnostic; skipped under -short")
	}

	const (
		concurrency = 16
		perWorker   = 40
	)

	newSvc := func(t *testing.T) (*metadata.Service, metadata.FileHandle, *metadata.AuthContext) {
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
		return svc, rootHandle, auth
	}

	fileAttr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}

	// Case A: sequential creates in one dir — the single-thread fsync floor.
	seqOps := func() float64 {
		svc, root, auth := newSvc(t)
		n := concurrency * perWorker
		start := time.Now()
		for i := 0; i < n; i++ {
			_, _, err := svc.CreateFile(auth, root, fmt.Sprintf("seq-%d", i), fileAttr)
			require.NoError(t, err)
		}
		return float64(n) / time.Since(start).Seconds()
	}()

	// Case B: concurrent creates, ALL in the same dir — shared parent key => SSI conflict.
	sameDirOps := func() float64 {
		svc, root, auth := newSvc(t)
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
		return float64(done) / time.Since(start).Seconds()
	}

	// Case C: concurrent creates, each worker in its OWN dir — no shared key.
	distinctDirOps := func() float64 {
		svc, root, auth := newSvc(t)
		dirs := make([]metadata.FileHandle, concurrency)
		for w := 0; w < concurrency; w++ {
			d, _, err := svc.CreateDirectory(auth, root, fmt.Sprintf("d%d", w),
				&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
			require.NoError(t, err)
			dirs[w], err = metadata.EncodeShareHandle("/test", d.ID)
			require.NoError(t, err)
		}
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
		return float64(done) / time.Since(start).Seconds()
	}

	// A single trial's ratio swings with runner scheduling — one stall in the
	// same-dir phase artificially depresses it. Take the median of a few trials
	// so a lone noisy sample can't trip the guard, while a genuine reintroduced
	// shared-key write (which depresses every trial) still fails.
	const trials = 3
	ratios := make([]float64, trials)
	var lastSame, lastDistinct float64
	for i := range ratios {
		lastSame = sameDirOps()
		lastDistinct = distinctDirOps()
		ratios[i] = lastSame / lastDistinct
	}
	sort.Float64s(ratios)
	ratio := ratios[trials/2]

	t.Logf("sequential  (1 dir, 1 thread):   %8.0f creates/s", seqOps)
	t.Logf("concurrent  (1 dir, %2d threads): %8.0f creates/s", concurrency, lastSame)
	t.Logf("concurrent  (%2d dirs,%2d threads): %8.0f creates/s", concurrency, concurrency, lastDistinct)
	t.Logf("same-dir / distinct-dir ratio (median of %d): %.2f   (was ~0.5 pre-#1573)", trials, ratio)

	// Regression guard for #1573: with the parent-inode bump coalesced out of the
	// create transaction, same-dir concurrent creates must no longer be penalized
	// vs distinct-dir (they were ~0.5x before, walled by SSI conflict-retry). The
	// floor must sit between that regressed ~0.5x and the scheduler noise a loaded
	// CI runner adds to a healthy run (observed as low as ~0.69). 0.6 catches a
	// reintroduced shared-key write while absorbing that noise.
	require.Greater(t, ratio, 0.6,
		"same-dir concurrent creates are contention-bound again; a parent-inode write was reintroduced into the create txn (#1573)")
}

// svc0store adapts *badger.BadgerMetadataStore to the metadata.Store interface
// the Service expects. It exists only so the test compiles against whatever the
// registration method takes; the concrete store already satisfies the interface.
func svc0store(s *badger.BadgerMetadataStore) metadata.Store { return s }
