package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// fileIDPersistentHalf returns the persistent half (bytes 0-7) of a 16-byte
// SMB2 FileID. The durable-handle store keys V1 handles on this half only
// (see durable_context.go zeroVolatileHalf / buildPersistedDurableHandle),
// and GenerateFileID packs the monotonic nextFileID counter into exactly these
// eight bytes — so a counter that resets on restart can re-mint a persistent
// half that already belongs to a disconnected durable handle.
func fileIDPersistentHalf(id [16]byte) [8]byte {
	return [8]byte(id[:8])
}

// TestDurableHandleSurvivesSimulatedRestart asserts the reconnect-resolution
// invariant for durable handles across a simulated server restart, and proves
// the FileID-counter collision hazard that motivates the startup reseed
// (Fix A / #1376).
//
// Sequence:
//  1. A durable handle is persisted with a known FileID. Its persistent half is
//     deliberately set to the value GenerateFileID mints first on a freshly
//     constructed Handler — so a naive restart re-mints a colliding persistent
//     half.
//  2. Restart is simulated by constructing a brand-new Handler (nextFileID
//     resets to 1) wired to the SAME durable store. The persisted record
//     survives the restart because it lives in the store, not in Handler state.
//  3. The persisted handle is still reconnect-resolvable by its FileID via the
//     store's persistent-half-keyed lookup (GetDurableHandleByFileID), which is
//     exactly what processV2Reconnect / processV1Reconnect call.
//  4. The collision hazard is demonstrated: the first FileID a fresh Handler
//     mints reuses the persisted handle's persistent half. This is the bug a
//     startup reseed of nextFileID (from the max persisted FileID) prevents.
//
// NOTE: the post-reseed assertion — that a freshly minted FileID does NOT
// collide with any persisted handle's persistent half — depends on Fix A's
// Handler.SeedFileIDFromDurableHandles, which is NOT yet on origin/develop
// (tracked by PR #1376). That assertion lives in the guarded sub-test below and
// is skipped until #1376 lands. We intentionally do NOT reimplement the seed
// here.
func TestDurableHandleSurvivesSimulatedRestart(t *testing.T) {
	ctx := context.Background()

	// FileID with persistent half == counter value 2 (little-endian in bytes
	// 0-7, matching GenerateFileID's packing), volatile half arbitrary.
	// A fresh Handler stores nextFileID=1, and GenerateFileID does Add(1)
	// BEFORE packing, so the first FileID it mints carries persistent half 2 —
	// the value chosen here so the collision hazard below is exact.
	persistedFileID := [16]byte{
		0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // persistent half = 2
		0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33, // volatile half
	}

	now := time.Now()
	store := newMockDurableStore()

	// --- Pre-restart: persist the durable handle. ---
	if err := store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:              "restart-survivor",
		FileID:          persistedFileID,
		OriginalFileID:  persistedFileID,
		Path:            "/restart/file.txt",
		ShareName:       "share1",
		DisconnectedAt:  now,
		TimeoutMs:       300000, // 5 min — well within timeout at "restart"
		ServerStartTime: now,
	}); err != nil {
		t.Fatalf("PutDurableHandle: %v", err)
	}

	// --- Simulate restart: brand-new Handler, counter resets to 1. ---
	h := NewHandler()
	h.DurableStore = store

	if got := h.nextFileID.Load(); got != 1 {
		t.Fatalf("fresh Handler nextFileID = %d, want 1 (restart counter reset)", got)
	}

	// --- The persisted handle survives and is reconnect-resolvable by FileID. ---
	// This is the lookup processV1Reconnect / processV2Reconnect perform for a
	// V1-via-DH2C (zero CreateGuid) reconnect — keyed on the persistent half.
	resolved, err := store.GetDurableHandleByFileID(ctx, persistedFileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID: %v", err)
	}
	if resolved == nil {
		t.Fatal("persisted durable handle not resolvable by FileID after restart")
	}
	if resolved.ID != "restart-survivor" {
		t.Fatalf("resolved wrong handle: ID=%q, want %q", resolved.ID, "restart-survivor")
	}
	if fileIDPersistentHalf(resolved.FileID) != fileIDPersistentHalf(persistedFileID) {
		t.Fatalf("resolved FileID persistent half %x != persisted %x",
			fileIDPersistentHalf(resolved.FileID), fileIDPersistentHalf(persistedFileID))
	}

	// --- Collision hazard (the bug Fix A / #1376 fixes). ---
	// Without a startup reseed, the FIRST FileID a fresh Handler mints reuses
	// the persisted handle's persistent half (both derive from counter value 2:
	// nextFileID resets to 1, GenerateFileID does Add(1) first).
	// A reconnect of the disconnected durable handle would then alias a live,
	// freshly opened file. This assertion documents the hazard so a regression
	// in GenerateFileID's packing (or the store's persistent-half keying) is
	// caught here.
	minted := h.GenerateFileID()
	if fileIDPersistentHalf(minted) != fileIDPersistentHalf(persistedFileID) {
		t.Fatalf("expected fresh-Handler first FileID to collide with persisted "+
			"persistent half (the #1376 hazard); minted=%x persisted=%x",
			fileIDPersistentHalf(minted), fileIDPersistentHalf(persistedFileID))
	}
}

// TestDurableHandleNoCollisionAfterReseed is the post-Fix-A assertion: after a
// fresh Handler runs the startup reseed of nextFileID from the persisted
// durable handles, a freshly minted FileID's persistent half must NOT collide
// with any persisted handle's persistent half — while the persisted handle
// stays reconnect-resolvable.
//
// This depends on Handler.SeedFileIDFromDurableHandles (Fix A, PR #1376), which
// is NOT on origin/develop at the time of writing. Enable this test once #1376
// merges by removing the skip and the build-tag guard below.
func TestDurableHandleNoCollisionAfterReseed(t *testing.T) {
	t.Skip("blocked on Fix A / PR #1376: Handler.SeedFileIDFromDurableHandles " +
		"not yet on origin/develop — see durable_restart_survival_test.go header")

	// Intended assertions once #1376 lands (kept as documentation; the symbol
	// SeedFileIDFromDurableHandles does not exist yet so this body must stay
	// behind the t.Skip above and uncompiled references avoided):
	//
	//   ctx := context.Background()
	//   store := newMockDurableStore()
	//   _ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
	//       ID: "seed-survivor", FileID: persistedFileID, ...})
	//
	//   h := NewHandler()
	//   h.DurableStore = store
	//   if err := h.SeedFileIDFromDurableHandles(ctx); err != nil { t.Fatal(err) }
	//
	//   minted := h.GenerateFileID()
	//   if fileIDPersistentHalf(minted) == fileIDPersistentHalf(persistedFileID) {
	//       t.Fatal("freshly minted FileID collides with persisted handle after reseed")
	//   }
	//   resolved, _ := store.GetDurableHandleByFileID(ctx, persistedFileID)
	//   if resolved == nil { t.Fatal("persisted handle not resolvable after reseed") }
}
