package badger

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
)

// TestBadgerWriteConflictMechanism validates the mechanism behind the #1573
// fix: badger's SSI detects READ conflicts, not write-write. So a create that
// only blind-Sets a shared key (no Get of it) can run concurrently without the
// conflict-retry storm that read+write of the same key triggers.
//
//	(a) Get(K)+Set(K) concurrent same key  => conflicts (the current create bug)
//	(b) Set(K) only, concurrent same key    => no conflicts (the fix)
//	(c) disjoint keys                        => no conflicts (control)
func TestBadgerWriteConflictMechanism(t *testing.T) {
	if testing.Short() {
		t.Skip("mechanism probe; skipped under -short")
	}
	const workers, iters = 16, 100
	key := []byte("shared")

	run := func(fn func(txn *badgerdb.Txn, w, i int) error) int64 {
		opts := badgerdb.DefaultOptions(t.TempDir()).WithLoggingLevel(badgerdb.ERROR).WithSyncWrites(true)
		db, err := badgerdb.Open(opts)
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		require.NoError(t, db.Update(func(txn *badgerdb.Txn) error { return txn.Set(key, []byte("0")) }))

		var conflicts int64
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					// single attempt (no retry) so we count raw conflicts
					if err := db.Update(func(txn *badgerdb.Txn) error { return fn(txn, w, i) }); err == badgerdb.ErrConflict {
						atomic.AddInt64(&conflicts, 1)
					}
				}
			}(w)
		}
		wg.Wait()
		return conflicts
	}

	// (a) read-then-write the shared key — the current createEntry pattern.
	getSet := run(func(txn *badgerdb.Txn, w, i int) error {
		if _, err := txn.Get(key); err != nil {
			return err
		}
		return txn.Set(key, []byte(fmt.Sprintf("%d-%d", w, i)))
	})

	// (b) blind-write the shared key, no Get — the fix.
	setOnly := run(func(txn *badgerdb.Txn, w, i int) error {
		return txn.Set(key, []byte(fmt.Sprintf("%d-%d", w, i)))
	})

	// (c) disjoint keys — control.
	disjoint := run(func(txn *badgerdb.Txn, w, i int) error {
		return txn.Set([]byte(fmt.Sprintf("k-%d-%d", w, i)), []byte("v"))
	})

	total := int64(workers * iters)
	t.Logf("(a) Get+Set same key: %d/%d conflicts", getSet, total)
	t.Logf("(b) Set-only same key: %d/%d conflicts", setOnly, total)
	t.Logf("(c) disjoint keys:     %d/%d conflicts", disjoint, total)

	require.Greater(t, getSet, int64(0), "expected read+write of a hot key to conflict (the bug)")
	require.Zero(t, setOnly, "blind Set of a shared key must not conflict (the fix mechanism)")
	require.Zero(t, disjoint, "disjoint writes must not conflict")
}
