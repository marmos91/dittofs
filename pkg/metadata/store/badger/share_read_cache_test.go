package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// createShareRoot registers a share by creating its root directory, the only
// entry point that records one.
func createShareRoot(tb testing.TB, store *BadgerMetadataStore, shareName string) {
	tb.Helper()
	_, err := store.CreateRootDirectory(context.Background(), shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	require.NoError(tb, err)
}

func BenchmarkGetShareOptions_Cached(b *testing.B) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, filepath.Join(b.TempDir(), "metadata.db"))
	require.NoError(b, err)
	defer func() { _ = store.Close() }()
	createShareRoot(b, store, "s1")
	require.NoError(b, store.UpdateShareOptions(ctx, "s1",
		&metadata.ShareOptions{ReadOnly: true}))
	_, _ = store.GetShareOptions(ctx, "s1") // warm the cache

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.GetShareOptions(ctx, "s1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetShareOptions_Uncached forces a cache miss each iteration to
// measure the badger View + decode cost the cache eliminates.
func BenchmarkGetShareOptions_Uncached(b *testing.B) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, filepath.Join(b.TempDir(), "metadata.db"))
	require.NoError(b, err)
	defer func() { _ = store.Close() }()
	createShareRoot(b, store, "s1")
	require.NoError(b, store.UpdateShareOptions(ctx, "s1",
		&metadata.ShareOptions{ReadOnly: true}))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.shareCache.Invalidate("s1")
		if _, err := store.GetShareOptions(ctx, "s1"); err != nil {
			b.Fatal(err)
		}
	}
}
