package badger_test

import (
	"context"
	"path/filepath"
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
