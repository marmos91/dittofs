package fs

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// nopFBS is a no-op blockstore.EngineFileBlockStore used by Phase 10
// tests. Every read returns ErrFileBlockNotFound; every write is a no-op.
// Sufficient for the append-log path because AppendWrite (D-34) does not
// consult FileBlockStore at all.
//
// Shared across plan 04/05/06 test files in the fs package. Phase 12
// (META-03 / D-09) narrowed the public FileBlockStore to 6 methods; this
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
func (nopFBS) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// Engine-internal surface (kept off the public FileBlockStore per
// META-03 / D-09).
func (nopFBS) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (nopFBS) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// newFSStoreForTest constructs an FSStore in t.TempDir with the given
// options and a nopFBS backing store. Registers t.Cleanup to Close the
// store. Shared by plan 04/05/06/07/09 test files in the fs package.
func newFSStoreForTest(t *testing.T, opts FSStoreOptions) *FSStore {
	t.Helper()
	dir := t.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, opts)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}
