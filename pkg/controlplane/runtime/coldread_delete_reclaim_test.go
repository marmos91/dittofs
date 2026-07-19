package runtime

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// TestColdReadThenDeleteReclaimsLocal reproduces the blocks-flip SMB local-disk
// leak: after a file is carved+synced to the remote, evicted, and then
// COLD-READ (which hydrates its bytes back into the local journal), deleting
// the file must free the local tier back to zero.
//
// The NFS variant of the E2E stays green because a kernel NFS client serves the
// post-evict read from its own page cache, so the server never hydrates and has
// nothing local to reclaim on unlink. The pure-Go SMB client does not cache, so
// its read drives a real server-side hydration — exercised directly here without
// any protocol.
func TestColdReadThenDeleteReclaimsLocal(t *testing.T) {
	ctx := context.Background()
	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)
	metaStore := registerSQLiteMeta(t, rt, cp, "sqlite-meta")
	localID := createFSLocalBlockStore(t, cp, "fs-local")
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	remoteCfg := &models.BlockStoreConfig{Name: "mem-remote", Kind: models.BlockStoreKindRemote, Type: "memory"}
	remoteID, err := cp.CreateBlockStore(ctx, remoteCfg)
	if err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	shareName := "/coldread-reclaim"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:               shareName,
		MetadataStore:      "sqlite-meta",
		LocalBlockStoreID:  localID,
		RemoteBlockStoreID: remoteID,
		Enabled:            true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	bs, err := rt.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}

	payload, handle := createFileForPayload(t, ctx, metaStore, shareName, "flip.bin")

	const fileSize = 16 * 1024 * 1024
	data := make([]byte, fileSize)
	rand.New(rand.NewSource(0xF11B)).Read(data) //nolint:gosec // deterministic fixture

	if err := common.WriteToBlockStore(ctx, bs, payload, data, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
	if err := common.CommitBlockStore(ctx, bs, payload); err != nil {
		t.Fatalf("CommitBlockStore: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
	t.Logf("after carve: local DiskUsed=%d", bs.LocalStats().DiskUsed)

	// Evict the synced local tier: bytes now live only on the remote.
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	t.Logf("after evict: local DiskUsed=%d", bs.LocalStats().DiskUsed)

	// Cold read the whole file: hydrates the covering chunks back into the
	// local journal.
	res, err := common.ReadFromBlockStore(ctx, bs, payload, 0, uint32(fileSize))
	if err != nil {
		t.Fatalf("cold ReadFromBlockStore: %v", err)
	}
	if len(res.Data) != fileSize {
		t.Fatalf("cold read returned %d bytes, want %d", len(res.Data), fileSize)
	}
	t.Logf("after cold read (hydrate): local DiskUsed=%d", bs.LocalStats().DiskUsed)

	// Unlink: the file-removal contract must reclaim the local tier eagerly —
	// with no further eviction pass.
	if err := bs.Delete(ctx, string(payload), nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_ = handle
	afterDelete := bs.LocalStats().DiskUsed
	t.Logf("after delete: local DiskUsed=%d", afterDelete)

	// The file's bytes must be gone (a read now zero-fills).
	res2, rerr := common.ReadFromBlockStore(ctx, bs, payload, 0, uint32(fileSize))
	if rerr != nil {
		t.Fatalf("read-after-delete: %v", rerr)
	}
	for i, b := range res2.Data {
		if b != 0 {
			t.Fatalf("read-after-delete: byte %d = %d, want 0 (file should be gone)", i, b)
		}
	}

	if afterDelete != 0 {
		t.Fatalf("local DiskUsed after delete = %d, want 0 (cold-read hydration leaked on unlink)", afterDelete)
	}
}
