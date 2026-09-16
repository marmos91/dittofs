package nfs

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// TestNSMClientStore_ResolvedForShareAddedAfterStartup covers the
// zero-eligible-share-at-start ordering: the adapter initialises NSM before any
// share exists, so the one-shot scan over rt.ListShares() finds no store that
// can persist client registrations. A share added later through the runtime API
// must still be picked up, otherwise every SM_MON registration for the rest of
// the adapter's life is memory-only and no SM_NOTIFY is sent after a restart.
func TestNSMClientStore_ResolvedForShareAddedAfterStartup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rt, bsID := newTestShareRuntime(t)
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, rt.RegisterMetadataStore("test-meta", metaStore))

	s := &NFSAdapter{BaseAdapter: &adapter.BaseAdapter{Registry: rt}}
	t.Cleanup(func() {
		for _, unsub := range s.shareUnsubscribers {
			unsub()
		}
	})

	// No shares yet: nothing can supply a registration store.
	s.initNSMHandler(rt, rt.GetMetadataService())
	require.Nil(t, s.nsmHandler.GetClientStore(),
		"no share exists, so no registration store should have been resolved")

	require.NoError(t, rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              "/late",
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		Enabled:           true,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}))

	require.NotNil(t, s.nsmHandler.GetClientStore(),
		"a share added after startup must supply the NSM client registration store")
}
