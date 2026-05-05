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
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// spyingRemoteStore wraps a remote.RemoteStore and counts how many times
// each fetch method is invoked, recording the keys requested. Used by the
// dual-read tests to assert that the engine routes through ReadBlockVerified
// for CAS rows and ReadBlock for legacy rows (D-21).
type spyingRemoteStore struct {
	remote.RemoteStore
	readCalls         atomic.Int64
	readVerifiedCalls atomic.Int64
	mu                gosync.Mutex
	readKeys          []string
	readVerifiedKeys  []string
}

func newSpyingRemoteStore(inner remote.RemoteStore) *spyingRemoteStore {
	return &spyingRemoteStore{RemoteStore: inner}
}

func (s *spyingRemoteStore) ReadBlock(ctx context.Context, key string) ([]byte, error) {
	s.readCalls.Add(1)
	s.mu.Lock()
	s.readKeys = append(s.readKeys, key)
	s.mu.Unlock()
	return s.RemoteStore.ReadBlock(ctx, key)
}

func (s *spyingRemoteStore) ReadBlockVerified(ctx context.Context, key string, expected blockstore.ContentHash) ([]byte, error) {
	s.readVerifiedCalls.Add(1)
	s.mu.Lock()
	s.readVerifiedKeys = append(s.readVerifiedKeys, key)
	s.mu.Unlock()
	return s.RemoteStore.ReadBlockVerified(ctx, key, expected)
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
	return newDualReadEnvWithLayout(t, metadata.BlockLayoutLegacy)
}

// newDualReadEnvWithLayout builds the dual-read fixture with an
// explicit per-share BlockLayout. The Plan 14-02 gate inside
// dispatchRemoteFetch consults this value before the legacy fallback.
func newDualReadEnvWithLayout(t *testing.T, layout metadata.BlockLayout) *dualReadEnv {
	t.Helper()
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmp, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	inner := remotememory.New()
	rs := newSpyingRemoteStore(inner)

	cfg := DefaultConfig()
	cfg.ClaimBatchSize = 4
	cfg.UploadConcurrency = 2
	cfg.ClaimTimeout = 100 * time.Millisecond
	cfg.BlockLayout = layout

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
// Hash routes through ReadBlockVerified using FormatCASKey(Hash).
func TestDualRead_CASRowRoutesToVerified(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/cas-file"
	data := []byte("CAS path bytes — verified read on fetch")
	hash := dualReadHash(data)
	casKey := blockstore.FormatCASKey(hash)

	// Stash bytes in the remote with the matching content-hash header.
	if err := env.innerRS.WriteBlockWithHash(ctx, casKey, hash, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	// Register the FileBlock metadata: Hash set, BlockStoreKey = casKey,
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
	if env.rs.readCalls.Load() != 0 {
		t.Errorf("ReadBlock calls = %d, want 0 (CAS path must not fall back to legacy)", env.rs.readCalls.Load())
	}
	if got := env.rs.readVerifiedKeys[0]; got != casKey {
		t.Errorf("ReadBlockVerified key = %q, want %q", got, casKey)
	}
}

// TestDualRead_LegacyRowRoutesToReadBlock asserts a FileBlock with a zero
// Hash but non-empty BlockStoreKey routes through ReadBlock (no
// verification possible — legacy bytes were never hashed).
func TestDualRead_LegacyRowRoutesToReadBlock(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/legacy-file"
	data := []byte("legacy {payloadID}/block-N path; no hash on the row")
	legacyKey := blockstore.FormatStoreKey(payloadID, 0)

	// Seed remote via legacy WriteBlock (no header, no hash recorded).
	if err := env.innerRS.WriteBlock(ctx, legacyKey, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	// Pre-Phase-11 row shape: Hash zero, BlockStoreKey set, State Pending
	// (per IsRemote dual-read fallback at types.go).
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		DataSize:      uint32(len(data)),
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
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("fetchBlock data mismatch: got %q, want %q", got, data)
	}

	if env.rs.readVerifiedCalls.Load() != 0 {
		t.Errorf("ReadBlockVerified calls = %d, want 0 (legacy must not verify)", env.rs.readVerifiedCalls.Load())
	}
	if env.rs.readCalls.Load() != 1 {
		t.Errorf("ReadBlock calls = %d, want 1", env.rs.readCalls.Load())
	}
	if got := env.rs.readKeys[0]; got != legacyKey {
		t.Errorf("ReadBlock key = %q, want %q", got, legacyKey)
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
	if env.rs.readVerifiedCalls.Load()+env.rs.readCalls.Load() != 0 {
		t.Errorf("expected zero remote calls for sparse block, got verified=%d read=%d",
			env.rs.readVerifiedCalls.Load(), env.rs.readCalls.Load())
	}
}

// TestDualRead_CASRowMismatchSurfacesError asserts that bytes that fail
// BLAKE3 verification are surfaced as ErrCASContentMismatch through the
// engine read path (INV-06 plumbed end-to-end).
func TestDualRead_CASRowMismatchSurfacesError(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/cas-mismatch"
	expected := []byte("expected payload — caller asks for THIS hash")
	wrongBytes := []byte("WRONG bytes — should fail body recompute")
	hash := dualReadHash(expected)
	casKey := blockstore.FormatCASKey(hash)

	// Stash the WRONG bytes at the CAS key via legacy WriteBlock so the
	// header pre-check is inert and the body recompute fires.
	if err := env.innerRS.WriteBlock(ctx, casKey, wrongBytes); err != nil {
		t.Fatalf("seed remote: %v", err)
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

// TestDualRead_CASMissingObjectFailsClosed (Phase 11 IN-3-05): a row
// with a non-zero hash whose CAS object is absent from the remote MUST
// surface as ErrBlockNotFound, NOT silently return zeros. INV-04
// fail-closed makes this state structurally impossible under correct GC,
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
		t.Fatalf("fetchBlock: expected ErrBlockNotFound, got nil with data=%v", got)
	}
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrBlockNotFound", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on fail-closed CAS miss", got)
	}
}

// TestDualRead_LegacyMissingObjectReturnsNil (Phase 11 IN-3-05): the
// fail-closed change in TestDualRead_CASMissingObjectFailsClosed must
// NOT regress legacy-path semantics. A row with zero hash whose
// {payloadID}/block-N object is absent represents a sparse / never-
// uploaded block per the dual-read contract — silent zero is correct.
func TestDualRead_LegacyMissingObjectReturnsNil(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/legacy-missing"
	legacyKey := blockstore.FormatStoreKey(payloadID, 0)

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
	if err != nil {
		t.Fatalf("fetchBlock: legacy missing should be silent zero, got %v", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil for legacy sparse block", got)
	}
}

// TestDualRead_CASOnly_RefusesLegacyFallback (Plan 14-02 / MIG-03 / D-A8):
// when the share's BlockLayout is cas-only, encountering a legacy-shaped
// FileBlock (zero ContentHash, populated BlockStoreKey) MUST NOT silently
// fall through to ReadBlock — that fallback is the exact behavior we want
// to defeat after a successful migration. Surface ErrLegacyReadOnCASOnly
// so operators see live-data-loss signals instead of stale bytes.
func TestDualRead_CASOnly_RefusesLegacyFallback(t *testing.T) {
	env := newDualReadEnvWithLayout(t, metadata.BlockLayoutCASOnly)
	ctx := context.Background()

	const payloadID = "share/cas-only-with-legacy-row"
	data := []byte("legacy bytes that must NOT be served on a cas-only share")
	legacyKey := blockstore.FormatStoreKey(payloadID, 0)

	// Seed the remote so that a successful legacy fallback would actually
	// return bytes — this proves the gate is the reason we don't get them.
	if err := env.innerRS.WriteBlock(ctx, legacyKey, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	// Legacy-shaped FileBlock: Hash zero, BlockStoreKey set.
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		DataSize:      uint32(len(data)),
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
		t.Fatalf("fetchBlock: expected ErrLegacyReadOnCASOnly, got nil with data=%v", got)
	}
	if !errors.Is(err, ErrLegacyReadOnCASOnly) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrLegacyReadOnCASOnly", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on cas-only legacy refusal", got)
	}
	if env.rs.readCalls.Load() != 0 {
		t.Errorf("ReadBlock calls = %d, want 0 (gate must short-circuit before remote)", env.rs.readCalls.Load())
	}
	if env.rs.readVerifiedCalls.Load() != 0 {
		t.Errorf("ReadBlockVerified calls = %d, want 0 (legacy row has no hash)", env.rs.readVerifiedCalls.Load())
	}
}

// TestDualRead_Legacy_AllowsBothPaths verifies the gate only fires on
// cas-only. With the default BlockLayoutLegacy, a legacy-shaped FileBlock
// still routes through ReadBlock as it did pre-Phase-14. (T-14-02-03
// non-regression: existing legacy shares behave identically.)
func TestDualRead_Legacy_AllowsBothPaths(t *testing.T) {
	env := newDualReadEnvWithLayout(t, metadata.BlockLayoutLegacy)
	ctx := context.Background()

	const payloadID = "share/legacy-share-legacy-row"
	data := []byte("legacy bytes — legacy share preserves the fallback")
	legacyKey := blockstore.FormatStoreKey(payloadID, 0)

	if err := env.innerRS.WriteBlock(ctx, legacyKey, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	fb := &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		DataSize:      uint32(len(data)),
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
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("fetchBlock data = %q, want %q", got, data)
	}
	if env.rs.readCalls.Load() != 1 {
		t.Errorf("ReadBlock calls = %d, want 1 (legacy share must keep fallback)", env.rs.readCalls.Load())
	}
}

// TestDualRead_CASOnly_AllowsCASPath verifies the gate is path-specific:
// a CAS-shaped FileBlock (non-zero Hash) on a cas-only share routes
// through ReadBlockVerified untouched.
func TestDualRead_CASOnly_AllowsCASPath(t *testing.T) {
	env := newDualReadEnvWithLayout(t, metadata.BlockLayoutCASOnly)
	ctx := context.Background()

	const payloadID = "share/cas-only-cas-row"
	data := []byte("CAS bytes — cas-only share serves these via verified read")
	hash := dualReadHash(data)
	casKey := blockstore.FormatCASKey(hash)

	if err := env.innerRS.WriteBlockWithHash(ctx, casKey, hash, data); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
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
		t.Fatalf("fetchBlock data = %q, want %q", got, data)
	}
	if env.rs.readVerifiedCalls.Load() != 1 {
		t.Errorf("ReadBlockVerified calls = %d, want 1", env.rs.readVerifiedCalls.Load())
	}
	if env.rs.readCalls.Load() != 0 {
		t.Errorf("ReadBlock calls = %d, want 0 (CAS path must not fall back)", env.rs.readCalls.Load())
	}
}

// TestDualRead_BlockLayoutGetterRoundTrips asserts the Syncer exposes its
// BlockLayout for Plan 05's auto-cutover path and for the wiring tests
// in Plan 14-02 task 2.
func TestDualRead_BlockLayoutGetterRoundTrips(t *testing.T) {
	envCAS := newDualReadEnvWithLayout(t, metadata.BlockLayoutCASOnly)
	if got := envCAS.syncer.BlockLayout(); got != metadata.BlockLayoutCASOnly {
		t.Errorf("Syncer.BlockLayout() = %q, want cas-only", got)
	}
	envLegacy := newDualReadEnvWithLayout(t, metadata.BlockLayoutLegacy)
	if got := envLegacy.syncer.BlockLayout(); got != metadata.BlockLayoutLegacy {
		t.Errorf("Syncer.BlockLayout() = %q, want legacy", got)
	}
	// Empty value must coerce to legacy at construction time (D-A6 forward-compat).
	envEmpty := newDualReadEnvWithLayout(t, metadata.BlockLayout(""))
	if got := envEmpty.syncer.BlockLayout(); got != metadata.BlockLayoutLegacy {
		t.Errorf("Syncer.BlockLayout() = %q, want legacy (zero-value coercion)", got)
	}
}
