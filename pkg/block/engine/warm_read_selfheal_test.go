package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// buildSelfHealEngine wires a journal-backed engine with per-read integrity
// verification turned on (the durable-tier posture) over the given remote block
// store. It returns the engine and the FSStore base dir so a test can corrupt a
// segment on disk.
func buildSelfHealEngine(t *testing.T, ms metadata.Store, mem *remotememory.Store) (*engine.Store, string) {
	t.Helper()
	dir := t.TempDir()
	localStore, err := journal.Open(filepath.Join(dir, "journal"), journal.Config{MaxLocalBytes: 100 * 1024 * 1024,
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	// Durable tier: verify warm reads per-record so on-disk corruption is caught.
	localStore.SetVerifyReads(true)

	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	coord := &testCoordinator{store: ms}
	cfg := engine.BlockStoreConfig{
		Local:           localStore,
		FileChunkStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
	}
	syncer := engine.NewRemoteSync(localStore, mem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	syncer.SetRemoteBlockStore(mem)
	cfg.RemoteSync = syncer
	cfg.Remote = mem
	bs, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, dir
}

// corruptSegmentByte flips one byte inside the largest journal segment, at an
// offset that lands in the first record's payload (well past the 64-byte segment
// header). It writes through a fresh fd; the store's fd sees it via the shared
// page cache. The flip breaks that record's payload CRC, which a verified warm
// read must detect.
func corruptSegmentByte(t *testing.T, journalDir string) {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(journalDir, "*.seg"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no journal segments under %s: %v", journalDir, err)
	}
	var path string
	var best int64
	for _, p := range segs {
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		if fi.Size() > best {
			best, path = fi.Size(), p
		}
	}
	if path == "" {
		t.Fatalf("no non-empty segment under %s", journalDir)
	}
	const off = 2048 // inside the first record's payload for a multi-MiB write
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatalf("read segment byte: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatalf("write corrupt byte: %v", err)
	}
}

// TestWarmReadSelfHeal_RemoteHeals writes a file, uploads it to the remote (no
// eviction — the bytes stay warm locally), corrupts a byte in the local segment,
// then reads the file back. With a remote present the corrupt warm read must
// self-heal: re-fetch the covering chunk (BLAKE3-verified) and return the CORRECT
// bytes, not the corrupt ones.
func TestWarmReadSelfHeal_RemoteHeals(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, dir := buildSelfHealEngine(t, ms, mem)

	rootHandle := createShare(t, ms, "heal")
	pid, _ := createRealFile(t, ms, "heal", "f.bin", rootHandle)

	const size = 2 * 1024 * 1024
	src := make([]byte, size)
	rand.New(rand.NewSource(0xC0FFEE)).Read(src) //nolint:gosec // deterministic fixture

	if _, err := bs.WriteAt(ctx, pid, nil, src, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	corruptSegmentByte(t, filepath.Join(dir, "journal"))

	got := make([]byte, size)
	n, err := bs.ReadAt(ctx, pid, got, 0)
	if err != nil {
		t.Fatalf("ReadAt after corruption should self-heal, got: %v", err)
	}
	if n != size {
		t.Fatalf("short read after heal: n=%d want=%d", n, size)
	}
	if !bytes.Equal(got, src) {
		for i := range got {
			if got[i] != src[i] {
				t.Fatalf("self-heal returned wrong bytes: first diff at +%d got=0x%02x want=0x%02x", i, got[i], src[i])
			}
		}
	}
}

// TestWarmReadSelfHeal_UnhealableFailsClosed corrupts a byte on disk after the
// remote has lost the blocks that back it, so the heal has nothing to re-fetch.
// The read must fail closed — never the corrupt bytes, never zeros.
func TestWarmReadSelfHeal_UnhealableFailsClosed(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, dir := buildSelfHealEngine(t, ms, mem)

	rootHandle := createShare(t, ms, "heal")
	pid, _ := createRealFile(t, ms, "heal", "f.bin", rootHandle)

	const size = 2 * 1024 * 1024
	src := make([]byte, size)
	rand.New(rand.NewSource(0xBEEF)).Read(src) //nolint:gosec // deterministic fixture

	if _, err := bs.WriteAt(ctx, pid, nil, src, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	// Drop every block the remote holds: the manifest still places the bytes,
	// but nothing can serve them back.
	var blockIDs []string
	if err := mem.WalkBlocks(ctx, func(id string, _ block.Meta) error {
		blockIDs = append(blockIDs, id)
		return nil
	}); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	if len(blockIDs) == 0 {
		t.Fatal("remote holds no blocks after the drain; the fixture proves nothing")
	}
	for _, id := range blockIDs {
		if err := mem.DeleteBlock(ctx, id); err != nil {
			t.Fatalf("DeleteBlock(%s): %v", id, err)
		}
	}

	corruptSegmentByte(t, filepath.Join(dir, "journal"))

	got := make([]byte, size)
	if _, err := bs.ReadAt(ctx, pid, got, 0); err == nil {
		t.Fatalf("unhealable corrupt read must fail closed, got nil error")
	}
}
