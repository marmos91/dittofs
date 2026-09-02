package storetest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runShareDeleteQuotaOps pins that deleting a share releases the per-identity
// usage its files held.
//
// The usage cache is in memory and is only ever adjusted by deltas, so a delete
// that removes the rows without recording the release leaves the store
// charging every owner for files that no longer exist — for the lifetime of
// the process, and invisibly, since nothing recomputes it.
//
// Two owners, and a distinct uid and gid per owner: a delete that collects the
// uid grouping but not the gid one (or the reverse) reads as correct if both
// scopes name the same identity.
func runShareDeleteQuotaOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("DeleteReleasesPerIdentityUsage", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		root := createTestShare(t, store, "/quota-del")
		createTestFileOwned(t, store, "/quota-del", root, "a.txt", 1001, 2001, 4096)
		createTestFileOwned(t, store, "/quota-del", root, "b.txt", 1002, 2002, 8192)

		for _, c := range []struct {
			scope metadata.QuotaScope
			id    uint32
		}{
			{metadata.QuotaScopeUser, 1001},
			{metadata.QuotaScopeUser, 1002},
			{metadata.QuotaScopeGroup, 2001},
			{metadata.QuotaScopeGroup, 2002},
		} {
			used, err := store.GetQuotaUsage("/quota-del", c.scope, c.id)
			require.NoError(t, err)
			require.NotZero(t, used.Bytes,
				"scope %v id %d holds no usage before the delete, so releasing it cannot be observed",
				c.scope, c.id)
		}

		require.NoError(t, store.DeleteShare(ctx, "/quota-del"))

		for _, c := range []struct {
			scope metadata.QuotaScope
			id    uint32
		}{
			{metadata.QuotaScopeUser, 1001},
			{metadata.QuotaScopeUser, 1002},
			{metadata.QuotaScopeGroup, 2001},
			{metadata.QuotaScopeGroup, 2002},
		} {
			used, err := store.GetQuotaUsage("/quota-del", c.scope, c.id)
			require.NoError(t, err)
			require.Zero(t, used.Bytes,
				"scope %v id %d still charged %d bytes after its share was deleted",
				c.scope, c.id, used.Bytes)
			require.Zero(t, used.Files,
				"scope %v id %d still charged %d files after its share was deleted",
				c.scope, c.id, used.Files)
		}
	})
}
