package engine

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	memorylocal "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"lukechampine.com/blake3"
)

// errBoomPut is the sentinel error returned by failingPutRemote.
var errBoomPut = errors.New("boom put")

// failingPutRemote wraps a RemoteStore and surfaces errBoomPut on every
// Put call. Used by TestMirrorLoop_PropagatesPutError to exercise the
// "remote put <hash>: %w" error path in mirrorOnce.
type failingPutRemote struct {
	remote.RemoteStore
}

func (f *failingPutRemote) Put(_ context.Context, _ blockstore.ContentHash, _ []byte) error {
	return errBoomPut
}

// noopSyncedHashStore satisfies metadata.SyncedHashStore with always-
// unsynced answers and idempotent Mark/Delete. mirrorOnce only reaches
// MarkSynced when remote.Put returns nil — the failing-put path bails
// out before that, so MarkSynced never fires in this test.
type noopSyncedHashStore struct{}

func (noopSyncedHashStore) IsSynced(_ context.Context, _ blockstore.ContentHash) (bool, error) {
	return false, nil
}
func (noopSyncedHashStore) MarkSynced(_ context.Context, _ blockstore.ContentHash) error {
	return nil
}
func (noopSyncedHashStore) DeleteSynced(_ context.Context, _ blockstore.ContentHash) error {
	return nil
}

// oneHashLocalStore wraps a MemoryStore and overrides ListUnsynced to
// yield exactly one synthetic hash so the test does not depend on the
// memory backend's append-log-driven rollup producing chunks
// deterministically. The Put-followed-by-override sequence ensures Get
// resolves the hash through the embedded MemoryStore CAS map.
type oneHashLocalStore struct {
	*memorylocal.MemoryStore
	hash blockstore.ContentHash
}

func newOneHashLocalStore(t *testing.T, hash blockstore.ContentHash, data []byte) *oneHashLocalStore {
	t.Helper()
	ms := memorylocal.New()
	if err := ms.Put(context.Background(), hash, data); err != nil {
		t.Fatalf("seed local Put: %v", err)
	}
	return &oneHashLocalStore{MemoryStore: ms, hash: hash}
}

func (o *oneHashLocalStore) ListUnsynced(_ context.Context) iter.Seq2[blockstore.ContentHash, error] {
	return func(yield func(blockstore.ContentHash, error) bool) {
		_ = yield(o.hash, nil)
	}
}

// TestMirrorLoop_PropagatesPutError asserts that Syncer.mirrorOnce
// surfaces a Put failure verbatim, wrapped as
// "remote put <hash>: %w". This pins the crash-safety contract: a
// failed Put leaves the hash unmarked-synced so the next pass retries
// it; a swallowed Put would leave the hash silently missing from the
// remote.
func TestMirrorLoop_PropagatesPutError(t *testing.T) {
	ctx := context.Background()
	payload := []byte("mirror-loop-put-error")
	// Use the real BLAKE3 hash of the payload so the pre-upload verify
	// step in mirrorOnce passes and the test exercises the Put failure
	// path (not the corruption-refusal path).
	hash := blockstore.ContentHash(blake3.Sum256(payload))

	local := newOneHashLocalStore(t, hash, payload)
	rs := &failingPutRemote{}
	m := &Syncer{
		local:           local,
		remoteStore:     rs,
		syncedHashStore: noopSyncedHashStore{},
	}

	err := m.mirrorOnce(ctx)
	if err == nil {
		t.Fatalf("mirrorOnce returned nil, want error wrapping %v", errBoomPut)
	}
	if !errors.Is(err, errBoomPut) {
		t.Fatalf("mirrorOnce error = %v, want wrapping %v", err, errBoomPut)
	}
}
