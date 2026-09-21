package badger

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// BenchmarkScanUsage measures the file scan that derives the usage buckets. It
// decodes every file row, which is what a store pays on the first open after
// upgrade and on every operator-invoked realign.
func BenchmarkScanUsage(b *testing.B) {
	const files = 50000
	ctx := context.Background()
	store := newSizeTestStore(b)
	createShareRoot(b, store, "bench")

	for i := 0; i < files; i++ {
		path := fmt.Sprintf("/f%06d", i)
		h, err := store.GenerateHandle(ctx, "bench", path)
		require.NoError(b, err)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(b, err)
		f := &metadata.File{
			ShareName: "bench",
			Path:      path,
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular, Mode: 0o600, UID: uint32(i % 32), GID: uint32(i % 8),
				PayloadID: metadata.PayloadID(fmt.Sprintf("payload-%06d", i)), Size: uint64(i),
			},
		}
		f.ID = id
		require.NoError(b, store.UpdateAttrs(ctx, f))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.scanUsage(nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
}
