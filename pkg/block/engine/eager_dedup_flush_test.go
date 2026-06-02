// Task 2 — engine.Flush ordering tests for eager
// small-file dedup (Opt 4).
//
// These pin engine.Flush's pre-rollup hook ordering: eager small-file
// dedup runs BEFORE trySpeculativeFileLevelDedup. On hit the
// eager path returns &block.FlushResult{Finalized: true}, nil and
// the speculative branch (which calls coordinator.GetFileObjectID) is
// skipped entirely.
//
// Distinguishing signal: fakeCoordinator.getFileObjectIDCalls is
// bumped ONLY by the speculative branch (engine.go ~line 691). Eager
// short-circuit ⇒ 0; speculative branch reached ⇒ 1.
//
// Cache.Put fingerprint: the eager path's HIT calls bs.cache.Put with
// the in-RAM bytes; the speculative path does NOT (it only calls
// InvalidateFile). A Put recorded by recordingPutCache after Flush is a
// positive eager-fired marker.

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// flushTestSetup wires a Store + fakeCoordinator + recordingPutCache.
// Returns all three so tests can seed objectIDHits, write bytes via
// WriteAt, drive Flush, and assert the eager/speculative call signals.
func flushTestSetup(t *testing.T) (*Store, *fakeCoordinator, *recordingPutCache) {
	t.Helper()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)
	rec := newRecordingPutCache()
	bs.cache = rec
	return bs, fc, rec
}

// TestEngine_Flush_SmallFile_Hit_ShortCircuits — eager hit returns
// Finalized=true and skips the speculative branch entirely
// (getFileObjectIDCalls == 0 after Flush; Cache.Put fingerprint observed).
func TestEngine_Flush_SmallFile_Hit_ShortCircuits(t *testing.T) {
	ctx := context.Background()
	bs, fc, rec := flushTestSetup(t)

	payloadID := "small-hit"
	data := []byte("small file fits in one chunk")
	contentHash := hashContent(data)
	provisional := singleBlockObjectID(data)

	// Seed: a previously-quiesced file dedups to this ObjectID.
	fc.objectIDHits[provisional] = []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(data))},
	}

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	result, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if result == nil || !result.Finalized {
		t.Fatalf("Flush result Finalized=false; want true (eager hit short-circuit)")
	}

	// Speculative branch's GetFileObjectID must NOT have fired (eager
	// ran first and short-circuited the entire Flush).
	fc.mu.Lock()
	got := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if got != 0 {
		t.Errorf("GetFileObjectID calls=%d; want 0 (eager hit must skip speculative branch)", got)
	}

	// Cache.Put fingerprint: only the eager path's HIT calls cache.Put
	// with the in-RAM bytes.
	rec.mu.Lock()
	putCalls := rec.putCalls
	var seenHash bool
	for _, h := range rec.putHashes {
		if h == contentHash {
			seenHash = true
			break
		}
	}
	rec.mu.Unlock()
	if putCalls < 1 || !seenHash {
		t.Errorf("Cache.Put: calls=%d seenHash=%v; want Put with content hash %s (D-16 eager-hit fingerprint)",
			putCalls, seenHash, contentHash.String())
	}
}

// TestEngine_Flush_SmallFile_Miss_FallsThroughToRollup — small-file
// content with no seeded ObjectID: eager runs and misses, the
// speculative branch then runs (getFileObjectIDCalls == 1), Flush
// completes via the syncer.Flush rollup path (Finalized=false).
func TestEngine_Flush_SmallFile_Miss_FallsThroughToRollup(t *testing.T) {
	ctx := context.Background()
	bs, fc, _ := flushTestSetup(t)

	payloadID := "small-miss"
	data := []byte("unique content that no one has seen")

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	result, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// On miss the eager path returns false → engine.Flush proceeds to
	// the speculative branch, which (with no seeded hit) also misses
	// and finally delegates to bs.syncer.Flush — the rollup path.
	// FlushResult is allowed to be non-nil or nil depending on the
	// syncer's local-only-mode return; what matters is the speculative
	// branch ran.
	_ = result

	fc.mu.Lock()
	got := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if got != 1 {
		t.Errorf("GetFileObjectID calls=%d; want 1 (eager missed → speculative branch ran)", got)
	}

	// Both eager + speculative branches consult FindByObjectID. Eager
	// hashes the in-RAM data; speculative passes the chunked-projection
	// ObjectID. For a single-chunk small file these coincide, but the
	// branches are independent — at least one lookup must have fired.
	fc.mu.Lock()
	finds := len(fc.findCalls)
	fc.mu.Unlock()
	if finds < 1 {
		t.Errorf("FindByObjectID calls=%d on miss; want at least 1", finds)
	}
}

// TestEngine_Flush_LargeFile_Bypasses_EagerPath — content >
// chunker.MinChunkSize: eager threshold gate returns false without
// invoking FindByObjectID; the speculative branch runs as today
// (getFileObjectIDCalls == 1).
//
// Eager's only side effect on this path is the early-return; no Cache.Put
// fires (eager only warms cache on HIT — see + the cache-warming
// fingerprint covered by Hit_ShortCircuits).
func TestEngine_Flush_LargeFile_Bypasses_EagerPath(t *testing.T) {
	ctx := context.Background()
	bs, fc, _ := flushTestSetup(t)

	payloadID := "large-bypass"
	// MinChunkSize + 1 byte: above threshold ⇒ eager bypasses
	// FindByObjectID entirely.
	data := []byte(strings.Repeat("y", chunker.MinChunkSize+1))

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	fc.mu.Lock()
	got := fc.getFileObjectIDCalls
	fc.mu.Unlock()
	if got != 1 {
		t.Errorf("GetFileObjectID calls=%d; want 1 (large file: eager bypassed → speculative ran)", got)
	}
}

// TestEngine_Flush_EagerHit_DeletesAppendLog — mirror: on hit the
// shared applyFileLevelDedupHit machinery calls local.DeleteAppendLog so any
// in-flight appendlog state is cleaned up. We assert via the memory
// backend's ReadPayloadAt: post-Flush, reads at offset 0 return
// ErrFileBlockNotFound because the per-payload appendlog was dropped.
func TestEngine_Flush_EagerHit_DeletesAppendLog(t *testing.T) {
	ctx := context.Background()
	bs, fc, _ := flushTestSetup(t)

	payloadID := "appendlog-cleanup"
	data := []byte("hash me and dedup")
	contentHash := hashContent(data)
	provisional := singleBlockObjectID(data)
	fc.objectIDHits[provisional] = []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(data))},
	}

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Pre-Flush: appendlog reachable via ReadPayloadAt.
	probe := make([]byte, len(data))
	if n, err := bs.local.ReadPayloadAt(ctx, payloadID, probe, 0); err != nil || n != len(data) {
		t.Fatalf("pre-Flush ReadPayloadAt: n=%d err=%v; want n=%d nil", n, err, len(data))
	}

	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Post-Flush hit: DeleteAppendLog cleaned up the appendlog. The
	// memory backend's ReadPayloadAt returns ErrFileBlockNotFound once
	// the appendlog has been dropped.
	probe2 := make([]byte, len(data))
	if _, err := bs.local.ReadPayloadAt(ctx, payloadID, probe2, 0); err == nil {
		t.Errorf("post-hit ReadPayloadAt err=nil; want ErrFileBlockNotFound (D-11 appendlog cleanup)")
	}
}

// TestEngine_Flush_EagerHit_BeforeSpeculative_Ordering — explicit
// ordering pin: on a small-file hit, the eager branch fires first
// (Cache.Put recorded) and the speculative branch is NOT reached
// (getFileObjectIDCalls remains 0).
//
// On a small-file MISS in the eager branch, Flush continues into the
// speculative branch (getFileObjectIDCalls == 1). This sub-test is
// covered by TestEngine_Flush_SmallFile_Miss_FallsThroughToRollup; the
// HIT half is asserted here for explicit ordering.
func TestEngine_Flush_EagerHit_BeforeSpeculative_Ordering(t *testing.T) {
	ctx := context.Background()
	bs, fc, rec := flushTestSetup(t)

	payloadID := "ordering"
	data := []byte("eager runs first on small files")
	contentHash := hashContent(data)
	provisional := singleBlockObjectID(data)
	fc.objectIDHits[provisional] = []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(data))},
	}

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	result, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if result == nil || !result.Finalized {
		t.Fatalf("Flush Finalized=false; want true (eager hit must short-circuit)")
	}

	fc.mu.Lock()
	specCalls := fc.getFileObjectIDCalls
	finds := len(fc.findCalls)
	fc.mu.Unlock()
	if specCalls != 0 {
		t.Errorf("Speculative branch ran (getFileObjectIDCalls=%d); ordering invariant violated — eager must fire first AND short-circuit", specCalls)
	}
	// Eager makes exactly one FindByObjectID lookup; speculative would
	// make a second — so the count must be 1.
	if finds != 1 {
		t.Errorf("FindByObjectID calls=%d; want 1 (eager only, no speculative follow-up)", finds)
	}

	// Cache.Put fingerprint confirms the eager branch fired.
	rec.mu.Lock()
	puts := rec.putCalls
	rec.mu.Unlock()
	if puts < 1 {
		t.Errorf("Cache.Put calls=%d; want >= 1 (eager-hit D-16 fingerprint)", puts)
	}
}
