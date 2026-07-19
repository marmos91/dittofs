package engine_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// buildSelfHealEngine wires a journal-backed engine with per-read integrity
// verification turned on (the durable-tier posture). A non-nil mem wires a real
// remote block store (so a corrupt warm read can self-heal); a nil mem leaves the
// share local-only (so a corrupt warm read must fail closed). It returns the
// engine and the FSStore base dir so a test can corrupt a segment on disk.
func buildSelfHealEngine(t *testing.T, ms metadata.Store, mem *remotememory.Store) (*engine.Store, string) {
	t.Helper()
	dir := t.TempDir()
	localStore, err := fs.NewWithOptions(dir, 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
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
	if mem != nil {
		syncer := engine.NewSyncer(localStore, mem, ms, engine.DefaultConfig())
		syncer.SetSyncedHashStore(syncedHashStore)
		syncer.SetRemoteBlockStore(mem)
		cfg.Syncer = syncer
		cfg.Remote = mem
	} else {
		syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
		syncer.SetSyncedHashStore(syncedHashStore)
		cfg.Syncer = syncer
	}
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
	n, err := bs.ReadAt(ctx, pid, nil, got, 0)
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

// TestWarmReadSelfHeal_LocalOnlyFailsClosed corrupts a byte in a local-only
// share's segment and asserts the verified warm read fails closed with
// ErrIntegrityCheckFailed (maps to NFS3ERR_IO) — never zeros, never garbage.
// There is no remote to heal from, so detection-only is the correct outcome.
func TestWarmReadSelfHeal_LocalOnlyFailsClosed(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, dir := buildSelfHealEngine(t, ms, nil) // local-only: no remote

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

	corruptSegmentByte(t, filepath.Join(dir, "journal"))

	got := make([]byte, size)
	_, err := bs.ReadAt(ctx, pid, nil, got, 0)
	if err == nil {
		t.Fatalf("local-only corrupt read must fail closed, got nil error")
	}
	if !errors.Is(err, block.ErrIntegrityCheckFailed) {
		t.Fatalf("want ErrIntegrityCheckFailed, got: %v", err)
	}
}
