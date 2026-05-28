package badger_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestBlockLayoutConformance runs the per-share BlockLayout conformance
// scenarios against the BadgerDB metadata store.
//
// Note: this test does NOT carry the `//go:build integration` tag — it
// is a fast, self-contained scenario like rollup_test.go and is part
// of the default test lane. The full conformance suite
// (TestConformance) remains gated by the integration tag.
func TestBlockLayoutConformance(t *testing.T) {
	storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.MetadataStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			_ = store.Close()
		})
		return store
	})
}
