// Package testruntime builds the control-plane runtime fixtures shared by the
// protocol adapter and control-plane tests.
//
// Three adapters carried byte-identical copies of these two fixtures. Nothing
// here touches a protocol type — they are control-plane fixtures wearing three
// adapter paths — so one copy serves all three, and a change to what a share
// needs lands once instead of in three places, two of which keep compiling
// when they are missed.
//
// Nothing here may import a package whose tests use it. Every consumer is an
// in-package test, so importing the package under test would close an import
// cycle, which the go tool rejects for tests. That bound comes from the
// consumers, not from this package: an adapter whose tests are external
// (`package handlers_test`, as `nfs/v3/handlers` does) could be imported here
// without a cycle. Lift it only for such a package, and only for a fixture
// that genuinely needs a protocol type, which is also the point at which the
// fixture probably belongs beside that adapter rather than here.
package testruntime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// New builds a runtime that provisions every share's journal under
// a temp dir. AddShare fails without a journal root.
//
// A share holds its journal open, and its engine reads its metadata store,
// until the share is removed, so the runtime drops its shares when the test
// ends. Cleanup runs in reverse order of registration, and the removal has to
// run before that store closes, or a download still in flight reads a closed
// store. A store is opened before any share that uses it is added, so the
// removal is registered whenever the shares change rather than once here,
// where it would run last. That also releases the journals before the temp dir
// is removed, which a platform that refuses to unlink an open file needs.
func New(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	rt := runtime.New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	rt.OnShareChange(func([]string) {
		t.Cleanup(func() {
			for _, name := range rt.ListShares() {
				_ = rt.RemoveShare(name)
			}
		})
	})
	return rt
}

// NewWithShareStore builds a runtime over an in-memory control-plane store
// holding one memory block store, and returns that store's id. Every share
// needs a block store, so every AddShare here carries the returned id.
func NewWithShareStore(t *testing.T) (*runtime.Runtime, string) {
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
	return New(t, cps), bsID
}
