package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// testCoordinator is a faithful, test-local re-implementation of the
// production engine.MetadataCoordinator (pkg/controlplane/runtime/shares/
// coordinator.go) — that package may not be imported here per the
// strict-grep boundary.
type testCoordinator struct {
	store metadata.Store
}

var _ engine.MetadataCoordinator = (*testCoordinator)(nil)

func (c *testCoordinator) IncrementRefCount(ctx context.Context, hash block.ContentHash) error {
	fb, err := c.store.GetByHash(ctx, hash)
	if err != nil {
		return err
	}
	if fb == nil {
		return metadata.ErrFileChunkNotFound
	}
	return c.store.IncrementRefCount(ctx, fb.ID)
}

func (c *testCoordinator) DecrementRefCountAndReap(ctx context.Context, payloadID string, offset uint64) (uint32, error) {
	// By EXACT ID — mirrors the production coordinator (no hash resolution).
	id := fmt.Sprintf("%s/%d", payloadID, offset)
	count, err := c.store.DecrementRefCountAndReap(ctx, id)
	if err != nil {
		if errors.Is(err, metadata.ErrFileChunkNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

// ReprojectBlocks is a no-op: this fake does not model the Blocks
// projection.
func (c *testCoordinator) ReprojectBlocks(_ context.Context, _ string) error { return nil }

func createShare(t *testing.T, store metadata.Store, shareName string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	// CreateRootDirectory inserts BOTH the "/" files-row AND the share row
	// (Postgres' shares.root_file_id is NOT NULL, so the share row cannot be
	// created independently). It is idempotent on the share name.
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	return rootHandle
}

// manifestHashes returns the set of DISTINCT content hashes the metadata
// snapshot manifest exposes, plus the serialized manifest size.

// createRealFile creates a file row through the metadata store the way the
// production CreateFile path does — crucially setting a UUID-based PayloadID
// (buildPayloadID(share, id), see #1166 PR-3) so the rollup-persist
// GetFileByPayloadID lookup resolves to THIS row. It does NOT pre-populate
// Blocks (doing so would mask the rollup-persist bug under test).
func createRealFile(t *testing.T, store metadata.Store, shareName, name string, rootHandle metadata.FileHandle) (string, metadata.FileHandle) {
	t.Helper()
	ctx := context.Background()
	path := "/" + name
	handle, err := store.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	payloadID := strings.TrimPrefix(shareName, "/") + "/" + id.String()
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			PayloadID: metadata.PayloadID(payloadID),
		},
	}
	if err := store.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs %q: %v", name, err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent %q: %v", name, err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		t.Fatalf("SetChild %q: %v", name, err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount %q: %v", name, err)
	}
	return payloadID, handle
}

// DecrementRefCountAndReapMany loops the single-offset form so this double
// keeps whatever bookkeeping and error injection that form already carries.
func (c *testCoordinator) DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error {
	for _, offset := range offsets {
		if _, err := c.DecrementRefCountAndReap(ctx, payloadID, offset); err != nil {
			return err
		}
	}
	return nil
}
