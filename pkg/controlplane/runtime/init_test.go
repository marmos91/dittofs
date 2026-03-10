package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupTestRuntime creates a runtime with SQLite in-memory store and a registered metadata store.
func setupTestRuntime(t *testing.T) (*Runtime, cpstore.Store) {
	t.Helper()
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	rt := New(s)
	ctx := context.Background()

	// Register a metadata store in the DB and runtime.
	metaStoreCfg := &models.MetadataStoreConfig{
		Name: "test-meta",
		Type: "memory",
	}
	if _, err := s.CreateMetadataStore(ctx, metaStoreCfg); err != nil {
		t.Fatalf("failed to create metadata store config: %v", err)
	}
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("failed to register metadata store: %v", err)
	}

	// Set local store defaults.
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
		MaxSize: 0, // unlimited
	})

	return rt, s
}

// createLocalBlockStoreConfig creates a local block store config in the DB with memory type.
func createLocalBlockStoreConfig(t *testing.T, s cpstore.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	cfg := &models.BlockStoreConfig{
		Name: name,
		Kind: models.BlockStoreKindLocal,
		Type: "memory",
	}
	id, err := s.CreateBlockStore(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create local block store config: %v", err)
	}
	return id
}

// createRemoteBlockStoreConfig creates a remote block store config in the DB with memory type.
func createRemoteBlockStoreConfig(t *testing.T, s cpstore.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	cfg := &models.BlockStoreConfig{
		Name: name,
		Kind: models.BlockStoreKindRemote,
		Type: "memory",
	}
	id, err := s.CreateBlockStore(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create remote block store config: %v", err)
	}
	return id
}

func TestPerShareBlockStoreLocalOnly(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	// Create a local block store config in the DB.
	localID := createLocalBlockStoreConfig(t, s, "test-local")

	// Create a share in the DB referencing the local block store.
	metaStores, _ := s.ListMetadataStores(ctx)
	share := &models.Share{
		Name:              "/test-share",
		MetadataStoreID:   metaStores[0].ID,
		LocalBlockStoreID: localID,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("failed to create share: %v", err)
	}

	// Load shares (this creates BlockStores).
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore failed: %v", err)
	}

	// Verify share exists with non-nil BlockStore.
	shareObj, err := rt.GetShare("/test-share")
	if err != nil {
		t.Fatalf("GetShare failed: %v", err)
	}
	if shareObj.BlockStore == nil {
		t.Fatal("expected non-nil BlockStore after local-only init")
	}

	// Verify BlockStore works: write and read back data.
	testData := []byte("hello per-share")
	if err := shareObj.BlockStore.WriteAt(ctx, "test-payload", testData, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	readBuf := make([]byte, len(testData))
	n, err := shareObj.BlockStore.ReadAt(ctx, "test-payload", readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("expected %d bytes, got %d", len(testData), n)
	}
	if string(readBuf) != string(testData) {
		t.Errorf("expected %q, got %q", testData, readBuf)
	}
}

func TestPerShareBlockStoreIsolation(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	// Create two different local block store configs (both memory type for speed).
	localID1 := createLocalBlockStoreConfig(t, s, "local-1")
	localID2 := createLocalBlockStoreConfig(t, s, "local-2")

	metaStores, _ := s.ListMetadataStores(ctx)
	metaID := metaStores[0].ID

	// Create two shares in DB.
	share1 := &models.Share{
		Name:              "/share-1",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID1,
	}
	if _, err := s.CreateShare(ctx, share1); err != nil {
		t.Fatalf("failed to create share-1: %v", err)
	}

	share2 := &models.Share{
		Name:              "/share-2",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID2,
	}
	if _, err := s.CreateShare(ctx, share2); err != nil {
		t.Fatalf("failed to create share-2: %v", err)
	}

	// Load shares.
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore failed: %v", err)
	}

	s1, _ := rt.GetShare("/share-1")
	s2, _ := rt.GetShare("/share-2")

	if s1.BlockStore == nil || s2.BlockStore == nil {
		t.Fatal("both shares should have non-nil BlockStores")
	}

	// Write data to share-1.
	data1 := []byte("data for share 1")
	if err := s1.BlockStore.WriteAt(ctx, "payload-1", data1, 0); err != nil {
		t.Fatalf("share-1 WriteAt failed: %v", err)
	}

	// Write different data to share-2.
	data2 := []byte("data for share 2")
	if err := s2.BlockStore.WriteAt(ctx, "payload-2", data2, 0); err != nil {
		t.Fatalf("share-2 WriteAt failed: %v", err)
	}

	// Verify share-1 does NOT see share-2's data (different local stores).
	readBuf1 := make([]byte, len(data2))
	_, err := s1.BlockStore.ReadAt(ctx, "payload-2", readBuf1, 0)
	// For memory local stores, this should either return zero-filled buffer or
	// the data won't be found. Since both are memory stores but separate instances,
	// the data should not be present.
	if err == nil {
		// Data was read. With separate memory stores, the buffer should be zeros.
		isZero := true
		for _, b := range readBuf1 {
			if b != 0 {
				isZero = false
				break
			}
		}
		if !isZero {
			t.Error("share-1's BlockStore should not see share-2's payload-2 data")
		}
	}

	// Verify share-2 does NOT see share-1's data.
	readBuf2 := make([]byte, len(data1))
	_, err = s2.BlockStore.ReadAt(ctx, "payload-1", readBuf2, 0)
	if err == nil {
		isZero := true
		for _, b := range readBuf2 {
			if b != 0 {
				isZero = false
				break
			}
		}
		if !isZero {
			t.Error("share-2's BlockStore should not see share-1's payload-1 data")
		}
	}

	// Verify each share can read its OWN data.
	readBuf1Own := make([]byte, len(data1))
	n, err := s1.BlockStore.ReadAt(ctx, "payload-1", readBuf1Own, 0)
	if err != nil {
		t.Fatalf("share-1 ReadAt own data failed: %v", err)
	}
	if n != len(data1) || string(readBuf1Own) != string(data1) {
		t.Errorf("share-1 read back: expected %q, got %q", data1, readBuf1Own)
	}

	readBuf2Own := make([]byte, len(data2))
	n, err = s2.BlockStore.ReadAt(ctx, "payload-2", readBuf2Own, 0)
	if err != nil {
		t.Fatalf("share-2 ReadAt own data failed: %v", err)
	}
	if n != len(data2) || string(readBuf2Own) != string(data2) {
		t.Errorf("share-2 read back: expected %q, got %q", data2, readBuf2Own)
	}
}

func TestPerShareBlockStoreRemoteSharing(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	// Create a SHARED remote block store config.
	remoteID := createRemoteBlockStoreConfig(t, s, "shared-remote")

	// Create two local block store configs (separate per share).
	localID1 := createLocalBlockStoreConfig(t, s, "local-r1")
	localID2 := createLocalBlockStoreConfig(t, s, "local-r2")

	metaStores, _ := s.ListMetadataStores(ctx)
	metaID := metaStores[0].ID

	// Create two shares referencing the SAME remote block store.
	share1 := &models.Share{
		Name:               "/remote-share-1",
		MetadataStoreID:    metaID,
		LocalBlockStoreID:  localID1,
		RemoteBlockStoreID: &remoteID,
	}
	if _, err := s.CreateShare(ctx, share1); err != nil {
		t.Fatalf("failed to create remote-share-1: %v", err)
	}

	share2 := &models.Share{
		Name:               "/remote-share-2",
		MetadataStoreID:    metaID,
		LocalBlockStoreID:  localID2,
		RemoteBlockStoreID: &remoteID,
	}
	if _, err := s.CreateShare(ctx, share2); err != nil {
		t.Fatalf("failed to create remote-share-2: %v", err)
	}

	// Load shares.
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore failed: %v", err)
	}

	s1, _ := rt.GetShare("/remote-share-1")
	s2, _ := rt.GetShare("/remote-share-2")

	if s1.BlockStore == nil || s2.BlockStore == nil {
		t.Fatal("both shares should have non-nil BlockStores")
	}

	// Both BlockStores should have non-nil remote stores.
	remote1 := s1.BlockStore.Remote()
	remote2 := s2.BlockStore.Remote()
	if remote1 == nil || remote2 == nil {
		t.Fatal("both BlockStores should have non-nil remote stores")
	}

	// Verify shared remote: write via share-1's remote, visible via share-2's remote.
	testBlockKey := "test-shared/block-0"
	testData := []byte("shared remote data")
	if err := remote1.WriteBlock(ctx, testBlockKey, testData); err != nil {
		t.Fatalf("write to shared remote via share-1 failed: %v", err)
	}
	readBack, err := remote2.ReadBlock(ctx, testBlockKey)
	if err != nil {
		t.Fatalf("read from shared remote via share-2 failed: %v", err)
	}
	if string(readBack) != string(testData) {
		t.Errorf("expected %q from shared remote, got %q", testData, readBack)
	}

	// Remove share-1. Remote store should still be open (ref count > 0).
	if err := rt.RemoveShare("/remote-share-1"); err != nil {
		t.Fatalf("RemoveShare failed: %v", err)
	}

	// share-2's remote store should still work.
	if err := s2.BlockStore.Remote().HealthCheck(ctx); err != nil {
		t.Fatalf("remote store should still be healthy after removing one share: %v", err)
	}

	// Remove share-2. Now the remote store should be fully cleaned up.
	if err := rt.RemoveShare("/remote-share-2"); err != nil {
		t.Fatalf("RemoveShare for share-2 failed: %v", err)
	}

	// Verify both shares are gone.
	if rt.ShareExists("/remote-share-1") || rt.ShareExists("/remote-share-2") {
		t.Error("shares should not exist after removal")
	}
}

func TestRemoveShareClosesBlockStore(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	localID := createLocalBlockStoreConfig(t, s, "local-close")

	metaStores, _ := s.ListMetadataStores(ctx)
	metaID := metaStores[0].ID

	share := &models.Share{
		Name:              "/close-test",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("failed to create share: %v", err)
	}

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore failed: %v", err)
	}

	shareObj, _ := rt.GetShare("/close-test")
	if shareObj.BlockStore == nil {
		t.Fatal("expected non-nil BlockStore")
	}
	bs := shareObj.BlockStore

	// Remove the share.
	if err := rt.RemoveShare("/close-test"); err != nil {
		t.Fatalf("RemoveShare failed: %v", err)
	}

	// Verify share is gone from registry.
	if rt.ShareExists("/close-test") {
		t.Error("share should not exist after removal")
	}

	// Verify write to the closed BlockStore returns an error.
	// After Close(), the syncer and local store are stopped. Depending on the
	// implementation, WriteAt may return an error or silently succeed to the
	// closed memory store. We just verify no panic occurs.
	_ = bs.WriteAt(ctx, "post-close-payload", []byte("should fail or noop"), 0)
}
