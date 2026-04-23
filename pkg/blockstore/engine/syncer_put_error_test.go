package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// errBoomPut is the sentinel error returned by failingPutFileBlockStore
// when its failAfter counter has been reached.
var errBoomPut = errors.New("boom put")

// failingPutFileBlockStore wraps a real FileBlockStore and can be instructed
// to fail PutFileBlock calls after the first `allowed` successful calls, so
// the test can let the initial "mark block syncing" Put succeed while failing
// the post-upload "mark block remote" Put — exercising the previously-swallowed
// error path in syncFileBlock.
type failingPutFileBlockStore struct {
	blockstore.FileBlockStore
	putCount atomic.Int64 // total PutFileBlock calls observed
	allowed  int64        // number of leading Puts that succeed; subsequent Puts return errBoomPut
}

func (f *failingPutFileBlockStore) PutFileBlock(ctx context.Context, block *blockstore.FileBlock) error {
	n := f.putCount.Add(1)
	if n > f.allowed {
		return errBoomPut
	}
	return f.FileBlockStore.PutFileBlock(ctx, block)
}

// TestSyncFileBlock_PropagatesPutError asserts that syncFileBlock returns an
// error when the FileBlockStore.PutFileBlock call that persists the post-upload
// BlockStateRemote transition fails. Before TD-02b, this Put was swallowed by
// `_ = m.fileBlockStore.PutFileBlock(...)` and the block would appear synced
// upstream despite metadata drift.
func TestSyncFileBlock_PropagatesPutError(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}

	failingStore := &failingPutFileBlockStore{
		FileBlockStore: ms,
		allowed:        1, // First Put (mark Syncing) succeeds; all later Puts fail.
	}

	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	m := NewSyncer(bc, rs, failingStore, DefaultConfig())
	defer func() { _ = m.Close() }()

	// Prepare a local block file on disk — syncFileBlock reads it directly.
	payloadID := "export/put-error-test.bin"
	blockFile := filepath.Join(tmpDir, "put-error-block.blk")
	data := []byte("hello world")
	if err := os.WriteFile(blockFile, data, 0o600); err != nil {
		t.Fatalf("WriteFile(block) error = %v", err)
	}

	fb := &blockstore.FileBlock{
		ID:         payloadID + "/0",
		LocalPath:  blockFile,
		DataSize:   uint32(len(data)),
		State:      blockstore.BlockStateLocal,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}

	// Drive the failing-Put path: after the first successful Put that flips
	// the block to Syncing, the post-upload Put that records BlockStateRemote
	// will fail with errBoomPut. On the pre-fix (`_ =`) code this error was
	// swallowed and syncFileBlock returned nil.
	err = m.syncFileBlock(ctx, fb)
	if err == nil {
		t.Fatalf("syncFileBlock returned nil, want error wrapping %v (put error was swallowed)", errBoomPut)
	}
	if !errors.Is(err, errBoomPut) {
		t.Fatalf("syncFileBlock error = %v, want wrapping %v", err, errBoomPut)
	}
}
