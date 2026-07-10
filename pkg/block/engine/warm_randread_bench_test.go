package engine_test

// Warm local random-read benchmark + regression guard for the warm-read fast
// path (verify-once cache + ranged sub-chunk reads).
//
// Unlike BenchmarkRandRead_Phase12 (memory local store, no disk, no blake3),
// this drives the REAL fs local store: write a multi-MiB payload, DrainRollups
// so the bytes live as rolled-up CAS chunks (WARM local read, no remote), then
// do 4 KiB reads at random offsets through engine.Store.ReadAt — the exact
// production warm path (ReadPayloadAt -> fillFromCASManifest -> ReadChunk).
// Remote is nil so scheduleReadahead early-returns.
//
// Run:
//   go test -run=XXX -bench=BenchmarkWarmRandRead_FS -benchtime=3s \
//     -cpuprofile=/tmp/rr.prof -memprofile=/tmp/rr.mem ./pkg/block/engine/

import (
	"bytes"
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
	bs, payloadID, blocks, _ := buildWarmFSFixture(b)
	return bs, payloadID, blocks
}

// buildWarmFSFixture is the shared fixture builder used by the benchmark and the
// ranged-read correctness test. It also returns the source bytes so the test can
// assert reads are byte-identical. tb is *testing.B or *testing.T.
func buildWarmFSFixture(tb testing.TB) (*engine.Store, string, []block.ChunkRef, []byte) {
	tb.Helper()
	logger.SetLevel("ERROR")
	tb.Cleanup(func() { logger.SetLevel("INFO") })
	ctx := context.Background()

	ms := metastore.NewMemoryMetadataStoreWithDefaults()
	shareName := "/warm"

	// --- share + file row (mirrors createShare/createRealFile) ---
	rootFile, err := ms.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		tb.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		tb.Fatalf("EncodeFileHandle: %v", err)
	}
	handle, err := ms.GenerateHandle(ctx, shareName, "/data.bin")
	if err != nil {
		tb.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		tb.Fatalf("DecodeFileHandle: %v", err)
	}
	payloadID := "warm/" + id.String()
	if err := ms.PutFile(ctx, &metadata.File{
		ID: id, ShareName: shareName, Path: "/data.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000,
			PayloadID: metadata.PayloadID(payloadID),
		},
	}); err != nil {
		tb.Fatalf("PutFile: %v", err)
	}
	if err := ms.SetParent(ctx, handle, rootHandle); err != nil {
		tb.Fatalf("SetParent: %v", err)
	}
	if err := ms.SetChild(ctx, rootHandle, "data.bin", handle); err != nil {
		tb.Fatalf("SetChild: %v", err)
	}
	if err := ms.SetLinkCount(ctx, handle, 1); err != nil {
		tb.Fatalf("SetLinkCount: %v", err)
	}

	// --- fs engine (mirrors newEngineOverStore; nil remote) ---
	localStore, err := fs.NewWithOptions(tb.TempDir(), 256*1024*1024, ms, fs.FSStoreOptions{
		LocalChunkIndex: ms,
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 3_600_000, // async rollup never fires; only explicit DrainRollups
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		tb.Fatalf("fs.NewWithOptions: %v", err)
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
		tb.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		tb.Fatalf("engine.Start: %v", err)
	}
	tb.Cleanup(func() { _ = bs.Close() })

	// --- write payload, then drain to CAS (warm local) ---
	rng := rand.New(rand.NewSource(warmRRSeed)) //nolint:gosec // bench fixture
	buf := make([]byte, warmRRFileSize)
	if _, err := rng.Read(buf); err != nil {
		tb.Fatalf("rng.Read: %v", err)
	}
	if _, err := bs.WriteAt(ctx, payloadID, nil, buf, 0); err != nil {
		tb.Fatalf("WriteAt: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		tb.Fatalf("DrainRollups: %v", err)
	}

	f, err := ms.GetFile(ctx, handle)
	if err != nil {
		tb.Fatalf("GetFile: %v", err)
	}
	return bs, payloadID, f.Blocks, buf
}

// BenchmarkWarmRandRead_FS profiles a 4 KiB random read served from warm local
// CAS via the fs store.
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

// TestWarmRandRead_RangedReadByteIdentical guards the ranged sub-chunk fast
// path: the SECOND read of a chunk (after the first read verifies + caches its
// hash) is served by a ranged pread rather than a whole-chunk read, and must
// return bytes byte-identical to the source. It exercises varied offsets and
// lengths, including reads that straddle chunk boundaries, and reads every
// offset twice so both the verify-and-cache path and the ranged path are hit.
func TestWarmRandRead_RangedReadByteIdentical(t *testing.T) {
	bs, payloadID, blocks, src := buildWarmFSFixture(t)
	ctx := context.Background()

	rng := rand.New(rand.NewSource(warmRRSeed + 1)) //nolint:gosec // test
	sizes := []int{1, 100, 4096, 65537, 1 << 20}    // last spans multiple ~1 MiB chunks
	for _, sz := range sizes {
		if sz > len(src) {
			continue
		}
		maxOffset := len(src) - sz
		for iter := 0; iter < 50; iter++ {
			off := 0
			if maxOffset > 0 {
				off = rng.Intn(maxOffset)
			}
			dest := make([]byte, sz)
			// Read twice: first primes the verified set (whole-chunk verify),
			// second is served by the ranged fast path.
			for pass := 0; pass < 2; pass++ {
				clear(dest)
				n, err := bs.ReadAt(ctx, payloadID, blocks, dest, uint64(off))
				if err != nil {
					t.Fatalf("ReadAt(off=%d sz=%d pass=%d): %v", off, sz, pass, err)
				}
				if n != sz {
					t.Fatalf("short read off=%d sz=%d pass=%d: got %d", off, sz, pass, n)
				}
				if !bytes.Equal(dest, src[off:off+sz]) {
					t.Fatalf("byte mismatch off=%d sz=%d pass=%d", off, sz, pass)
				}
			}
		}
	}
}
