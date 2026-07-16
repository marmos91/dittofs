package common

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newTestEngine constructs an engine.Store backed by an on-disk
// FSStore rooted at a temp dir, mirroring the engine package's test
// helper. Used here instead of a mock because *engine.Store is a
// concrete struct and the helper under test takes it directly.
//
// The FSStore is constructed with an inline SyncedHashStore + a tight
// stabilization window, and StartRollup is
// invoked so AppendWrite-staged bytes flow through the
// rollup → CAS chunk → FileChunk-row pipeline that the engine's CAS
// read path consumes.
func newTestEngine(t *testing.T) *engine.Store {
	t.Helper()

	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(tmpDir, 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions failed: %v", err)
	}

	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())

	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileChunkStore:  ms,
		ReadBufferBytes: 0,
		PrefetchWorkers: 0,
	})
	if err != nil {
		t.Fatalf("engine.New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	return bs
}

// forceRollup is a no-op in the journal model: WriteAt lands bytes in the
// journal's interval index that ReadAt serves warm immediately, so there is no
// rollup pass to drive before reading back. Retained so the call sites read
// intentionally.
func forceRollup(_ *testing.T, _ *engine.Store, _ string) {}

// TestWriteToBlockStore_Passthrough asserts the Phase-09 passthrough contract:
// WriteToBlockStore writes identical bytes at identical offsets to the
// underlying engine. Verifies by round-tripping through engine.ReadAt.
func TestWriteToBlockStore_Passthrough(t *testing.T) {
	ctx := context.Background()
	bs := newTestEngine(t)

	payloadID := "test-payload-passthrough"
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	if err := WriteToBlockStore(ctx, bs, metadata.PayloadID(payloadID), data, 0); err != nil {
		t.Fatalf("WriteToBlockStore returned error: %v", err)
	}
	forceRollup(t, bs, payloadID)

	// Round-trip: read back via engine.ReadAt and compare bytes.
	readBack := make([]byte, len(data))
	n, readErr := bs.ReadAt(ctx, payloadID, nil, readBack, 0)
	if readErr != nil {
		t.Fatalf("engine.ReadAt after WriteToBlockStore failed: %v", readErr)
	}
	if n != len(data) {
		t.Fatalf("engine.ReadAt returned %d bytes, want %d", n, len(data))
	}
	for i := range data {
		if readBack[i] != data[i] {
			t.Fatalf("byte %d: got 0x%02x, want 0x%02x", i, readBack[i], data[i])
		}
	}
}

// TestWriteToBlockStore_OffsetRespected asserts that the offset is passed
// through verbatim — writes at offset 4096 land at offset 4096, not 0.
func TestWriteToBlockStore_OffsetRespected(t *testing.T) {
	ctx := context.Background()
	bs := newTestEngine(t)

	payloadID := "test-payload-offset"
	const offset = uint64(4096)
	data := []byte("hello-offset-4096")

	if err := WriteToBlockStore(ctx, bs, metadata.PayloadID(payloadID), data, offset); err != nil {
		t.Fatalf("WriteToBlockStore returned error: %v", err)
	}
	forceRollup(t, bs, payloadID)

	readBack := make([]byte, len(data))
	n, readErr := bs.ReadAt(ctx, payloadID, nil, readBack, offset)
	if readErr != nil {
		t.Fatalf("engine.ReadAt at offset %d failed: %v", offset, readErr)
	}
	if n != len(data) {
		t.Fatalf("engine.ReadAt returned %d bytes, want %d", n, len(data))
	}
	if string(readBack) != string(data) {
		t.Fatalf("got %q, want %q", readBack, data)
	}
}

// TestWriteToBlockStore_EmptyData asserts the engine's documented behaviour
// for len(data)==0: WriteAt returns nil (no-op). WriteToBlockStore must
// surface that verbatim — no translation, no wrapping.
func TestWriteToBlockStore_EmptyData(t *testing.T) {
	ctx := context.Background()
	bs := newTestEngine(t)

	// engine.WriteAt returns nil for len(data)==0 without touching the store.
	if err := WriteToBlockStore(ctx, bs, metadata.PayloadID("test-empty"), nil, 0); err != nil {
		t.Errorf("WriteToBlockStore(nil data) = %v, want nil (engine no-op contract)", err)
	}
	if err := WriteToBlockStore(ctx, bs, metadata.PayloadID("test-empty"), []byte{}, 0); err != nil {
		t.Errorf("WriteToBlockStore(empty data) = %v, want nil (engine no-op contract)", err)
	}
}

// TestWriteToBlockStore_SingleErrorReturn is a compile-time guarantee that
// WriteToBlockStore returns `error` only (not (int, error)). Any regression
// that widens the signature to (int, error) — mirroring a latent misuse of
// engine.WriteAt — will fail this file to compile.
func TestWriteToBlockStore_SingleErrorReturn(t *testing.T) {
	ctx := context.Background()
	bs := newTestEngine(t)

	// The following assignment only type-checks if WriteToBlockStore returns
	// exactly one error value. If someone widens the signature to
	// (int, error), `err := ...` will fail to compile here.
	err := WriteToBlockStore(ctx, bs, metadata.PayloadID("single-return"), []byte("x"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
