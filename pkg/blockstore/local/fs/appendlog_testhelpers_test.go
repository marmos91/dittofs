package fs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// nopFBS is a no-op blockstore.EngineFileBlockStore used by
// tests. Every read returns ErrFileBlockNotFound; every write is a no-op.
// Sufficient for the append-log path because AppendWrite does not
// consult FileBlockStore at all.
//
// Shared across /05/06 test files in the fs package.
// narrowed the public FileBlockStore to 6 methods; this
// stub satisfies the wider engine-internal surface (the 6 plus
// GetFileBlock + ListFileBlocks).
type nopFBS struct{}

func (nopFBS) GetByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) Put(_ context.Context, _ *blockstore.FileBlock) error { return nil }
func (nopFBS) Delete(_ context.Context, _ string) error {
	return blockstore.ErrFileBlockNotFound
}
func (nopFBS) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (nopFBS) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBS) DecrementRefCountAndReap(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBS) AddRef(_ context.Context, _ blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
	// tests don't exercise the LRU hit path, so always
	// returning ErrUnknownHash matches "hash never Put" — production
	// callers fall back to the full Put path.
	return blockstore.ErrUnknownHash
}
func (nopFBS) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// Engine-internal surface (kept off the public FileBlockStore per
func (nopFBS) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (nopFBS) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// newFSStoreForTest constructs an FSStore in t.TempDir with the given
// options and a nopFBS backing store. Registers t.Cleanup to Close the
// store. Shared by /05/06/07/09 test files in the fs package.
func newFSStoreForTest(t *testing.T, opts FSStoreOptions) *FSStore {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsstore-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, opts)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() {
		_ = bc.Close()
		// On Windows, file handles may linger after Close due to
		// kernel-level delayed release. Retry so cleanup doesn't
		// fail the test for a timing issue.
		for range 5 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return bc
}
