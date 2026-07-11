package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// newShareCacheStore spins up a throwaway badger store for the share-cache tests.
func newShareCacheStore(t *testing.T) *BadgerMetadataStore {
	t.Helper()
	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestShareCache_NoStalePermissionAfterUpdate proves the cache never serves a
// stale permission decision: an UpdateShareOptions after a populating read must
// be reflected on the next GetShareOptions. A miss here is a security bug.
func TestShareCache_NoStalePermissionAfterUpdate(t *testing.T) {
	ctx := context.Background()
	store := newShareCacheStore(t)

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{
		Name:    "s1",
		Options: metadata.ShareOptions{ReadOnly: false},
	}))

	// Populate the cache and confirm the pre-update value.
	got, err := store.GetShareOptions(ctx, "s1")
	require.NoError(t, err)
	require.False(t, got.ReadOnly)

	// Flip ReadOnly true; the cached false must not survive.
	require.NoError(t, store.UpdateShareOptions(ctx, "s1", &metadata.ShareOptions{ReadOnly: true}))
	got, err = store.GetShareOptions(ctx, "s1")
	require.NoError(t, err)
	require.True(t, got.ReadOnly, "cache served a stale ReadOnly=false after update")

	// Same guarantee for a reference-bearing slice field.
	require.NoError(t, store.UpdateShareOptions(ctx, "s1", &metadata.ShareOptions{
		ReadOnly:       true,
		AllowedClients: []string{"10.0.0.0/8"},
	}))
	got, err = store.GetShareOptions(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.0/8"}, got.AllowedClients)
}

// TestShareCache_InvalidateOnDelete proves a deleted share is not still served
// from the cache.
func TestShareCache_InvalidateOnDelete(t *testing.T) {
	ctx := context.Background()
	store := newShareCacheStore(t)

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: "s1"}))
	_, err := store.GetShareOptions(ctx, "s1") // populate
	require.NoError(t, err)

	require.NoError(t, store.DeleteShare(ctx, "s1"))
	_, err = store.GetShareOptions(ctx, "s1")
	require.Error(t, err, "cache served options for a deleted share")
}

// TestShareCache_CopySafety proves the returned *ShareOptions is a deep copy:
// mutating it (including appending to its slice) cannot corrupt the cache.
func TestShareCache_CopySafety(t *testing.T) {
	ctx := context.Background()
	store := newShareCacheStore(t)

	sid := "S-1-5-7"
	uid := uint32(65534)
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{
		Name: "s1",
		Options: metadata.ShareOptions{
			AllowedClients:  []string{"192.168.1.0/24"},
			IdentityMapping: &metadata.IdentityMapping{AnonymousUID: &uid, AnonymousSID: &sid},
		},
	}))

	first, err := store.GetShareOptions(ctx, "s1") // populate
	require.NoError(t, err)

	// Vandalize the returned copy every way a caller could.
	first.ReadOnly = true
	first.AllowedClients = append(first.AllowedClients, "0.0.0.0/0")
	first.AllowedClients[0] = "mutated"
	*first.IdentityMapping.AnonymousUID = 0
	*first.IdentityMapping.AnonymousSID = "mutated"

	second, err := store.GetShareOptions(ctx, "s1")
	require.NoError(t, err)
	require.False(t, second.ReadOnly)
	require.Equal(t, []string{"192.168.1.0/24"}, second.AllowedClients)
	require.Equal(t, uint32(65534), *second.IdentityMapping.AnonymousUID)
	require.Equal(t, "S-1-5-7", *second.IdentityMapping.AnonymousSID)
}

// TestShareCache_TxUpdateInvalidates covers the withTransaction dirtyShares
// hook: an UpdateShareOptions run through the transaction path must invalidate
// the cache just like the direct db.Update path.
func TestShareCache_TxUpdateInvalidates(t *testing.T) {
	ctx := context.Background()
	store := newShareCacheStore(t)

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: "s1"}))
	got, err := store.GetShareOptions(ctx, "s1") // populate
	require.NoError(t, err)
	require.False(t, got.ReadOnly)

	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.(*badgerTransaction).UpdateShareOptions(ctx, "s1", &metadata.ShareOptions{ReadOnly: true})
	}))

	got, err = store.GetShareOptions(ctx, "s1")
	require.NoError(t, err)
	require.True(t, got.ReadOnly, "tx-path update did not invalidate the cache")
}

func BenchmarkGetShareOptions_Cached(b *testing.B) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, filepath.Join(b.TempDir(), "metadata.db"))
	require.NoError(b, err)
	defer func() { _ = store.Close() }()
	require.NoError(b, store.CreateShare(ctx, &metadata.Share{
		Name:    "s1",
		Options: metadata.ShareOptions{AllowedClients: []string{"10.0.0.0/8"}},
	}))
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
	require.NoError(b, store.CreateShare(ctx, &metadata.Share{
		Name:    "s1",
		Options: metadata.ShareOptions{AllowedClients: []string{"10.0.0.0/8"}},
	}))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.shareCache.invalidate("s1")
		if _, err := store.GetShareOptions(ctx, "s1"); err != nil {
			b.Fatal(err)
		}
	}
}
