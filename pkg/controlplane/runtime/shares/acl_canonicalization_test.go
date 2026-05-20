package shares

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// metaStoreProvider returns a single in-memory metadata store registered
// against a fixed name. Used to drive AddShare end-to-end without a full
// runtime composition.
type metaStoreProvider struct {
	name  string
	store metadata.MetadataStore
}

func (p *metaStoreProvider) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	return p.store, nil
}

// metaSvcRegistrar implements MetadataServiceRegistrar with a no-op so
// AddShare can complete without standing up the real metadata service.
type metaSvcRegistrar struct{}

func (metaSvcRegistrar) RegisterStoreForShare(shareName string, store metadata.MetadataStore) error {
	return nil
}

// TestAddShare_AclFlagInheritedCanonicalization_Propagates — T1 of #514.
//
// AddShare must copy the toggle from ShareConfig onto the runtime Share so
// downstream SMB CREATE / SET_INFO Security handlers (T3/T4) can read it
// without re-querying the DB. Both directions of the boolean must propagate
// — explicitly `true` and explicitly `false` — to avoid silently coercing
// values during the model→config→runtime hand-off.
func TestAddShare_AclFlagInheritedCanonicalization_Propagates(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		share string
		val   bool
	}{
		{"true_propagates", "/acl-on", true},
		{"false_propagates_samba_opt_out", "/acl-off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mds := metamem.NewMemoryMetadataStoreWithDefaults()
			t.Cleanup(func() { _ = mds.Close() })

			svc := New()
			cfg := &ShareConfig{
				Name:                             tc.share,
				MetadataStore:                    "meta-test",
				Enabled:                          true,
				AclFlagInheritedCanonicalization: tc.val,
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

			share, err := svc.GetShare(tc.share)
			if err != nil {
				t.Fatalf("GetShare: %v", err)
			}
			if share.AclFlagInheritedCanonicalization != tc.val {
				t.Errorf(
					"AclFlagInheritedCanonicalization=%v, want %v",
					share.AclFlagInheritedCanonicalization, tc.val,
				)
			}
		})
	}
}
