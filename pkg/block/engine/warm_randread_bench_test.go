package engine_test

// Warm local random-read benchmark + regression guard for the verified-chunk
// cache (skip re-hashing immutable CAS chunks on every 4 KiB read).
//
// Unlike BenchmarkRandRead_Phase12 (memory local store, no disk, no blake3),
// this drives the REAL fs local store: write a multi-MiB payload, DrainRollups
// so the bytes live as rolled-up CAS chunks (WARM local read, no remote), then
// do 4 KiB reads at random offsets through engine.Store.ReadAt — the exact
// production warm path (ReadPayloadAt -> fillFromCASManifest -> ReadChunk ->
// blake3 verify). Remote is nil so scheduleReadahead early-returns.
//
// Run:
//   go test -run=XXX -bench=BenchmarkWarmRandRead_FS -benchtime=3s \
//     -cpuprofile=/tmp/rr.prof -memprofile=/tmp/rr.mem ./pkg/block/engine/

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metastore "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

const (
	warmRRFileSize = 16 * 1024 * 1024 // multi-MiB payload
	warmRRReadSize = 4096             // 4 KiB random read
	warmRRSeed     = 42
)

// setupWarmFSFixture builds a full fs-backed engine over a memory metadata
// store, writes warmRRFileSize bytes, and DrainRollups so the payload is a warm
// local CAS read. Returns the store + payloadID + covering ChunkRef manifest.
func setupWarmFSFixture(b *testing.B) (*engine.Store, string, []block.ChunkRef) {
	b.Helper()
	logger.SetLevel("ERROR")
	b.Cleanup(func() { logger.SetLevel("INFO") })
	ctx := context.Background()

	ms := metastore.NewMemoryMetadataStoreWithDefaults()
	shareName := "/warm"

	// --- share + file row (mirrors createShare/createRealFile) ---
	rootFile, err := ms.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		b.Fatalf("EncodeFileHandle: %v", err)
	}
	handle, err := ms.GenerateHandle(ctx, shareName, "/data.bin")
	if err != nil {
		b.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		b.Fatalf("DecodeFileHandle: %v", err)
	}
	payloadID := "warm/" + id.String()
	if err := ms.PutFile(ctx, &metadata.File{
		ID: id, ShareName: shareName, Path: "/data.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000,
			PayloadID: metadata.PayloadID(payloadID),
		},
	}); err != nil {
		b.Fatalf("PutFile: %v", err)
	}
	if err := ms.SetParent(ctx, handle, rootHandle); err != nil {
		b.Fatalf("SetParent: %v", err)
	}
	if err := ms.SetChild(ctx, rootHandle, "data.bin", handle); err != nil {
		b.Fatalf("SetChild: %v", err)
	}
	if err := ms.SetLinkCount(ctx, handle, 1); err != nil {
		b.Fatalf("SetLinkCount: %v", err)
	}

	// --- fs engine (mirrors newEngineOverStore; nil remote) ---
	localStore, err := fs.NewWithOptions(b.TempDir(), 256*1024*1024, ms, fs.FSStoreOptions{
		LocalChunkIndex: ms,
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 3_600_000, // async rollup never fires; only explicit DrainRollups
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		b.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		b.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		b.Fatalf("engine.Start: %v", err)
	}
	b.Cleanup(func() { _ = bs.Close() })

	// --- write payload, then drain to CAS (warm local) ---
	rng := rand.New(rand.NewSource(warmRRSeed)) //nolint:gosec // bench fixture
	buf := make([]byte, warmRRFileSize)
	if _, err := rng.Read(buf); err != nil {
		b.Fatalf("rng.Read: %v", err)
	}
	if _, err := bs.WriteAt(ctx, payloadID, nil, buf, 0); err != nil {
		b.Fatalf("WriteAt: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		b.Fatalf("DrainRollups: %v", err)
	}

	f, err := ms.GetFile(ctx, handle)
	if err != nil {
		b.Fatalf("GetFile: %v", err)
	}
	blocks := f.Blocks
	avg := warmRRFileSize / 1024
	if len(blocks) > 0 {
		avg /= len(blocks)
	}
	b.Logf("warm fixture: %d bytes, %d CAS chunks (avg %d KiB)", warmRRFileSize, len(blocks), avg)
	return bs, payloadID, blocks
}

// BenchmarkWarmRandRead_FS profiles a 4 KiB random read served from warm local
// CAS via the fs store. This is the CPU-attribution target.
func BenchmarkWarmRandRead_FS(b *testing.B) {
	bs, payloadID, blocks := setupWarmFSFixture(b)
	ctx := context.Background()
	dest := make([]byte, warmRRReadSize)
	rng := rand.New(rand.NewSource(warmRRSeed)) //nolint:gosec // bench
	maxOffset := warmRRFileSize - warmRRReadSize

	b.SetBytes(warmRRReadSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		offset := uint64(rng.Intn(maxOffset))
		if _, err := bs.ReadAt(ctx, payloadID, blocks, dest, offset); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}
	b.StopTimer()
}
