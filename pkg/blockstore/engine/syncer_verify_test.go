package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	memorylocal "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"lukechampine.com/blake3"
)

// countingPutRemote wraps a RemoteStore and counts Put calls so the
// corruption test can assert that no upload happened when the local
// bytes' BLAKE3 hash does not match the claimed CAS hash.
type countingPutRemote struct {
	remote.RemoteStore
	puts atomic.Int32
}

func (r *countingPutRemote) Put(_ context.Context, _ blockstore.ContentHash, _ []byte) error {
	r.puts.Add(1)
	return nil
}

// TestMirrorOnce_DetectsLocalCorruption_RefusesUpload verifies that
// mirrorOnce re-hashes the local bytes before uploading and refuses
// to upload (and refuses to MarkSynced) when the computed BLAKE3 hash
// does not match the claimed CAS hash. This protects against local
// bitrot, torn writes, or hardware errors silently propagating corrupt
// bytes to the remote.
func TestMirrorOnce_DetectsLocalCorruption_RefusesUpload(t *testing.T) {
	ctx := context.Background()

	// Claimed hash deliberately constructed to NOT match the payload.
	claimedHash := blockstore.ContentHash{0xDE, 0xAD, 0xBE, 0xEF}
	corruptPayload := []byte("corrupt-bytes-that-do-not-hash-to-claimed-hash")

	// Sanity-guard the fixture: if these somehow collided, the test
	// would be a false-negative.
	if blockstore.ContentHash(blake3.Sum256(corruptPayload)) == claimedHash {
		t.Fatalf("test fixture invalid: corrupt payload coincidentally hashes to claimed hash")
	}

	local := newOneHashLocalStore(t, claimedHash, corruptPayload)
	rs := &countingPutRemote{}
	synced := newRecordingSyncedHashStore()

	m := &Syncer{
		local:           local,
		remoteStore:     rs,
		syncedHashStore: synced,
	}

	err := m.mirrorOnce(ctx)
	if err == nil {
		t.Fatalf("mirrorOnce returned nil; want error reporting local corruption")
	}
	if !strings.Contains(err.Error(), "local corruption") {
		t.Errorf("mirrorOnce error = %q; want substring %q", err.Error(), "local corruption")
	}
	if got := rs.puts.Load(); got != 0 {
		t.Errorf("remote.Put called %d times; want 0 (upload must be refused on hash mismatch)", got)
	}
	if ok, _ := synced.IsSynced(ctx, claimedHash); ok {
		t.Errorf("MarkSynced fired for corrupt hash; want no mark on hash mismatch")
	}
}

// TestMirrorOnce_GoodHash_Uploads verifies the happy path: when the
// local bytes' BLAKE3 hash matches the claimed CAS hash, mirrorOnce
// uploads via remote.Put and MarkSynced fires through the synced-hash
// store. Confirms the new verify step does not break the legitimate
// code path.
func TestMirrorOnce_GoodHash_Uploads(t *testing.T) {
	ctx := context.Background()

	payload := []byte("legitimate-mirror-loop-payload-bytes-for-happy-path")
	hash := blockstore.ContentHash(blake3.Sum256(payload))

	ms := memorylocal.New()
	if err := ms.Put(ctx, hash, payload); err != nil {
		t.Fatalf("seed local Put: %v", err)
	}
	local := &oneHashLocalStore{MemoryStore: ms, hash: hash}

	rs := &countingPutRemote{}
	synced := newRecordingSyncedHashStore()

	m := &Syncer{
		local:           local,
		remoteStore:     rs,
		syncedHashStore: synced,
	}

	if err := m.mirrorOnce(ctx); err != nil {
		t.Fatalf("mirrorOnce: %v", err)
	}
	if got := rs.puts.Load(); got != 1 {
		t.Errorf("remote.Put called %d times; want 1", got)
	}
	if ok, _ := synced.IsSynced(ctx, hash); !ok {
		t.Errorf("MarkSynced did not fire for good hash; want hash marked synced after upload")
	}
}
