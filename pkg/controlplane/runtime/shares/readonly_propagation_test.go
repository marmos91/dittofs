package shares

import (
	"context"
	"testing"

	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestAddShare_ReadOnlyPropagatesToShareOptions asserts that a genuinely
// read-only share (ShareConfig.ReadOnly == true) has its flag propagated into
// the metadata store's ShareOptions.
//
// The runtime initialises a share's metadata via CreateRootDirectory (not
// CreateShare), which leaves ShareOptions zero-valued. Without propagation the
// permission layer never sees ShareOptions.ReadOnly at runtime, so a store-level
// read-only share is indistinguishable from a per-user read-only level
// (ctx.ShareReadOnly, which is true for BOTH). That collapse made a read-only
// share's create/write denials surface as EACCES instead of EROFS. This test
// locks the store-level discriminator at its source: the flag must reach
// GetShareOptions so checkPermission / CheckParentCreateAccess can return
// ErrReadOnly (NFS3ERR_ROFS) for a genuine read-only share.
func TestAddShare_ReadOnlyPropagatesToShareOptions(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		share string
		val   bool
	}{
		{"read_only_share_propagates", "/archive", true},
		{"read_write_share_stays_false", "/scratch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mds := metamem.NewMemoryMetadataStoreWithDefaults()
			t.Cleanup(func() { _ = mds.Close() })

			svc := New()
			cfg := &ShareConfig{
				Name:          tc.share,
				MetadataStore: "meta-test",
				Enabled:       true,
				ReadOnly:      tc.val,
			}

			err := svc.AddShare(
				ctx,
				cfg,
				&metaStoreProvider{name: "meta-test", store: mds},
				metaSvcRegistrar{},
				nil, // no block store provider — LocalBlockStoreID empty skips the path
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("AddShare: %v", err)
			}

			opts, err := mds.GetShareOptions(ctx, tc.share)
			if err != nil {
				t.Fatalf("GetShareOptions: %v", err)
			}
			if opts.ReadOnly != tc.val {
				t.Errorf("ShareOptions.ReadOnly=%v, want %v", opts.ReadOnly, tc.val)
			}
		})
	}
}
