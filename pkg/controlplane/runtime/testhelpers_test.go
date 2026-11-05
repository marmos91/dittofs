package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/controlplane/models"
	"github.com/cubbit/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/cubbit/dittofs/pkg/controlplane/store"
	"github.com/cubbit/dittofs/pkg/metadata"
)

// setJournalRoot points the runtime's share state root at a temp dir and drops
// every share the test leaves behind, before the stores those shares use
// close: the removal is registered whenever the shares change rather than once
// here, where it would run last. Removing an already-removed share is a no-op,
// so a second call on the same runtime is harmless.
func setJournalRoot(t *testing.T, rt *Runtime) {
	t.Helper()
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	rt.OnShareChange(func([]string) {
		t.Cleanup(func() {
			for _, name := range rt.ListShares() {
				_ = rt.RemoveShare(name)
			}
		})
	})
}

// newRuntimeWithBlockStore builds a runtime over an in-memory control-plane
// store holding one memory block store, and returns that store's id. Every
// share needs a block store, so every AddShare here carries the returned id.
func newRuntimeWithBlockStore(t *testing.T) (*Runtime, string) {
	t.Helper()
	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })
	bsID, err := cps.CreateBlockStore(context.Background(), &models.BlockStoreConfig{
		Name: "test-blocks", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	rt := New(cps)
	setJournalRoot(t, rt)
	return rt, bsID
}

// remoteForShare returns the underlying remote store for a share's remote
// config so the test can count CAS objects (Walk) vs block objects

// closeWatchingStore records the outcome of every transaction that reaches the
// metadata store after it has been closed.
type closeWatchingStore struct {
	metadata.Store

	mu        sync.Mutex
	closed    bool
	afterErrs []error
}

func (c *closeWatchingStore) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Store.Close()
}

func (c *closeWatchingStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	err := c.Store.WithTransaction(ctx, fn)

	c.mu.Lock()
	if c.closed {
		c.afterErrs = append(c.afterErrs, err)
	}
	c.mu.Unlock()
	return err
}

func (c *closeWatchingStore) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
