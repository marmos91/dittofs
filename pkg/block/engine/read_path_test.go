package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReadPath_BlockLocator_Plaintext verifies that fetchResolvedBlock routes a
// block-keyed locator through readChunkVerified and returns the original bytes.
func TestReadPath_BlockLocator_Plaintext(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("read-path-plain-"), 512)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (plaintext)")
	}
}

// TestReadPath_BlockLocator_ThroughCompress verifies that fetchResolvedBlock
// correctly reads back a chunk that was carved through a compressed remote.
func TestReadPath_BlockLocator_ThroughCompress(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	dec, err := compression.NewRemote(base, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, dec, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("XXXX-YYYY-ZZZZ-"), 4096)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	loc, synced, lerr := f.ms.GetLocator(ctx, h)
	if lerr != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, lerr)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock (compress): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (compressed remote)")
	}
}

// TestReadPath_BlockLocator_ThroughCompressEncrypt verifies that fetchResolvedBlock
// correctly reads back a chunk carved through a compression(encryption(base)) stack.
func TestReadPath_BlockLocator_ThroughCompressEncrypt(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	enc, err := encryption.NewRemote(base, encryption.EncryptionPolicy{AEAD: encryption.AEADAES256GCM}, newEncryptionProvider(t))
	if err != nil {
		t.Fatalf("encryption.NewRemote: %v", err)
	}
	dec, err := compression.NewRemote(enc, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, dec, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("secret-read-path-"), 1024)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	loc, synced, lerr := f.ms.GetLocator(ctx, h)
	if lerr != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, lerr)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock (compress+encrypt): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (compress+encrypt remote)")
	}
}

// TestReadPath_StandaloneLocatorServedViaFallback: a synced hash whose recorded
// locator is still standalone (BlockID == "") — a chunk the now-background
// cas→blocks migration has not repacked yet — is served
// through the legacy CAS fallback, byte-identical, instead of being refused.
// This is what lets the share serve immediately while the migration runs in the
// background. The genuine-data-loss case (bytes resident nowhere) is covered by
// TestReadPath_StandaloneLocatorMissingEverywhere below.
func TestReadPath_StandaloneLocatorServedViaFallback(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, metadatamemory.NewMemoryMetadataStoreWithDefaults(), fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	syncer := NewSyncer(local, mem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	// Wire the block-keyed remote like production (shares/service.go) and
	// carveFixture do — it's the object the legacy-CAS fallback reads through.
	syncer.SetRemoteBlockStore(mem)

	data := []byte("cas-back-compat-payload")
	h := block.ContentHash(blake3.Sum256(data))

	// Plant the legacy standalone cas/ object (a pre-#1414 upload shape) plus a
	// standalone locator (BlockID == "") — the exact pre-flip state a synced
	// hash carries before the background migration repacks it.
	if err := mem.PutLegacyChunk(ctx, h, data); err != nil {
		t.Fatalf("PutLegacyChunk: %v", err)
	}
	if err := ms.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	got, err := syncer.fetchResolvedBlock(ctx, &block.FileChunk{ID: "share/standalone/0", Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock: want standalone fallback to serve, got err=%v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("fetchResolvedBlock standalone fallback returned %d bytes, want the planted payload", len(got))
	}
}

// TestReadPath_StandaloneLocatorMissingEverywhere proves the genuine
// data-loss case is still fail-closed: a standalone (empty-BlockID) marker whose
// bytes are resident nowhere — no local copy, no legacy cas/ object — surfaces
// ErrChunkNotFound rather than serving zeros.
func TestReadPath_StandaloneLocatorMissingEverywhere(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	// A standalone marker with no bytes anywhere (nothing planted on the remote,
	// nothing stored locally).
	h := block.ContentHash(blake3.Sum256([]byte("vanished-standalone")))
	if err := f.ms.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{ID: "share/lost/0", Hash: h})
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("fetchResolvedBlock: want ErrChunkNotFound, got err=%v", err)
	}
	if got != nil {
		t.Fatalf("fetchResolvedBlock data = %d bytes, want nil on missing chunk", len(got))
	}
}

// TestReadPath_CorruptBlock_FailClosed verifies that readChunkVerified returns
// ErrChunkContentMismatch when the chunk is read back with the wrong expected hash.
func TestReadPath_CorruptBlock_FailClosed(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("corrupt-check-data-"), 256)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	// Build a wrong hash by flipping every byte of h.
	var wrongHash block.ContentHash
	for i, b := range h {
		wrongHash[i] = ^b
	}

	_, err = f.syncer.readChunkVerified(ctx, loc, wrongHash)
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("readChunkVerified with wrong hash: want ErrChunkContentMismatch, got %v", err)
	}
}
