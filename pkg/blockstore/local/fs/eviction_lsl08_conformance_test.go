package fs_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFSStore_EvictionLSL08Conformance runs the shared LSL-08 D-27
// eviction conformance suite against the in-tree *fs.FSStore. The factory
// wires a small disk limit (so eviction is easy to trigger) and a counting
// FileBlockStore (so the no-FBS-call assertion in the suite can probe).
func TestFSStore_EvictionLSL08Conformance(t *testing.T) {
	localtest.RunEvictionLSL08Suite(t, func(t *testing.T) *fs.FSStore {
		t.Helper()
		dir := t.TempDir()
		mds := memmeta.NewMemoryMetadataStoreWithDefaults()
		// Wrap with the counting FBS so the suite's no-FBS-call assertion
		// is meaningful. countingFBSWrapper is defined in this _test.go to
		// keep the production package free of test-only types.
		spy := &countingFBSWrapper{inner: mds}
		bc, err := fs.New(dir, 600, 1<<30, spy)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		bc.SetEvictionEnabled(true)
		bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)
		t.Cleanup(func() { _ = bc.Close() })
		return bc
	})
}

// countingFBSWrapper is a thin call-counting wrapper around a
// FileBlockStore. Mirrors the package-internal countingFileBlockStore but
// lives in this external _test.go so it can be wired into the
// localtest-side conformance factory. Satisfies fs.FBSCounter via the
// exported ResetCount/TotalCount methods.
type countingFBSWrapper struct {
	inner   blockstore.FileBlockStore
	counter int
}

func (c *countingFBSWrapper) ResetCount()     { c.counter = 0 }
func (c *countingFBSWrapper) TotalCount() int { return c.counter }

func (c *countingFBSWrapper) GetFileBlock(ctx context.Context, id string) (*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.GetFileBlock(ctx, id)
}
func (c *countingFBSWrapper) PutFileBlock(ctx context.Context, b *blockstore.FileBlock) error {
	c.counter++
	return c.inner.PutFileBlock(ctx, b)
}
func (c *countingFBSWrapper) DeleteFileBlock(ctx context.Context, id string) error {
	c.counter++
	return c.inner.DeleteFileBlock(ctx, id)
}
func (c *countingFBSWrapper) IncrementRefCount(ctx context.Context, id string) error {
	c.counter++
	return c.inner.IncrementRefCount(ctx, id)
}
func (c *countingFBSWrapper) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	c.counter++
	return c.inner.DecrementRefCount(ctx, id)
}
func (c *countingFBSWrapper) FindFileBlockByHash(ctx context.Context, h blockstore.ContentHash) (*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.FindFileBlockByHash(ctx, h)
}
func (c *countingFBSWrapper) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.ListLocalBlocks(ctx, olderThan, limit)
}
func (c *countingFBSWrapper) ListRemoteBlocks(ctx context.Context, limit int) ([]*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.ListRemoteBlocks(ctx, limit)
}
func (c *countingFBSWrapper) ListUnreferenced(ctx context.Context, limit int) ([]*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.ListUnreferenced(ctx, limit)
}
func (c *countingFBSWrapper) ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	c.counter++
	return c.inner.ListFileBlocks(ctx, payloadID)
}
func (c *countingFBSWrapper) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	c.counter++
	return c.inner.EnumerateFileBlocks(ctx, fn)
}
