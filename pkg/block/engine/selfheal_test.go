package engine

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ===== stubMetrics =====

// stubMetrics counts calls to the 5 new DataplaneMetrics methods.
// All 11 DataplaneMetrics methods are implemented; the 6 legacy ones are no-ops.
type stubMetrics struct {
	localCorruptions  atomic.Int32
	selfHealSuccesses atomic.Int32
	selfHealFailures  atomic.Int32
	remoteCorruptions atomic.Int32
	blockRangeReads   atomic.Int32
	blockRangeBytes   atomic.Int64
}

// Legacy 6 methods — no-ops.
func (s *stubMetrics) RecordUpload(_ int, _ string, _ time.Duration) {}
func (s *stubMetrics) UploadStarted()                                {}
func (s *stubMetrics) UploadFinished()                               {}
func (s *stubMetrics) SetUploadQueueDepth(_ int)                     {}
func (s *stubMetrics) SetUploadWindow(_ int)                         {}
func (s *stubMetrics) RecordRehash(_ time.Duration)                  {}

// New 5 methods — increment counters.
func (s *stubMetrics) RecordLocalCorruption(n int)  { s.localCorruptions.Add(int32(n)) }
func (s *stubMetrics) RecordSelfHealSuccess(n int)  { s.selfHealSuccesses.Add(int32(n)) }
func (s *stubMetrics) RecordSelfHealFailure(n int)  { s.selfHealFailures.Add(int32(n)) }
func (s *stubMetrics) RecordRemoteCorruption(n int) { s.remoteCorruptions.Add(int32(n)) }
func (s *stubMetrics) RecordBlockRangeRead(bytes int) {
	s.blockRangeReads.Add(1)
	s.blockRangeBytes.Add(int64(bytes))
}

// ===== corruptingGetLocalStore =====

// corruptingGetLocalStore wraps a local.LocalStore and returns garbage bytes for
// specified hashes on Get.  Delete removes the hash from the corruption set (and
// delegates to the underlying store), so after a self-heal the re-Put chunk can
// be read back cleanly.  Put is promoted unchanged from the embedded interface.
type corruptingGetLocalStore struct {
	local.LocalStore
	mu            sync.Mutex
	corruptHashes map[block.ContentHash]struct{}
}

func newCorruptingGetLocalStore(inner local.LocalStore, corrupt ...block.ContentHash) *corruptingGetLocalStore {
	m := make(map[block.ContentHash]struct{}, len(corrupt))
	for _, h := range corrupt {
		m[h] = struct{}{}
	}
	return &corruptingGetLocalStore{LocalStore: inner, corruptHashes: m}
}

func (c *corruptingGetLocalStore) Get(ctx context.Context, h block.ContentHash) ([]byte, error) {
	c.mu.Lock()
	_, bad := c.corruptHashes[h]
	c.mu.Unlock()
	if bad {
		return []byte("corrupt-garbage-xxxx"), nil
	}
	return c.LocalStore.Get(ctx, h)
}

func (c *corruptingGetLocalStore) Delete(ctx context.Context, h block.ContentHash) error {
	c.mu.Lock()
	delete(c.corruptHashes, h)
	c.mu.Unlock()
	return c.LocalStore.Delete(ctx, h)
}

// ===== countingRemoteStore =====

// countingRemoteStore wraps *remotememory.Store and counts ReadBlockVerified calls.
type countingRemoteStore struct {
	*remotememory.Store
	readVerifiedCount atomic.Int32
}

func (s *countingRemoteStore) ReadBlockVerified(ctx context.Context, hash, expected block.ContentHash) ([]byte, error) {
	s.readVerifiedCount.Add(1)
	return s.Store.ReadBlockVerified(ctx, hash, expected)
}

// ===== newTestEngineWithRemoteAndSHS =====

// newTestEngineWithRemoteAndSHS creates a Store wired with a remote store and a
// SyncedHashStore, for the self-heal path tests.  The caller provides the local
// store (possibly a corruptingGetLocalStore wrapper) and the remote / SHS.
func newTestEngineWithRemoteAndSHS(t *testing.T, localStore local.LocalStore, remote *countingRemoteStore, shs metadata.SyncedHashStore) *Store {
	t.Helper()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, remote, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          remote,
		Syncer:          syncer,
		FileChunkStore:  fbs,
		SyncedHashStore: shs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// setStubMetrics wires sm as the engine's dataplane metrics sink.
// Uses the package-private metrics field directly (same package).
func setStubMetrics(bs *Store, sm *stubMetrics) {
	var dm DataplaneMetrics = sm
	bs.metrics.Store(&dm)
}

// ===== Tests =====

// TestReadLocalByHash_LocalCorruption_SyncedSelfHeals verifies that a corrupt
// local chunk is transparently replaced from the remote store, the read returns
// correct data, and metrics counters are incremented once.  A second read after
// the heal must NOT trigger another remote fetch.
func TestReadLocalByHash_LocalCorruption_SyncedSelfHeals(t *testing.T) {
	ctx := context.Background()
	correctData := []byte("hello self-heal world")

	// Build stores.
	inner := memory.New()
	correctHash := block.ContentHash(blake3.Sum256(correctData))
	corruptLocal := newCorruptingGetLocalStore(inner, correctHash)
	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	shs := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Seed: put correct bytes in both local (via inner) and remote.
	if err := inner.Put(ctx, correctHash, correctData); err != nil {
		t.Fatalf("Put local: %v", err)
	}
	if err := remoteRS.Put(ctx, correctHash, correctData); err != nil {
		t.Fatalf("Put remote: %v", err)
	}
	// Mark synced (standalone locator).
	if err := shs.MarkSynced(ctx, correctHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Build engine with the corrupting local store.
	bs := newTestEngineWithRemoteAndSHS(t, corruptLocal, remoteRS, shs)

	// Wire stub metrics.
	sm := &stubMetrics{}
	setStubMetrics(bs, sm)

	// Populate fileChunkStore directly — engine wires the emitter to it via New,
	// but we bypassed the write path.  Access the engine's fcs via the syncer.
	payloadID := "p1"
	fb := &block.FileChunk{
		ID:       payloadID + "/0",
		Hash:     correctHash,
		DataSize: uint32(len(correctData)),
		State:    block.BlockStatePending,
	}
	if err := bs.syncer.fileChunkStore.Put(ctx, fb); err != nil {
		t.Fatalf("fbs.Put: %v", err)
	}

	// First read: should detect corruption, self-heal, and return correct data.
	dest := make([]byte, len(correctData))
	found, err := bs.readLocalByHash(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("readLocalByHash returned error: %v", err)
	}
	if !found {
		t.Fatal("readLocalByHash returned found=false; expected found=true after self-heal")
	}
	if !bytes.Equal(dest, correctData) {
		t.Fatalf("data mismatch: got %q, want %q", dest, correctData)
	}

	// Metrics after first read.
	if got := sm.localCorruptions.Load(); got != 1 {
		t.Errorf("localCorruptions = %d, want 1", got)
	}
	if got := sm.selfHealSuccesses.Load(); got != 1 {
		t.Errorf("selfHealSuccesses = %d, want 1", got)
	}
	if got := sm.selfHealFailures.Load(); got != 0 {
		t.Errorf("selfHealFailures = %d, want 0", got)
	}
	if got := remoteRS.readVerifiedCount.Load(); got != 1 {
		t.Errorf("remote readVerifiedCount after first read = %d, want 1", got)
	}

	// Second read: corruption set cleared, local store has correct bytes — no extra fetch.
	dest2 := make([]byte, len(correctData))
	found2, err2 := bs.readLocalByHash(ctx, payloadID, dest2, 0)
	if err2 != nil {
		t.Fatalf("second readLocalByHash returned error: %v", err2)
	}
	if !found2 {
		t.Fatal("second readLocalByHash returned found=false; expected true")
	}
	if !bytes.Equal(dest2, correctData) {
		t.Fatalf("second read data mismatch: got %q, want %q", dest2, correctData)
	}
	if got := remoteRS.readVerifiedCount.Load(); got != 1 {
		t.Errorf("remote readVerifiedCount after second read = %d, want 1 (no extra fetch)", got)
	}
}

// TestReadLocalByHash_LocalCorruption_UnsyncedFailClosed verifies that a corrupt
// unsynced chunk causes a fail-closed error (no remote fetch attempted).
func TestReadLocalByHash_LocalCorruption_UnsyncedFailClosed(t *testing.T) {
	ctx := context.Background()
	correctData := []byte("unsynced chunk data")

	inner := memory.New()
	correctHash := block.ContentHash(blake3.Sum256(correctData))
	corruptLocal := newCorruptingGetLocalStore(inner, correctHash)
	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	shs := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Seed local only — do NOT MarkSynced.
	if err := inner.Put(ctx, correctHash, correctData); err != nil {
		t.Fatalf("Put local: %v", err)
	}

	bs := newTestEngineWithRemoteAndSHS(t, corruptLocal, remoteRS, shs)
	sm := &stubMetrics{}
	setStubMetrics(bs, sm)

	payloadID := "p2"
	fb := &block.FileChunk{
		ID:       payloadID + "/0",
		Hash:     correctHash,
		DataSize: uint32(len(correctData)),
		State:    block.BlockStatePending,
	}
	if err := bs.syncer.fileChunkStore.Put(ctx, fb); err != nil {
		t.Fatalf("fbs.Put: %v", err)
	}

	dest := make([]byte, len(correctData))
	found, err := bs.readLocalByHash(ctx, payloadID, dest, 0)
	if found {
		t.Fatal("readLocalByHash returned found=true; expected false (fail-closed)")
	}
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("error = %v; want ErrCASContentMismatch", err)
	}

	if got := sm.localCorruptions.Load(); got != 1 {
		t.Errorf("localCorruptions = %d, want 1", got)
	}
	if got := sm.selfHealFailures.Load(); got != 1 {
		t.Errorf("selfHealFailures = %d, want 1", got)
	}
	if got := sm.selfHealSuccesses.Load(); got != 0 {
		t.Errorf("selfHealSuccesses = %d, want 0", got)
	}
	if got := remoteRS.readVerifiedCount.Load(); got != 0 {
		t.Errorf("remote readVerifiedCount = %d, want 0 (no fetch for unsynced)", got)
	}
}

// TestReadChunkVerified_RemoteCorruption_RecordsMetric verifies that a remote
// chunk whose bytes don't match the expected BLAKE3 hash records a remote
// corruption metric and returns ErrCASContentMismatch.
func TestReadChunkVerified_RemoteCorruption_RecordsMetric(t *testing.T) {
	ctx := context.Background()

	// Correct and garbage bytes.
	correctData := []byte("hello")
	garbageData := []byte("garb!")
	correctHash := block.ContentHash(blake3.Sum256(correctData))

	// Build remote with garbage stored at blockID "blk1".
	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	if err := remoteRS.PutBlock(ctx, "blk1", bytes.NewReader(garbageData)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// SHS: mark correctHash synced with a block locator pointing to blk1.
	shs := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	loc := block.ChunkLocator{BlockID: "blk1", WireOffset: 0, WireLength: int64(len(garbageData))}
	if err := shs.MarkSynced(ctx, correctHash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	inner := memory.New()
	bs := newTestEngineWithRemoteAndSHS(t, inner, remoteRS, shs)
	sm := &stubMetrics{}
	setStubMetrics(bs, sm)

	// FileChunk whose Hash is blake3(correctData) — the expected hash.
	fb := &block.FileChunk{
		ID:       "p3/0",
		Hash:     correctHash,
		DataSize: uint32(len(correctData)),
		State:    block.BlockStatePending,
	}

	// dispatchRemoteFetch: resolves block locator → readChunkVerified → mismatch.
	_, _, err := bs.syncer.dispatchRemoteFetch(ctx, fb)
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("error = %v; want ErrCASContentMismatch", err)
	}

	if got := sm.remoteCorruptions.Load(); got != 1 {
		t.Errorf("remoteCorruptions = %d, want 1", got)
	}
}

// TestReadLocalByHash_LocalCorrupt_SyncedButRemoteAlsoCorrupt covers the
// combined failure path: a corrupt local chunk that IS synced but whose remote
// block copy is ALSO corrupt must fail closed (ErrCASContentMismatch), never
// serve corrupt bytes, and record one local corruption, one remote corruption,
// and one self-heal failure.
func TestReadLocalByHash_LocalCorrupt_SyncedButRemoteAlsoCorrupt(t *testing.T) {
	ctx := context.Background()
	correctData := []byte("heal path but remote also corrupt")
	garbageData := []byte("remote-garbage-yyyy")
	correctHash := block.ContentHash(blake3.Sum256(correctData))

	inner := memory.New()
	corruptLocal := newCorruptingGetLocalStore(inner, correctHash)
	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	shs := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Local holds (corrupt-on-Get) bytes; the remote block holds GARBAGE.
	if err := inner.Put(ctx, correctHash, correctData); err != nil {
		t.Fatalf("Put local: %v", err)
	}
	if err := remoteRS.PutBlock(ctx, "blkbad", bytes.NewReader(garbageData)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	// Synced with a block locator pointing at the garbage block.
	loc := block.ChunkLocator{BlockID: "blkbad", WireOffset: 0, WireLength: int64(len(garbageData))}
	if err := shs.MarkSynced(ctx, correctHash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	bs := newTestEngineWithRemoteAndSHS(t, corruptLocal, remoteRS, shs)
	sm := &stubMetrics{}
	setStubMetrics(bs, sm)

	payloadID := "p5"
	fb := &block.FileChunk{
		ID:       payloadID + "/0",
		Hash:     correctHash,
		DataSize: uint32(len(correctData)),
		State:    block.BlockStatePending,
	}
	if err := bs.syncer.fileChunkStore.Put(ctx, fb); err != nil {
		t.Fatalf("fbs.Put: %v", err)
	}

	dest := make([]byte, len(correctData))
	found, err := bs.readLocalByHash(ctx, payloadID, dest, 0)
	if found {
		t.Fatal("readLocalByHash returned found=true; expected false (fail-closed, remote also corrupt)")
	}
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("error = %v; want ErrCASContentMismatch", err)
	}

	if got := sm.localCorruptions.Load(); got != 1 {
		t.Errorf("localCorruptions = %d, want 1", got)
	}
	if got := sm.remoteCorruptions.Load(); got != 1 {
		t.Errorf("remoteCorruptions = %d, want 1", got)
	}
	if got := sm.selfHealFailures.Load(); got != 1 {
		t.Errorf("selfHealFailures = %d, want 1", got)
	}
	if got := sm.selfHealSuccesses.Load(); got != 0 {
		t.Errorf("selfHealSuccesses = %d, want 0", got)
	}
}

// TestReadLocalByHash_NilMetrics_NocrashWithCorruption verifies that corrupt
// unsynced chunks fail cleanly even when no metrics recorder is wired.
func TestReadLocalByHash_NilMetrics_NocrashWithCorruption(t *testing.T) {
	ctx := context.Background()
	correctData := []byte("unsynced no metrics")

	inner := memory.New()
	correctHash := block.ContentHash(blake3.Sum256(correctData))
	corruptLocal := newCorruptingGetLocalStore(inner, correctHash)
	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	shs := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	if err := inner.Put(ctx, correctHash, correctData); err != nil {
		t.Fatalf("Put local: %v", err)
	}

	// No MarkSynced — unsynced chunk.
	bs := newTestEngineWithRemoteAndSHS(t, corruptLocal, remoteRS, shs)
	// Do NOT wire any metrics — bs.metrics remains nil.

	payloadID := "p4"
	fb := &block.FileChunk{
		ID:       payloadID + "/0",
		Hash:     correctHash,
		DataSize: uint32(len(correctData)),
		State:    block.BlockStatePending,
	}
	if err := bs.syncer.fileChunkStore.Put(ctx, fb); err != nil {
		t.Fatalf("fbs.Put: %v", err)
	}

	dest := make([]byte, len(correctData))
	found, err := bs.readLocalByHash(ctx, payloadID, dest, 0)
	if found {
		t.Fatal("readLocalByHash returned found=true; expected false")
	}
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("error = %v; want ErrCASContentMismatch", err)
	}
	// If we reach here without panicking, the nil-metrics guard works.
}
