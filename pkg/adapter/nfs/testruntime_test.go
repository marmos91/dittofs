package nfs

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// newTestRuntime builds a runtime that provisions every share's journal under
// a temp dir. AddShare fails without a journal root.
//
// A share holds its journal open until it is removed, so the runtime drops its
// shares when the test ends. The removal is registered after the temp dir so
// cleanup's reverse order releases the journals before the dir is removed —
// a platform that refuses to unlink an open file cannot remove the root
// otherwise.
func newTestRuntime(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	rt := runtime.New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})
	return rt
}

// newTestShareRuntime builds a runtime over an in-memory control-plane store
// holding one memory block store, and returns that store's id. Every share
// needs a block store, so every AddShare here carries the returned id.
func newTestShareRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	cps, err := store.New(&store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("store.New(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })
	bsID, err := cps.CreateBlockStore(context.Background(), &models.BlockStoreConfig{
		Name: "test-blocks", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	return newTestRuntime(t, cps), bsID
}
