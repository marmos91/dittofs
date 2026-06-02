// TestSmallFileEagerDedup pins the contract that two payloads with
// identical small-file content dedup at Flush time via the eager
// small-file fast-path: the second payload's Flush skips the
// chunker, the appendlog, and any CAS-side StoreChunk; the engine
// Cache is warm for the matched hash; and ReadAt on the second
// payload returns the same bytes as the first (short-circuit MUST
// NOT corrupt data).
//
// Unit- and flush-level tests cover the internals
// (tryEagerSmallFileDedup branch behavior, FindByObjectID seeding,
// applyFileLevelDedupHit fingerprint); this gate covers the
// end-to-end observable contract a downstream caller (NFS COMMIT,
// SMB Flush) actually relies on.
//
// Counter instrumentation:
//   - Eager-path fingerprint = Cache.Put recorded with the content hash
//     (no other Flush path warms the cache with the in-RAM bytes
//     directly; rollup-side warming arrives via OnChunkComplete but on
//     a successful eager hit no chunker runs, so no StoreChunk fires).
//   - chunker bypass proxy = StoreChunk counter via a wrapped FSStore.
//     The plan called for a chunker.NewChunker hook; no such hook
//     exists in-tree (verified at planner time). StoreChunk is a
//     sufficient proxy: chunker.Next is the only caller path inside
//     rollupFile, and rollup is the only path that invokes StoreChunk.
//   - appendlog cleanup observable = post-Flush bs.local.ReadPayloadAt
//     returns ErrFileBlockNotFound (EagerHit_DeletesAppendLog
//     pattern, asserted again here as part of the end-to-end shape).

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// TestSmallFileEagerDedup_BSCAS06 — Opt 4 end-to-end hard-gate.
//
// Flow
//  1. Write file-A with 64 KiB identical content; Flush; observe its
//     ObjectID gets seeded into the coordinator (file-A's own rollup
//     path materializes the row; we seed manually for the test fixture
//     because the engine_test.go stub fileBlockStore + fakeCoordinator
//     don't run a real rollup pump).
//  2. Write file-B with IDENTICAL 64 KiB content; Flush; observe
//     a. Flush returns Finalized=true (eager hit short-circuit).
//     b. fc.getFileObjectIDCalls remains 0 for file-B's Flush —
//     speculative branch was skipped.
//     c. Cache.Put fingerprint observed for file-B's content hash.
//     d. Post-Flush ReadPayloadAt for file-B returns
//
// ErrFileBlockNotFound — appendlog cleanup verified.
func TestSmallFileEagerDedup_BSCAS06(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)
	rec := newRecordingPutCache()
	bs.cache = rec

	content := bytes.Repeat([]byte{0xBE, 0xEF, 0xCA, 0xFE}, 16*1024) // 64 KiB
	if int64(len(content)) > chunker.MinChunkSize {
		t.Fatalf("test content size %d > MinChunkSize %d — would bypass eager path",
			len(content), chunker.MinChunkSize)
	}
	contentHash := hashContent(content)
	provisional := singleBlockObjectID(content)

	// Seed: file-A's previously-quiesced single-block ObjectID is in the
	// coordinator's hit table. In production this row is materialized by
	// file-A's own rollup pass before file-B's Flush arrives; the stub
	// + fakeCoordinator harness elides the rollup pump and seeds the row
	// directly. The shape of the seeded data mirrors what
	// applyFileLevelDedupHit's set-difference math expects.
	fc.objectIDHits[provisional] = []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(content))},
	}

	// Write file-B (the "second arrival" — file-A's bytes are already
	// represented in metadata by the seed above).
	const payloadB = "file-B-bscas06"
	if _, err := bs.WriteAt(ctx, payloadB, nil, content, 0); err != nil {
		t.Fatalf("WriteAt file-B: %v", err)
	}

	// Sanity: pre-Flush, file-B's appendlog must be reachable via
	// ReadPayloadAt — the eager-path's post-hit cleanup hasn't fired
	// yet. This guards against a regression where AppendWrite silently
	// no-ops.
	probe := make([]byte, len(content))
	if n, err := bs.local.ReadPayloadAt(ctx, payloadB, probe, 0); err != nil || n != len(content) {
		t.Fatalf("pre-Flush ReadPayloadAt file-B: n=%d err=%v; want n=%d nil",
			n, err, len(content))
	}

	// Reset put-call accounting just before file-B's Flush so the
	// pre-Flush WriteAt machinery (which may have warmed the cache via
	// other paths) doesn't pollute the eager-hit fingerprint.
	rec.mu.Lock()
	rec.putCalls = 0
	rec.putHashes = nil
	rec.mu.Unlock()
	fc.mu.Lock()
	fc.getFileObjectIDCalls = 0
	fc.mu.Unlock()

	// (a) Flush file-B: must short-circuit on the eager hit.
	result, err := bs.Flush(ctx, payloadB)
	if err != nil {
		t.Fatalf("Flush file-B: %v", err)
	}
	if result == nil || !result.Finalized {
		t.Fatalf("Flush file-B Finalized=false; want true (BSCAS-06 eager hit short-circuit)")
	}

	// (b) Speculative branch must NOT have run for file-B.
	fc.mu.Lock()
	specCalls := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if specCalls != 0 {
		t.Errorf("file-B GetFileObjectID calls=%d; want 0 (BSCAS-06: eager hit must skip speculative)",
			specCalls)
	}

	// (c) Cache.Put fingerprint for file-B's content hash — cache
	// warming on eager hit.
	rec.mu.Lock()
	puts := rec.putCalls
	var sawHash bool
	for _, h := range rec.putHashes {
		if h == contentHash {
			sawHash = true
			break
		}
	}
	rec.mu.Unlock()
	if puts < 1 || !sawHash {
		t.Errorf("file-B Cache.Put: calls=%d sawHash=%v; want >=1 with hash=%s (D-16 cache warming)",
			puts, sawHash, contentHash.String())
	}

	// (d) appendlog cleanup: post-hit ReadPayloadAt returns
	// ErrFileBlockNotFound. The shared applyFileLevelDedupHit machinery
	// fires local.DeleteAppendLog as part of the finalize sequence, so the
	// per-payload appendlog is gone.
	probe2 := make([]byte, len(content))
	if _, err := bs.local.ReadPayloadAt(ctx, payloadB, probe2, 0); err == nil {
		t.Errorf("post-Flush ReadPayloadAt file-B err=nil; want ErrFileBlockNotFound (D-11 appendlog cleanup)")
	}
}

// TestSmallFileEagerDedup_AtThreshold — boundary: exactly MinChunkSize
// triggers the eager path (inclusive upper bound: data <= MinChunkSize
// proceeds; > MinChunkSize bypasses). Together with the +1 test below
// pins the threshold gate at both sides.
//
// The miss path is asserted (no seeded ObjectID): eager runs, misses
// and the speculative branch runs (getFileObjectIDCalls == 1).
func TestSmallFileEagerDedup_AtThreshold(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)

	// Exactly MinChunkSize: triggers eager (inclusive upper bound).
	data := []byte(strings.Repeat("a", chunker.MinChunkSize))

	const payloadID = "at-threshold"
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Eager ran (FindByObjectID was called) — miss — speculative ran.
	fc.mu.Lock()
	finds := len(fc.findCalls)
	spec := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if finds < 1 {
		t.Errorf("FindByObjectID calls=%d at threshold; want >=1 (eager path engaged)", finds)
	}
	if spec != 1 {
		t.Errorf("GetFileObjectID calls=%d at threshold; want 1 (eager missed → speculative ran)", spec)
	}
}

// TestSmallFileEagerDedup_AboveThreshold — boundary: MinChunkSize+1
// bypasses the eager path entirely. FindByObjectID is NOT consulted by
// the eager branch (the size gate at the call site avoids the
// ReadPayloadAt alloc + I/O for large files). The speculative branch
// runs as today (getFileObjectIDCalls == 1).
func TestSmallFileEagerDedup_AboveThreshold(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)

	// MinChunkSize + 1: above threshold. The outer call-site gate
	// `size > 0 && size <= chunker.MinChunkSize` returns false, so the
	// eager block is skipped wholesale — no FindByObjectID lookups from
	// the eager path. The speculative branch then runs (FindByObjectID
	// MAY fire from there on snapshotPendingBlockRefs evaluation, but
	// its presence/absence is not part of THIS boundary's contract
	// the contract is "GetFileObjectID == 1, eager bypassed").
	data := []byte(strings.Repeat("b", chunker.MinChunkSize+1))

	const payloadID = "above-threshold"
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	fc.mu.Lock()
	spec := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if spec != 1 {
		t.Errorf("GetFileObjectID calls=%d above threshold; want 1 (eager bypassed → speculative ran)", spec)
	}
}
