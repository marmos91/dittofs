package badger

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata"
)

// BenchmarkGetFileChunkAtOffset compares a covered read with a hole after the
// last chunk. Both queries see the same manifest, so increasing its size
// exposes work repeated for each rejected covering candidate.
func BenchmarkGetFileChunkAtOffset(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("chunks=%d", count), func(b *testing.B) {
			ctx := b.Context()
			s := newSizeTestStore(b)
			if err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
				for i := range count {
					if err := tx.Put(ctx, &metadata.FileChunk{
						ID:       fmt.Sprintf("lookup-bench/%d", i*4096),
						DataSize: 4096,
					}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}

			for _, query := range []struct {
				name    string
				off     uint64
				covered bool
			}{
				{"dense-hit", uint64((count-1)*4096 + 2048), true},
				{"sparse-tail", uint64(count*4096 + 1), false},
			} {
				b.Run(query.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						row, err := s.GetFileChunkAtOffset(ctx, "lookup-bench", query.off)
						if err != nil || (row != nil) != query.covered {
							b.Fatalf("GetFileChunkAtOffset(%d) = %v, %v", query.off, row, err)
						}
					}
				})
			}
		})
	}
}
