package badger_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// BenchmarkNamespaceCommit measures serial single-thread namespace-commit
// throughput — the create/remove/rename hot path (#1573 Wall 1). Both sub-benchmarks
// run the SAME code a create runs (WithTransactionRelaxed committing a PutFile);
// only the store's durability config differs, so the delta is purely the
// per-commit fsync the strict store still pays and the relaxed store defers.
//
// NOTE: on macOS the badger fsync is cheap (Darwin's fsync is not a full
// barrier), so the local ratio understates the win. On a Linux disk that honors
// fsync (the bench VM) the strict path is fsync-bound at ~168 commits/s and the
// relaxed path runs at memtable speed — the headline Wall 1 improvement.
func BenchmarkNamespaceCommit(b *testing.B) {
	for _, tc := range []struct {
		name    string
		relaxed bool
	}{
		{"strict", false},
		{"relaxed", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			dbPath := filepath.Join(b.TempDir(), "metadata.db")
			store, err := badger.NewBadgerMetadataStore(context.Background(), badger.BadgerMetadataStoreConfig{
				DBPath:            dbPath,
				RelaxedDurability: tc.relaxed,
			})
			require.NoError(b, err)
			b.Cleanup(func() { _ = store.Close() })

			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f := &metadata.File{
					ID:        uuid.New(),
					ShareName: "bench",
					Path:      "/f",
					FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644},
				}
				// Same call the create path uses: relaxed in relaxed mode,
				// fsync-per-commit in strict mode (SyncWrites=true).
				if err := store.WithTransactionRelaxed(ctx, func(tx metadata.Transaction) error {
					return tx.PutFile(ctx, f)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestRelaxedDurability_ConcurrentDurableCommitsCoalesce proves the DURABLE
// store path (WithTransaction in relaxed mode → syncIfRelaxed → the group-commit
// leader) actually routes through the leader under concurrency (#1573). It fires
// N durable commits at once behind a start barrier and asserts the leader ran
// (drainPasses advanced) and never issued MORE barrier passes than there were
// commits — i.e. concurrent bursts coalesce, never amplify. The exact ratio is
// timing-dependent (Darwin's cheap fsync serializes more than a real disk), so
// the deterministic coalescing proof lives in commit_leader_test.go; this guards
// the real durable path against silently bypassing the leader.
func TestRelaxedDurability_ConcurrentDurableCommitsCoalesce(t *testing.T) {
	const submissions = 32

	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStore(context.Background(), badger.BadgerMetadataStoreConfig{
		DBPath:            dbPath,
		RelaxedDurability: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	before := store.DrainPassCountForTest()

	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(submissions)
	for i := 0; i < submissions; i++ {
		go func(i int) {
			defer done.Done()
			f := &metadata.File{
				ID:        uuid.New(),
				ShareName: "bench",
				Path:      fmt.Sprintf("/f%d", i),
				FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644},
			}
			start.Wait() // release all goroutines together to maximize overlap
			require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
				return tx.PutFile(ctx, f)
			}))
		}(i)
	}
	start.Done()
	done.Wait()

	passes := store.DrainPassCountForTest() - before
	require.Positive(t, passes, "durable commits must route through the group-commit leader")
	require.LessOrEqual(t, passes, int64(submissions),
		"leader must never issue more barrier passes than there were commits")
	t.Logf("%d concurrent durable commits coalesced onto %d barrier passes", submissions, passes)
}
