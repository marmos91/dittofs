package badger

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// BenchmarkInitUsedBytesCounter measures the open-time file scan that seeds the
// usage cache. It decodes every file row, so it is the second of the two
// file-count-proportional steps a share start pays before it becomes visible.
func BenchmarkInitUsedBytesCounter(b *testing.B) {
	const files = 50000
	ctx := context.Background()
	store := newSizeTestStore(b)
	require.NoError(b, store.CreateShare(ctx, &metadata.Share{Name: "bench"}))

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
		if err := store.initUsedBytesCounter(nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
}
