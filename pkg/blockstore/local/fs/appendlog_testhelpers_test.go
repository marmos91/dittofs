package fs

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// nopFBS is a no-op FileBlockStore used by Phase 10 tests. Every read
// returns ErrFileBlockNotFound; every write is a no-op. Sufficient for
// the append-log path because AppendWrite (D-34) does not consult
// FileBlockStore at all.
//
// Shared across plan 04/05/06 test files in the fs package. If the
// FileBlockStore interface gains or drops methods, stub signatures here
// must be updated to keep the package tests compiling.
type nopFBS struct{}

func (nopFBS) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (nopFBS) PutFileBlock(_ context.Context, _ *blockstore.FileBlock) error { return nil }
func (nopFBS) DeleteFileBlock(_ context.Context, _ string) error {
	return blockstore.ErrFileBlockNotFound
}
func (nopFBS) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (nopFBS) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFBS) FindFileBlockByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) ListLocalBlocks(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) ListRemoteBlocks(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) ListUnreferenced(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFBS) EnumerateFileBlocks(_ context.Context, _ func(blockstore.ContentHash) error) error {
	return nil
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
