//go:build integration

package badger_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
		})
		return store
	})
}
