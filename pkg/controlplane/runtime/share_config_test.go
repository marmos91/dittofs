package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// nfsOptionsStore finds the share's metadata store and answers its NFS options
// with nfs and err. Any other method panics on the nil embedded interface.
type nfsOptionsStore struct {
	store.Store
	nfs *models.ShareAdapterConfig
	err error
}

func (s nfsOptionsStore) GetMetadataStoreByID(context.Context, string) (*models.MetadataStoreConfig, error) {
	return &models.MetadataStoreConfig{Name: "meta"}, nil
}

func (s nfsOptionsStore) GetShareAdapterConfig(context.Context, string, string) (*models.ShareAdapterConfig, error) {
	return s.nfs, s.err
}

// TestBuildShareConfig_NFSOptionsFailureIsAnError pins that a share whose NFS
// options can't be read is not built with the defaults, which would serve it
// without its squash and auth-flavor policy.
func TestBuildShareConfig_NFSOptionsFailureIsAnError(t *testing.T) {
	share := &models.Share{ID: "id", Name: "/export", MetadataStoreID: "meta"}
	for name, s := range map[string]nfsOptionsStore{
		"lookup fails": {err: context.Canceled},
		"parse fails":  {nfs: &models.ShareAdapterConfig{Config: "{"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := buildShareConfig(context.Background(), s, share)
			if err == nil {
				t.Fatalf("buildShareConfig = %+v, nil; want an error rather than the default NFS options", cfg)
			}
			if s.err != nil && !errors.Is(err, s.err) {
				t.Errorf("err = %v, want it to wrap %v", err, s.err)
			}
		})
	}
	t.Run("no NFS options", func(t *testing.T) {
		if _, err := buildShareConfig(context.Background(), nfsOptionsStore{}, share); err != nil {
			t.Fatalf("a share with no NFS options row must take the defaults: %v", err)
		}
	})
}
