package handlers

import (
	"context"
	"encoding/binary"
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
//
// The slice→array conversion `[8]byte(id[:8])` is valid Go (≥ 1.20; this repo
// is on Go 1.25); it copies the first eight bytes into a comparable array.
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
// The closed-loop counterpart — that after Handler.SeedFileIDFromDurableHandles
// (Fix A, #1376) a freshly minted FileID does NOT collide — lives in
// TestDurableHandleNoCollisionAfterReseed below.
func TestDurableHandleSurvivesSimulatedRestart(t *testing.T) {
	ctx := context.Background()

	// Persistent half == counter value 2 (little-endian in bytes 0-7, matching
	// GenerateFileID's packing). A fresh Handler stores nextFileID=1, and
	// GenerateFileID does Add(1) BEFORE packing, so the first FileID it mints
	// carries persistent half 2 — chosen here so the collision hazard below is
	// exact.
	//
	// Production storage shape: the persisted FileID has its VOLATILE half
	// (bytes 8-15) ZEROED — buildPersistedDurableHandle stores only the
	// persistent half in FileID, and keeps the full original 16-byte value in
	// OriginalFileID (see pkg/metadata/lock/durable_store.go). Mirror that here
	// so the seed key matches what the store would actually hold.
	const persistedCounter uint64 = 2
	var persistedFileID [16]byte // volatile half stays zeroed (production shape)
	binary.LittleEndian.PutUint64(persistedFileID[:8], persistedCounter)

	// originalFileID is the full 16-byte value (persistent + volatile) as seen
	// at the original CREATE — only OriginalFileID retains the volatile half.
	originalFileID := persistedFileID
	copy(originalFileID[8:], []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33})

	now := time.Now()
	store := newMockDurableStore()

	// --- Pre-restart: persist the durable handle. ---
	if err := store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:              "restart-survivor",
		FileID:          persistedFileID, // volatile half zeroed, as in production
		OriginalFileID:  originalFileID,  // full original value retained
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
// durable handles (Handler.SeedFileIDFromDurableHandles, #1376), a freshly
// minted FileID's persistent half must NOT collide with the persisted handle's
// persistent half — while the persisted handle stays reconnect-resolvable by
// FileID.
//
// This is the closed-loop counterpart to TestDurableHandleSurvivesSimulatedRestart,
// which demonstrates the un-seeded collision hazard: SeedFileIDFromDurableHandles
// is exactly what removes it.
func TestDurableHandleNoCollisionAfterReseed(t *testing.T) {
	ctx := context.Background()

	// A durable handle persisted with a deliberately HIGH persistent half
	// (counter value 5000, little-endian in bytes 0-7, matching GenerateFileID's
	// packing). A fresh Handler's counter starts at 1, so without the reseed its
	// minted FileIDs would walk 2,3,4,… and eventually collide with 5000.
	//
	// Production storage shape (pkg/metadata/lock/durable_store.go): the
	// persisted FileID keeps only the persistent half — the volatile half
	// (bytes 8-15) is zeroed, and the full original value lives in
	// OriginalFileID. Mirror that here; the reseed reads bytes 0-7 only, so the
	// zeroed volatile half does not affect the seed math.
	const persistedCounter uint64 = 5000
	var persistedFileID [16]byte // volatile half stays zeroed (production shape)
	binary.LittleEndian.PutUint64(persistedFileID[:8], persistedCounter)

	originalFileID := persistedFileID
	copy(originalFileID[8:], []byte{0xCA, 0xFE, 0xBA, 0xBE, 0x00, 0x01, 0x02, 0x03})

	now := time.Now()
	store := newMockDurableStore()
	if err := store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:              "seed-survivor",
		FileID:          persistedFileID, // volatile half zeroed, as in production
		OriginalFileID:  originalFileID,  // full original value retained
		Path:            "/reseed/file.txt",
		ShareName:       "share1",
		DisconnectedAt:  now,
		TimeoutMs:       300000,
		ServerStartTime: now,
	}); err != nil {
		t.Fatalf("PutDurableHandle: %v", err)
	}

	// --- Simulate restart + run the startup reseed (Fix A). ---
	h := NewHandler()
	h.DurableStore = store
	if got := h.nextFileID.Load(); got != 1 {
		t.Fatalf("fresh Handler nextFileID = %d, want 1 (restart counter reset)", got)
	}

	h.SeedFileIDFromDurableHandles(ctx, store)

	// The reseed stores the max persisted persistent half (5000) as the
	// last-issued value; GenerateFileID pre-increments, so the next minted
	// persistent half is 5001 — strictly above every persisted handle.
	if got := h.nextFileID.Load(); got != persistedCounter {
		t.Fatalf("after reseed nextFileID = %d, want %d (max persisted persistent half)",
			got, persistedCounter)
	}

	// --- A freshly minted FileID must NOT collide with the persisted handle. ---
	minted := h.GenerateFileID()
	if fileIDPersistentHalf(minted) == fileIDPersistentHalf(persistedFileID) {
		t.Fatalf("freshly minted FileID collides with persisted handle after reseed: "+
			"minted=%x persisted=%x",
			fileIDPersistentHalf(minted), fileIDPersistentHalf(persistedFileID))
	}
	if mintedCounter := binary.LittleEndian.Uint64(minted[:8]); mintedCounter != persistedCounter+1 {
		t.Fatalf("first post-reseed FileID persistent half = %d, want %d (max+1)",
			mintedCounter, persistedCounter+1)
	}

	// --- The persisted handle is still reconnect-resolvable by FileID. ---
	resolved, err := store.GetDurableHandleByFileID(ctx, persistedFileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID: %v", err)
	}
	if resolved == nil {
		t.Fatal("persisted durable handle not resolvable by FileID after reseed")
	}
	if resolved.ID != "seed-survivor" {
		t.Fatalf("resolved wrong handle: ID=%q, want %q", resolved.ID, "seed-survivor")
	}
}
