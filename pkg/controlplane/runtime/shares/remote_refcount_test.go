package shares

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestRemoteStore_SharedAndReleasedWithItsShares pins the remote ref-count:
// two shares bound to one block store config share one remote, removing one
// keeps it for the other, and removing the last releases it.
func TestRemoteStore_SharedAndReleasedWithItsShares(t *testing.T) {
	svc := New()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)
	metaSvc := metadata.New()

	for _, name := range []string{"/one", "/two"} {
		cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID}
		if err := svc.AddShare(context.Background(), cfg, fixedStoreProvider{store: store},
			metaSvc, memBlockStoreProvider{}, defaults); err != nil {
			t.Fatalf("AddShare %s: %v", name, err)
		}
	}
	entries := svc.DistinctRemoteStores()
	if len(entries) != 1 || len(entries[0].Shares) != 2 {
		t.Fatalf("remotes for two shares on one config = %+v, want one remote with both shares", entries)
	}

	if err := svc.RemoveShare("/one"); err != nil {
		t.Fatalf("RemoveShare /one: %v", err)
	}
	if entries := svc.DistinctRemoteStores(); len(entries) != 1 || len(entries[0].Shares) != 1 {
		t.Fatalf("remotes after removing one share = %+v, want the remote kept for the other", entries)
	}

	if err := svc.RemoveShare("/two"); err != nil {
		t.Fatalf("RemoveShare /two: %v", err)
	}
	svc.mu.RLock()
	left := len(svc.remoteStores)
	svc.mu.RUnlock()
	if left != 0 {
		t.Errorf("%d remote store(s) still held after the last share was removed", left)
	}
}
