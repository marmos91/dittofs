package engine

import (
	"context"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// spyingRemoteStore wraps a remote.RemoteStore and counts how many times
// the CAS verified-read entry point is invoked. Post-Phase-17 the legacy
// fallback is gone; only the verified-read counter survives.
type spyingRemoteStore struct {
	remote.RemoteStore
	readVerifiedCalls atomic.Int64
	mu                gosync.Mutex
	readVerifiedKeys  []string
}

func newSpyingRemoteStore(inner remote.RemoteStore) *spyingRemoteStore {
	return &spyingRemoteStore{RemoteStore: inner}
}

func (s *spyingRemoteStore) ReadBlockVerified(ctx context.Context, hash, expected blockstore.ContentHash) ([]byte, error) {
	s.readVerifiedCalls.Add(1)
	s.mu.Lock()
	s.readVerifiedKeys = append(s.readVerifiedKeys, blockstore.FormatCASKey(hash))
	s.mu.Unlock()
	return s.RemoteStore.ReadBlockVerified(ctx, hash, expected)
}

// Healthcheck delegates so the syncer's HealthMonitor sees a healthy
// status; without this the wrapper would shadow the interface method
// with the default zero-value Report (fail-closed unhealthy).
func (s *spyingRemoteStore) Healthcheck(ctx context.Context) health.Report {
	return s.RemoteStore.Healthcheck(ctx)
}

// dualReadEnv is a self-contained syncer fixture using in-memory
// metadata + spying remote store. The syncer is NOT Started — tests
// drive fetchBlock directly so the periodic uploader does not race.
type dualReadEnv struct {
	tmp     string
	ms      *metadatamemory.MemoryMetadataStore
	rs      *spyingRemoteStore
	innerRS *remotememory.Store
	syncer  *Syncer
}

func newDualReadEnv(t *testing.T) *dualReadEnv {
	t.Helper()
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.NewWithOptions(tmp, 0, 0, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	inner := remotememory.New()
	rs := newSpyingRemoteStore(inner)

	cfg := DefaultConfig()
	cfg.UploadConcurrency = 2
	cfg.ClaimTimeout = 100 * time.Millisecond

	m := NewSyncer(bc, rs, ms, cfg)
	t.Cleanup(func() {
		_ = m.Close()
		_ = inner.Close()
	})
	return &dualReadEnv{tmp: tmp, ms: ms, rs: rs, innerRS: inner, syncer: m}
}

func dualReadHash(data []byte) blockstore.ContentHash {
	sum := blake3.Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

// TestDualRead_CASRowRoutesToVerified asserts a FileBlock with a non-zero
// Hash routes through ReadBlockVerified using the renamed (hash, expected)
// argument signature.
func TestDualRead_CASRowRoutesToVerified(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/cas-file"
	data := []byte("CAS path bytes — verified read on fetch")
	hash := dualReadHash(data)
	casKey := blockstore.FormatCASKey(hash)

	// Stash bytes in the remote at the CAS key with the matching
	// content-hash header.
	if err := env.innerRS.Put(ctx, hash, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	// Register the FileBlock metadata: Hash set, BlockStoreKey = casKey
	// State = Remote (post-Phase-11 row).
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		Hash:          hash,
		DataSize:      uint32(len(data)),
		BlockStoreKey: casKey,
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("fetchBlock data mismatch: got %q, want %q", got, data)
	}

	if env.rs.readVerifiedCalls.Load() != 1 {
		t.Errorf("ReadBlockVerified calls = %d, want 1", env.rs.readVerifiedCalls.Load())
	}
	if got := env.rs.readVerifiedKeys[0]; got != casKey {
		t.Errorf("ReadBlockVerified key = %q, want %q", got, casKey)
	}
}

// TestDualRead_NoFileBlockReturnsNil asserts that a missing metadata row
// (sparse / never uploaded) yields no remote call and a nil result.
func TestDualRead_NoFileBlockReturnsNil(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	got, err := env.syncer.fetchBlock(ctx, "share/missing", 0)
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if got != nil {
		t.Fatalf("fetchBlock data = %v, want nil for sparse block", got)
	}
	if env.rs.readVerifiedCalls.Load() != 0 {
		t.Errorf("expected zero verified-read calls for sparse block, got %d",
			env.rs.readVerifiedCalls.Load())
	}
}

// TestDualRead_CASRowMismatchSurfacesError asserts that bytes that fail
// BLAKE3 verification are surfaced as ErrCASContentMismatch through the
// engine read path (plumbed end-to-end).
func TestDualRead_CASRowMismatchSurfacesError(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/cas-mismatch"
	expected := []byte("expected payload — caller asks for THIS hash")
	wrongBytes := []byte("WRONG bytes — should fail body recompute")
	hash := dualReadHash(expected)
	casKey := blockstore.FormatCASKey(hash)

	// Stash WRONG bytes under the EXPECTED hash. The memory backend's
	// Put accepts the caller-supplied hash as the key without recomputing
	// — a deliberate seam for corruption tests like this one. The
	// downstream ReadBlockVerified re-hashes the stored bytes and surfaces
	// ErrCASContentMismatch when they fail to match `expected`.
	if err := env.innerRS.Put(ctx, hash, wrongBytes); err != nil {
		t.Fatalf("seed corrupted remote: %v", err)
	}

	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		Hash:          hash,
		DataSize:      uint32(len(expected)),
		BlockStoreKey: casKey,
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock: %v", err)
	}

	_, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatal("fetchBlock: expected ErrCASContentMismatch, got nil")
	}
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrCASContentMismatch", err)
	}

	// Verified path must have been chosen.
	if env.rs.readVerifiedCalls.Load() != 1 {
		t.Errorf("ReadBlockVerified calls = %d, want 1", env.rs.readVerifiedCalls.Load())
	}
}

// TestDualRead_CASMissingObjectFailsClosed: a row
// with a non-zero hash whose CAS object is absent from the remote MUST
// surface as ErrChunkNotFound, NOT silently return zeros.
// fail-closed makes this state structurally impossible under correct GC
// but if a bug ever lets a live CAS object get reaped, the read path
// should fail loudly rather than corrupt the caller's data.
func TestDualRead_CASMissingObjectFailsClosed(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/cas-missing"
	hash := dualReadHash([]byte("expected payload"))
	casKey := blockstore.FormatCASKey(hash)

	// Register a CAS-shaped FileBlock but DO NOT seed the remote object.
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		Hash:          hash,
		DataSize:      32,
		BlockStoreKey: casKey,
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatalf("fetchBlock: expected ErrChunkNotFound, got nil with data=%v", got)
	}
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrChunkNotFound", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on fail-closed CAS miss", got)
	}
}

// TestDualRead_LegacyRowRefusedPostMigration: a FileBlock
// with a zero ContentHash that reaches dispatchRemoteFetch is migration
// drift. 's boot guard refuses to start against an un-migrated
// store, but if the sentinel is lost or hand-removed and a stray legacy-
// shaped row surfaces at runtime, the read path MUST refuse rather than
// silently return zeros. This is the replacement for the pre-Phase-17
// per-share BlockLayout gate.
func TestDualRead_LegacyRowRefusedPostMigration(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/legacy-row"
	// Synthesize the legacy "{payloadID}/block-{idx}" key shape directly
	// the helper was deleted in with the rest of the legacy
	// path-keyed surface.
	legacyKey := payloadID + "/block-0"

	// Legacy-shaped FileBlock: Hash zero, BlockStoreKey set.
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		DataSize:      32,
		BlockStoreKey: legacyKey,
		State:         blockstore.BlockStatePending,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatalf("fetchBlock: expected legacy-row refusal, got nil with data=%v", got)
	}
	// The error message carries the block_id so operators can triage
	// which row the migration tool missed.
	if got := err.Error(); !contains(got, "legacy zero-hash FileBlock") || !contains(got, payloadID) {
		t.Errorf("fetchBlock err = %v, want one mentioning legacy zero-hash + payloadID", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on legacy refusal", got)
	}
	if env.rs.readVerifiedCalls.Load() != 0 {
		t.Errorf("ReadBlockVerified calls = %d, want 0 (legacy row has no hash)", env.rs.readVerifiedCalls.Load())
	}
}

// contains is a tiny helper that returns true if s contains substr (used
// to keep the error-message assertion above readable).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
