package badger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// newShareOptionsStore opens a store holding one share whose options carry every
// reference-bearing field the share cache has to defend.
func newShareOptionsStore(t *testing.T) (*BadgerMetadataStore, string) {
	t.Helper()
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const shareName = "/opts"
	anonUID := uint32(65534)
	_, err = store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateShareOptions(ctx, shareName, &metadata.ShareOptions{
		AllowedClients:     []string{"10.0.0.0/8"},
		DeniedClients:      []string{"10.1.2.3"},
		AllowedAuthMethods: []string{"sys"},
		IdentityMapping:    &metadata.IdentityMapping{AnonymousUID: &anonUID},
	}))
	return store, shareName
}

// TestGetShareOptions_CallerCannotMutateCachedEntry is the mutation-safety guard
// on the permission hot path. GetShareOptions serves the share cache, and the
// options it returns decide access; if a caller's write reached the shared entry
// it would silently change the permission answer given to every later caller of
// that share. Every reference-bearing field has to be copied, not aliased.
// Both paths out of GetShareOptions have to defend the entry: the populate path
// hands back the very value it just cached, and the cache-hit path hands back
// the stored one.
func TestGetShareOptions_CallerCannotMutateCachedEntry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		warmUpFirst bool
	}{
		{"populate path", false},
		{"cache-hit path", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, shareName := newShareOptionsStore(t)
			ctx := context.Background()

			if tc.warmUpFirst {
				_, err := store.GetShareOptions(ctx, shareName)
				require.NoError(t, err)
			}

			got, err := store.GetShareOptions(ctx, shareName)
			require.NoError(t, err)

			// A caller does what callers do with a value they believe they own.
			got.ReadOnly = true
			got.AllowedClients[0] = "0.0.0.0/0"
			got.DeniedClients = append(got.DeniedClients, "192.168.0.1")
			got.AllowedAuthMethods[0] = "none"
			*got.IdentityMapping.AnonymousUID = 0

			after, err := store.GetShareOptions(ctx, shareName)
			require.NoError(t, err)

			require.False(t, after.ReadOnly, "a caller's write reached the cached share entry")
			require.Equal(t, []string{"10.0.0.0/8"}, after.AllowedClients,
				"AllowedClients decides access and must not alias the cache")
			require.Equal(t, []string{"10.1.2.3"}, after.DeniedClients)
			require.Equal(t, []string{"sys"}, after.AllowedAuthMethods)
			require.Equal(t, uint32(65534), *after.IdentityMapping.AnonymousUID,
				"the anonymous UID squash target must not alias the cache")
		})
	}
}

// TestGetShareOptions_PopulatesCache pins the populate half: a first read must
// leave the decoded options in the share cache, or the permission funnel pays a
// badger View txn plus a record decode on every single operation.
func TestGetShareOptions_PopulatesCache(t *testing.T) {
	store, shareName := newShareOptionsStore(t)
	ctx := context.Background()

	// CreateShare invalidates the entry, so the cache starts cold.
	_, cached := store.shareCache.Get(shareName)
	require.False(t, cached, "cache should be cold before the first read")

	_, err := store.GetShareOptions(ctx, shareName)
	require.NoError(t, err)

	_, cached = store.shareCache.Get(shareName)
	require.True(t, cached, "a completed read must populate the share cache")
}
