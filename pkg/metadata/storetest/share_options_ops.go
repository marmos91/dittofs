package storetest

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// errRollbackShareOptions aborts a transaction after it wrote share options, so
// the rollback case can prove the write never becomes visible.
var errRollbackShareOptions = errors.New("rollback share options")

// runShareOptionsOps pins the freshness of GetShareOptions. It is the read every
// permission check funnels through, so backends cache it — and a cached entry
// that outlives the write that superseded it is a wrong permission decision, not
// a stale display value. Each case reads first (populating whatever cache the
// backend keeps), writes, then reads again.
func runShareOptionsOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("UpdateIsVisibleToNextRead", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		require.NoError(t, store.CreateShare(ctx, &metadata.Share{
			Name:    "/opts-update",
			Options: metadata.ShareOptions{ReadOnly: false},
		}))

		got, err := store.GetShareOptions(ctx, "/opts-update")
		require.NoError(t, err)
		require.False(t, got.ReadOnly)

		require.NoError(t, store.UpdateShareOptions(ctx, "/opts-update",
			&metadata.ShareOptions{ReadOnly: true}))

		got, err = store.GetShareOptions(ctx, "/opts-update")
		require.NoError(t, err)
		require.True(t, got.ReadOnly, "read served a stale ReadOnly=false after the update")

		// A reference-bearing field must refresh too, not just the scalar.
		require.NoError(t, store.UpdateShareOptions(ctx, "/opts-update", &metadata.ShareOptions{
			ReadOnly:       true,
			AllowedClients: []string{"10.0.0.0/8"},
		}))
		got, err = store.GetShareOptions(ctx, "/opts-update")
		require.NoError(t, err)
		require.Equal(t, []string{"10.0.0.0/8"}, got.AllowedClients)
	})

	t.Run("TxUpdateIsVisibleToNextRead", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: "/opts-tx"}))

		got, err := store.GetShareOptions(ctx, "/opts-tx")
		require.NoError(t, err)
		require.False(t, got.ReadOnly)

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			return tx.UpdateShareOptions(ctx, "/opts-tx", &metadata.ShareOptions{ReadOnly: true})
		}))

		got, err = store.GetShareOptions(ctx, "/opts-tx")
		require.NoError(t, err)
		require.True(t, got.ReadOnly, "read served a stale value after a transactional update")
	})

	t.Run("DeletedShareIsNotServed", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: "/opts-delete"}))

		_, err := store.GetShareOptions(ctx, "/opts-delete")
		require.NoError(t, err)

		require.NoError(t, store.DeleteShare(ctx, "/opts-delete"))

		_, err = store.GetShareOptions(ctx, "/opts-delete")
		require.Error(t, err, "read served options for a deleted share")
	})

	t.Run("RolledBackUpdateIsNotVisible", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: "/opts-rollback"}))

		_, err := store.GetShareOptions(ctx, "/opts-rollback")
		require.NoError(t, err)

		wantErr := errRollbackShareOptions
		err = store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			if err := tx.UpdateShareOptions(ctx, "/opts-rollback",
				&metadata.ShareOptions{ReadOnly: true}); err != nil {
				return err
			}
			return wantErr
		})
		require.ErrorIs(t, err, wantErr)

		got, err := store.GetShareOptions(ctx, "/opts-rollback")
		require.NoError(t, err)
		require.False(t, got.ReadOnly, "a rolled-back update became visible")
	})
}
