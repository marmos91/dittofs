package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metadatatesting "github.com/marmos91/dittofs/pkg/metadata/testing"
)

// TestBadgerMetadataStore runs the complete MetadataStore test suite
// against the BadgerMetadataStore implementation.
//
// This test verifies that BadgerMetadataStore correctly implements all
// MetadataStore interface methods and behaves correctly with persistent
// storage backed by BadgerDB.
func TestBadgerMetadataStore(t *testing.T) {
	suite := &metadatatesting.StoreTestSuite{
		NewStore: func() metadata.MetadataStore {
			return newTestBadgerStore(t)
		},
	}

	suite.Run(t)
}

// TestBadgerMetadataStorePersistence tests that metadata persists across
// store close/reopen cycles.
func TestBadgerMetadataStorePersistence(t *testing.T) {
	// Create temporary directory for this test
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "badger.db")

	ctx := context.Background()

	// Helper to create store at same path
	createStore := func() *BadgerMetadataStore {
		store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}
		return store
	}

	// Phase 1: Create and populate store
	t.Run("Phase1_Populate", func(t *testing.T) {
		store := createStore()
		defer func() { _ = store.Close() }()

		// Add a share
		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		}
		err := store.AddShare(ctx, "testshare", metadata.ShareOptions{
			ReadOnly:    false,
			RequireAuth: false,
		}, rootAttr)
		if err != nil {
			t.Fatalf("Failed to add share: %v", err)
		}

		// Verify share was added
		shares, err := store.GetShares(ctx)
		if err != nil {
			t.Fatalf("Failed to get shares: %v", err)
		}
		if len(shares) != 1 || shares[0].Name != "testshare" {
			t.Fatalf("Share not found or incorrect: got %v", shares)
		}
	})

	// Phase 2: Reopen and verify persistence
	t.Run("Phase2_Verify", func(t *testing.T) {
		store := createStore()
		defer func() { _ = store.Close() }()

		// Verify share persisted
		shares, err := store.GetShares(ctx)
		if err != nil {
			t.Fatalf("Failed to get shares: %v", err)
		}
		if len(shares) != 1 || shares[0].Name != "testshare" {
			t.Fatalf("Share did not persist: got %v", shares)
		}
	})
}

// newTestBadgerStore creates a BadgerMetadataStore instance for testing
// using a temporary directory.
func newTestBadgerStore(t *testing.T) *BadgerMetadataStore {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("Failed to create test BadgerMetadataStore: %v", err)
	}

	return store
}
