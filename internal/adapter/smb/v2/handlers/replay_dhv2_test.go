package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// encodeDH2QContext builds a 32-byte SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
// payload carrying the given CreateGuid (offset 16), matching DecodeDH2QRequest.
func encodeDH2QContext(createGuid [16]byte) []byte {
	buf := make([]byte, 32)
	copy(buf[16:32], createGuid[:])
	return buf
}

// dh2qCreateReq builds a CREATE request carrying a DH2Q context with createGuid
// and, when leaseCtx is non-nil, an RqLs request context.
func dh2qCreateReq(createGuid [16]byte, leaseCtx []byte) *CreateRequest {
	req := &CreateRequest{
		CreateContexts: []CreateContext{
			{Name: DurableHandleV2RequestTag, Data: encodeDH2QContext(createGuid)},
		},
	}
	if leaseCtx != nil {
		req.CreateContexts = append(req.CreateContexts,
			CreateContext{Name: LeaseContextTagRequest, Data: leaseCtx})
	}
	return req
}

// leaseResponseStateFromResp reads the LeaseState out of the RqLs response
// context of a CreateResponse (offset 16, uint32 LE). Returns (0, false) when
// no RqLs response context is present.
func leaseResponseStateFromResp(resp *CreateResponse) (uint32, bool) {
	for i := range resp.CreateContexts {
		if resp.CreateContexts[i].Name == LeaseContextTagResponse {
			data := resp.CreateContexts[i].Data
			if len(data) < 20 {
				return 0, false
			}
			return binary.LittleEndian.Uint32(data[16:20]), true
		}
	}
	return 0, false
}

func TestRewriteLeaseResponseState_V2(t *testing.T) {
	leaseKey := [16]byte{0x11, 0x22}
	// Original encoded V2 response at RH (0x3), epoch 5.
	orig := (&LeaseResponseContext{
		LeaseKey:   leaseKey,
		LeaseState: lock.LeaseStateRead | lock.LeaseStateHandle,
		Epoch:      5,
	}).Encode()

	out := rewriteLeaseResponseState(orig, lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 9)

	if got := binary.LittleEndian.Uint32(out[16:20]); got != 0x7 {
		t.Fatalf("rewritten state = 0x%x, want RWH (0x7)", got)
	}
	if got := binary.LittleEndian.Uint16(out[48:50]); got != 9 {
		t.Fatalf("rewritten epoch = %d, want 9", got)
	}
	// Lease key must be preserved.
	for i := 0; i < 16; i++ {
		if out[i] != orig[i] {
			t.Fatalf("lease key byte %d changed: got 0x%x want 0x%x", i, out[i], orig[i])
		}
	}
	// Source slice must not be mutated.
	if binary.LittleEndian.Uint32(orig[16:20]) != 0x3 {
		t.Fatal("rewriteLeaseResponseState mutated its input")
	}
}

func TestRewriteLeaseResponseState_V1NoEpoch(t *testing.T) {
	// 32-byte V1 layout: rewrite state but never touch bytes past the buffer.
	v1 := make([]byte, LeaseV1ContextSize)
	binary.LittleEndian.PutUint32(v1[16:20], 0x3)
	out := rewriteLeaseResponseState(v1, 0x7, 9)
	if got := binary.LittleEndian.Uint32(out[16:20]); got != 0x7 {
		t.Fatalf("V1 rewritten state = 0x%x, want 0x7", got)
	}
	if len(out) != LeaseV1ContextSize {
		t.Fatalf("V1 buffer length changed: %d", len(out))
	}
}

// newReplayTestHandler builds a Handler wired with a real LeaseManager and an
// empty CreateReplayCache, plus the backing lock Manager for direct lease
// grants.
func newReplayTestHandler() (*Handler, *lock.Manager, *lease.LeaseManager) {
	mgr := lock.NewManager()
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)
	h := &Handler{
		LeaseManager:      leaseMgr,
		CreateReplayCache: NewCreateReplayCache(),
	}
	return h, mgr, leaseMgr
}

// newReplayCtx builds an SMBHandlerContext with the replay flag set as given.
func newReplayCtx(sessionID uint64, isReplay bool) *SMBHandlerContext {
	c := NewSMBHandlerContext(context.Background(), "test", sessionID, 1, 1)
	c.IsReplay = isReplay
	return c
}

// TestResolveCreateReplay_DuplicateObjectid: a non-replay CREATE whose
// CreateGuid matches a cached open returns STATUS_DUPLICATE_OBJECTID.
func TestResolveCreateReplay_DuplicateObjectid(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	const sessionID = uint64(7)
	guid := [16]byte{0xAB, 0xCD}

	h.CreateReplayCache.Store(sessionID,
		guid,
		&CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}},
		&OpenFile{},
	)

	resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, false), dh2qCreateReq(guid, nil))
	if !handled {
		t.Fatal("non-replay duplicate CreateGuid must be handled")
	}
	if resp.Status != types.StatusDuplicateObjectid {
		t.Fatalf("status = %s, want STATUS_DUPLICATE_OBJECTID", resp.Status)
	}
}

// TestResolveCreateReplay_PendingFileNotAvailable: a replay arriving while the
// original CREATE for the CreateGuid is still parked returns
// STATUS_FILE_NOT_AVAILABLE.
func TestResolveCreateReplay_PendingFileNotAvailable(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	const sessionID = uint64(7)
	guid := [16]byte{0x01, 0x02, 0x03}

	h.CreateReplayCache.Reserve(sessionID, guid)

	resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, nil))
	if !handled {
		t.Fatal("replay against a reserved (parked) CreateGuid must be handled")
	}
	if resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("status = %s, want STATUS_FILE_NOT_AVAILABLE", resp.Status)
	}

	// Once the original completes (reservation released), the same replay
	// falls through (no cached entry yet) rather than returning FILE_NOT_AVAILABLE.
	h.CreateReplayCache.Release(sessionID, guid)
	if _, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, nil)); handled {
		t.Fatal("after release with no cached entry the replay must fall through")
	}
}

// TestResolveCreateReplay_OplockSnapshot: a plain (non-lease) oplock replay
// returns the cached snapshot verbatim — covers replay-dhv2-oplock1/3.
func TestResolveCreateReplay_OplockSnapshot(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	const sessionID = uint64(7)
	guid := [16]byte{0x09}

	cached := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     OplockLevelBatch,
		FileID:          [16]byte{0xDE, 0xAD},
	}
	h.CreateReplayCache.Store(sessionID, guid, cached,
		&OpenFile{OplockLevel: OplockLevelBatch})

	resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, nil))
	if !handled {
		t.Fatal("oplock replay must be handled")
	}
	if resp.Status != types.StatusSuccess || resp.OplockLevel != OplockLevelBatch {
		t.Fatalf("oplock replay = (%s, 0x%x), want (SUCCESS, BATCH)", resp.Status, resp.OplockLevel)
	}
	if resp.FileID != cached.FileID {
		t.Fatal("oplock replay must return the original FileID")
	}
}

// grantRWHLease seeds the LeaseManager with an RWH lease for leaseKey and
// returns the live OpenFile representing the original lease open.
func grantRWHLease(t *testing.T, leaseMgr *lease.LeaseManager, leaseKey [16]byte, sessionID uint64) *OpenFile {
	t.Helper()
	ctx := context.Background()
	fh := lock.FileHandle("file-1")
	rwh := lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
	reqCtx := encodeV2LeaseContext(leaseKey, rwh, 1)
	resp, err := ProcessLeaseCreateContext(ctx, leaseMgr, reqCtx, fh, sessionID, [16]byte{}, "smb:7", "share1", false, false)
	if err != nil {
		t.Fatalf("granting RWH lease failed: %v", err)
	}
	if resp.LeaseState != rwh {
		t.Fatalf("granted lease state 0x%x, want RWH (0x7)", resp.LeaseState)
	}
	return &OpenFile{
		SessionID:   sessionID,
		OplockLevel: OplockLevelLease,
		LeaseKey:    leaseKey,
	}
}

// TestResolveCreateReplay_LeaseReturnsCurrentState: the original CREATE
// returned RH (0x3); the lease was later upgraded to RWH (0x7). The replay must
// return the CURRENT upgraded state, not the create-time snapshot
// (replay-dhv2-lease1/2, replay.c:918 "lease_state 0x7 should not be 0x3").
func TestResolveCreateReplay_LeaseReturnsCurrentState(t *testing.T) {
	h, _, leaseMgr := newReplayTestHandler()
	const sessionID = uint64(7)
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC}

	open := grantRWHLease(t, leaseMgr, leaseKey, sessionID)

	// Cache a response whose RqLs context encodes the ORIGINAL RH (0x3).
	cached := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     OplockLevelLease,
		CreateContexts: []CreateContext{{
			Name: LeaseContextTagResponse,
			Data: (&LeaseResponseContext{
				LeaseKey:   leaseKey,
				LeaseState: lock.LeaseStateRead | lock.LeaseStateHandle, // RH 0x3
				Epoch:      1,
			}).Encode(),
		}},
	}
	h.CreateReplayCache.Store(sessionID, leaseKey16Guid(leaseKey), cached, open)

	// Replay the original RH request.
	rhReq := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateHandle, 1)
	resp, handled := h.resolveCreateReplay(
		newReplayCtx(sessionID, true),
		dh2qCreateReq(leaseKey16Guid(leaseKey), rhReq),
	)
	if !handled {
		t.Fatal("lease replay must be handled")
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("lease replay status = %s, want SUCCESS", resp.Status)
	}
	state, ok := leaseResponseStateFromResp(resp)
	if !ok {
		t.Fatal("lease replay response missing RqLs context")
	}
	rwh := lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
	if state != rwh {
		t.Fatalf("replay lease_state = 0x%x, want CURRENT RWH (0x7) not snapshot RH (0x3)", state)
	}
}

// TestResolveCreateReplay_LeaseReplayDoesNotMutateCache: a lease replay that
// refreshes the response state must not mutate the cached entry's stored
// bytes (the shallow-copy shares the CreateContexts backing array). A second
// replay must still observe the original cached snapshot as its starting point
// and re-derive the current state.
func TestResolveCreateReplay_LeaseReplayDoesNotMutateCache(t *testing.T) {
	h, _, leaseMgr := newReplayTestHandler()
	const sessionID = uint64(7)
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC}
	open := grantRWHLease(t, leaseMgr, leaseKey, sessionID)

	origData := (&LeaseResponseContext{
		LeaseKey:   leaseKey,
		LeaseState: lock.LeaseStateRead | lock.LeaseStateHandle, // RH 0x3
		Epoch:      1,
	}).Encode()
	cached := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     OplockLevelLease,
		CreateContexts:  []CreateContext{{Name: LeaseContextTagResponse, Data: origData}},
	}
	guid := leaseKey16Guid(leaseKey)
	h.CreateReplayCache.Store(sessionID, guid, cached, open)

	rhReq := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateHandle, 1)
	for i := 0; i < 2; i++ {
		resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, rhReq))
		if !handled {
			t.Fatalf("replay %d not handled", i)
		}
		state, ok := leaseResponseStateFromResp(resp)
		if !ok || state != lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle {
			t.Fatalf("replay %d state = 0x%x, want RWH (0x7)", i, state)
		}
	}

	// Cached snapshot bytes must be untouched (still RH 0x3).
	if got := binary.LittleEndian.Uint32(origData[16:20]); got != 0x3 {
		t.Fatalf("cached response bytes mutated: state = 0x%x, want RH (0x3)", got)
	}
	if cached.CreateContexts[0].Data[16] != origData[16] {
		t.Fatal("cached entry's context Data was swapped")
	}
}

// TestResolveCreateReplay_LeaseKeyMismatch: a replay carrying a different
// lease key than the open returns ACCESS_DENIED (replay-dhv2-lease3).
func TestResolveCreateReplay_LeaseKeyMismatch(t *testing.T) {
	h, _, leaseMgr := newReplayTestHandler()
	const sessionID = uint64(7)
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC}
	open := grantRWHLease(t, leaseMgr, leaseKey, sessionID)

	cached := &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}, OplockLevel: OplockLevelLease}
	h.CreateReplayCache.Store(sessionID, leaseKey16Guid(leaseKey), cached, open)

	otherKey := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	mismatchReq := encodeV2LeaseContext(otherKey, lock.LeaseStateRead|lock.LeaseStateHandle, 1)
	resp, handled := h.resolveCreateReplay(
		newReplayCtx(sessionID, true),
		dh2qCreateReq(leaseKey16Guid(leaseKey), mismatchReq),
	)
	if !handled {
		t.Fatal("lease-key-mismatch replay must be handled")
	}
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("status = %s, want STATUS_ACCESS_DENIED", resp.Status)
	}
}

// TestResolveCreateReplay_OplockReplayedAsLease: the original open is an oplock
// (not a lease); a replay carrying a lease context returns ACCESS_DENIED
// (replay-dhv2-oplock-lease).
func TestResolveCreateReplay_OplockReplayedAsLease(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	const sessionID = uint64(7)
	guid := [16]byte{0x42}

	cached := &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}, OplockLevel: OplockLevelBatch}
	h.CreateReplayCache.Store(sessionID, guid, cached, &OpenFile{OplockLevel: OplockLevelBatch})

	leaseReq := encodeV2LeaseContext([16]byte{0x55}, lock.LeaseStateRead|lock.LeaseStateHandle, 1)
	resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, leaseReq))
	if !handled {
		t.Fatal("oplock-replayed-as-lease must be handled")
	}
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("status = %s, want STATUS_ACCESS_DENIED", resp.Status)
	}
}

// TestResolveCreateReplay_NoOplockPendingReserved models the no-oplock
// share-conflict "n" variant (smbtorture replay-dhv2-pending1n-*-sane): the
// conflicting first open holds NO oplock/lease, so the second CREATE takes the
// inline (non-park) path. Create now reserves the DH2Q CreateGuid across that
// inline break+complete window. While reserved, a concurrent replay
// (FLAGS_REPLAY_OPERATION) must return STATUS_FILE_NOT_AVAILABLE rather than
// fall through the full CREATE path (the bug fixed for #749). After the original
// finishes (reservation released) and with no cached entry, a fresh replay must
// fall through cleanly — proving the reservation was not leaked.
func TestResolveCreateReplay_NoOplockPendingReserved(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	const sessionID = uint64(7)
	guid := [16]byte{0x4E, 0x4F} // "NO"

	// Create's inline-conflict path reserves the guid (no async park, no cached
	// entry yet — the original open has not completed).
	h.CreateReplayCache.Reserve(sessionID, guid)

	// Concurrent replay while reserved → FILE_NOT_AVAILABLE.
	resp, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, nil))
	if !handled {
		t.Fatal("replay against a reserved no-oplock CREATE must be handled")
	}
	if resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("status = %s, want STATUS_FILE_NOT_AVAILABLE", resp.Status)
	}

	// A non-replay duplicate while reserved must NOT be hijacked into
	// FILE_NOT_AVAILABLE: with no cached entry it simply falls through to the
	// normal CREATE path (IsReserved is gated on ctx.IsReplay).
	if _, handled := h.resolveCreateReplay(newReplayCtx(sessionID, false), dh2qCreateReq(guid, nil)); handled {
		t.Fatal("non-replay CREATE on a reserved-but-uncached guid must fall through")
	}

	// Original completes: Create releases the reservation. A fresh replay now
	// finds neither a reservation nor a cached entry and falls through — no leak.
	h.CreateReplayCache.Release(sessionID, guid)
	if h.CreateReplayCache.IsReserved(sessionID, guid) {
		t.Fatal("reservation leaked after Release")
	}
	if _, handled := h.resolveCreateReplay(newReplayCtx(sessionID, true), dh2qCreateReq(guid, nil)); handled {
		t.Fatal("after release with no cached entry the replay must fall through")
	}
}

// TestCreateReplayCache_ReserveReleaseSingleReservation proves the
// single-logical-reservation invariant the #749 fix relies on: Reserve is
// idempotent (set semantics) and one Release clears the guid regardless of how
// many Reserves preceded it. This guards the design where Create reserves the
// guid before dispatch and — on the async-park path — parkCreateOnLeaseBreak's
// resume goroutine performs the single Release. Even if both sites were to
// Reserve, a single Release must fully clear the guid (no stuck reservation
// that would wrongly FILE_NOT_AVAILABLE all future opens) and an extra Release
// must be a harmless no-op (no panic / no negative refcount).
func TestCreateReplayCache_ReserveReleaseSingleReservation(t *testing.T) {
	c := NewCreateReplayCache()
	const sessionID = uint64(11)
	guid := [16]byte{0xC0, 0xDE}

	// Idempotent Reserve: two reserves still collapse to one logical entry.
	c.Reserve(sessionID, guid)
	c.Reserve(sessionID, guid)
	if !c.IsReserved(sessionID, guid) {
		t.Fatal("guid must be reserved after Reserve")
	}

	// A single Release clears it.
	c.Release(sessionID, guid)
	if c.IsReserved(sessionID, guid) {
		t.Fatal("a single Release must fully clear the reservation (no stuck reservation)")
	}

	// An extra Release is a harmless no-op.
	c.Release(sessionID, guid)
	if c.IsReserved(sessionID, guid) {
		t.Fatal("double Release must not resurrect the reservation")
	}

	// A zero CreateGuid is never tracked (non-V2 / no DH2Q creates).
	c.Reserve(sessionID, [16]byte{})
	if c.IsReserved(sessionID, [16]byte{}) {
		t.Fatal("zero CreateGuid must never be reserved")
	}
}

// leaseKey16Guid reuses a lease key's bytes as a distinct CreateGuid for tests
// (the two are independent identifiers; reusing keeps the fixtures compact).
func leaseKey16Guid(leaseKey [16]byte) [16]byte {
	var g [16]byte
	copy(g[:], leaseKey[:])
	g[15] ^= 0xFF // perturb so guid != leaseKey, exercising independence
	return g
}
