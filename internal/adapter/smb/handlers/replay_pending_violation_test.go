package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// These tests pin the replay-decision state machine for the #749
// "pending create vs share/lease violation" smbtorture rows, modelled at the
// resolveCreateReplay level (the live park→break→resume lifecycle is what
// smbtorture itself drives; here we assert the cache-state transitions that
// lifecycle produces, which is what determines the wire status of a replay).
//
// The smbtorture sequence (source4/torture/smb2/replay.c,
// _test_dhv2_pending1_vs_violation) is:
//
//  1. holder opens the file with an RWH lease, share_access=NONE.
//  2. opener sends a DH2Q CREATE (here the "n" variant: no oplock/lease) that
//     conflicts on the share mode → the CREATE parks pending a lease break.
//     While parked, Create has Reserved the opener's CreateGuid.
//  3. a REPLAY of that CREATE arrives while still parked
//     → must return STATUS_FILE_NOT_AVAILABLE (the "sane"/Samba policy).
//  4. the holder releases its lease, either by:
//       - CLOSE  : the holder's open goes away; the parked CREATE then
//                  succeeds (STATUS_OK), caches its open, and releases the
//                  reservation.
//       - ACK    : the holder downgrades RWH→RH but keeps its open; the
//                  parked CREATE still loses the share-mode contest and
//                  resolves to STATUS_SHARING_VIOLATION, caching nothing and
//                  releasing the reservation.
//  5. a SECOND REPLAY arrives after resolution and must observe the SAME
//     terminal status as the original parked CREATE:
//       - close → STATUS_OK  (cached open replayed, same FileId)
//       - ack   → STATUS_SHARING_VIOLATION (no cache; falls through and the
//                 live share-mode contest is re-evaluated to the same loss)
//
// The "-windows" policy variants diverge (Windows ignores the replay flag in
// this corner and returns SHARING_VIOLATION for every step); they are tracked
// separately and verified by CI smbtorture, not asserted here.

const violationSessionID = uint64(7)

// TestReplayPendingViolation_LeaseClose_Sane drives the close corner:
// pending replay → FILE_NOT_AVAILABLE, then post-resolution replay → OK with
// the original FileId (smbtorture dhv2-pending1n-vs-violation-lease-close-sane).
func TestReplayPendingViolation_LeaseClose_Sane(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	guid := [16]byte{0xC1}
	req := dh2qCreateReq(guid, nil)

	// Step 2: opener's CREATE parks; Create reserves the CreateGuid.
	h.CreateReplayCache.Reserve(violationSessionID, guid)

	// Step 3: replay while parked → FILE_NOT_AVAILABLE.
	resp, handled := h.resolveCreateReplay(newReplayCtx(violationSessionID, true), req)
	if !handled || resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("replay-while-parked = (%v, %s), want (true, FILE_NOT_AVAILABLE)", handled, resp.GetStatus())
	}

	// Step 4 (close): the parked CREATE resolves to OK, caches its open, and
	// releases the reservation. This is exactly what the parked resume
	// goroutine does: completeCreateAfterBreak Store on success, then the
	// deferred Release.
	originalFileID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	okResp := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		FileID:          originalFileID,
	}
	h.CreateReplayCache.Store(violationSessionID, guid, okResp,
		&OpenFile{SessionID: violationSessionID})
	h.CreateReplayCache.Release(violationSessionID, guid)

	// Step 5: second replay → cached OK, same FileId. Never FILE_NOT_AVAILABLE
	// (the reservation is gone) and never a fresh re-execution (the open is
	// returned verbatim).
	resp2, handled2 := h.resolveCreateReplay(newReplayCtx(violationSessionID, true), req)
	if !handled2 {
		t.Fatal("post-close replay must be handled by the cache")
	}
	if resp2.Status != types.StatusSuccess {
		t.Fatalf("post-close replay status = %s, want STATUS_OK", resp2.GetStatus())
	}
	if resp2.FileID != originalFileID {
		t.Fatalf("post-close replay FileId = %x, want original %x", resp2.FileID, originalFileID)
	}
}

// TestReplayPendingViolation_LeaseAck_Sane drives the ack corner: pending
// replay → FILE_NOT_AVAILABLE, then post-resolution replay must NOT be served
// from the cache (nothing was cached for the failed open) and must fall
// through so the live share-mode contest re-produces SHARING_VIOLATION
// (smbtorture dhv2-pending1n-vs-violation-lease-ack-sane).
func TestReplayPendingViolation_LeaseAck_Sane(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	guid := [16]byte{0xAC}
	req := dh2qCreateReq(guid, nil)

	// Step 2: opener's CREATE parks; Create reserves the CreateGuid.
	h.CreateReplayCache.Reserve(violationSessionID, guid)

	// Step 3: replay while parked → FILE_NOT_AVAILABLE.
	resp, handled := h.resolveCreateReplay(newReplayCtx(violationSessionID, true), req)
	if !handled || resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("replay-while-parked = (%v, %s), want (true, FILE_NOT_AVAILABLE)", handled, resp.GetStatus())
	}

	// Step 4 (ack): the holder downgrades RWH→RH but keeps its open, so the
	// parked CREATE loses the share-mode contest and resolves to
	// SHARING_VIOLATION. completeCreateAfterBreak only stores the replay cache
	// entry on success (openFile != nil), so a SHARING_VIOLATION parked
	// resolution caches NOTHING; the deferred Release still clears the guid.
	// We model exactly that: no Store, just Release.
	h.CreateReplayCache.Release(violationSessionID, guid)

	if h.CreateReplayCache.LookupEntry(violationSessionID, guid) != nil {
		t.Fatal("a SHARING_VIOLATION parked resolution must not leave a cache entry")
	}

	// Step 5: second replay finds neither a reservation nor a cached entry, so
	// it falls through (handled=false) to the normal CREATE path, which
	// re-evaluates the still-conflicting share mode and returns
	// SHARING_VIOLATION on the wire. Critically it must NOT return
	// FILE_NOT_AVAILABLE (reservation released) and must NOT be silently served
	// as a success from a stale cache entry.
	if resp2, handled2 := h.resolveCreateReplay(newReplayCtx(violationSessionID, true), req); handled2 {
		t.Fatalf("post-ack replay must fall through to re-execute, got handled with %s", resp2.GetStatus())
	}
}

// TestReplayPendingViolation_NonReplayDuringPark proves a NON-replay CREATE
// that races the parked window is never hijacked into FILE_NOT_AVAILABLE: the
// reservation gate is keyed on ctx.IsReplay. With no cached entry it falls
// through; only once an entry is cached does a non-replay duplicate become
// DUPLICATE_OBJECTID. This separates the "n"-variant pending gate from the
// duplicate-objectid gate so neither corner regresses the other.
func TestReplayPendingViolation_NonReplayDuringPark(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	guid := [16]byte{0x4E, 0x52}
	req := dh2qCreateReq(guid, nil)

	h.CreateReplayCache.Reserve(violationSessionID, guid)

	// Non-replay CREATE while reserved-but-uncached → falls through.
	if resp, handled := h.resolveCreateReplay(newReplayCtx(violationSessionID, false), req); handled {
		t.Fatalf("non-replay CREATE during park must fall through, got %s", resp.GetStatus())
	}

	// Once the parked CREATE succeeds and caches its open, a non-replay
	// duplicate of the same CreateGuid is a protocol violation →
	// DUPLICATE_OBJECTID, regardless of the earlier reservation.
	h.CreateReplayCache.Store(violationSessionID, guid,
		&CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}},
		&OpenFile{SessionID: violationSessionID})
	h.CreateReplayCache.Release(violationSessionID, guid)

	resp, handled := h.resolveCreateReplay(newReplayCtx(violationSessionID, false), req)
	if !handled {
		t.Fatal("non-replay duplicate of a cached CreateGuid must be handled")
	}
	if resp.Status != types.StatusDuplicateObjectid {
		t.Fatalf("non-replay duplicate status = %s, want DUPLICATE_OBJECTID", resp.GetStatus())
	}
}

// TestReplayPendingViolation_CrossSessionIsolation proves the pending gate is
// per-session: a replay carrying a CreateGuid reserved by a DIFFERENT session
// must not see that reservation (it would otherwise wrongly stall an unrelated
// session's create with FILE_NOT_AVAILABLE). The reserveKey is {SessionID,
// CreateGuid}, so a foreign session falls through.
func TestReplayPendingViolation_CrossSessionIsolation(t *testing.T) {
	h, _, _ := newReplayTestHandler()
	guid := [16]byte{0x15, 0x0}
	req := dh2qCreateReq(guid, nil)

	h.CreateReplayCache.Reserve(violationSessionID, guid)

	// Same session, replay → FILE_NOT_AVAILABLE.
	if resp, handled := h.resolveCreateReplay(newReplayCtx(violationSessionID, true), req); !handled || resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("same-session parked replay = (%v, %s), want (true, FILE_NOT_AVAILABLE)", handled, resp.GetStatus())
	}

	// Different session, same guid, replay → must fall through (no foreign
	// reservation visibility, no cached entry).
	const otherSession = uint64(99)
	if resp, handled := h.resolveCreateReplay(newReplayCtx(otherSession, true), req); handled {
		t.Fatalf("foreign-session replay must not see another session's reservation, got %s", resp.GetStatus())
	}
}

// TestParkedCreate_ReplayReservationClearedBeforeFinalResponse pins the
// ORDERING the state-machine tests above cannot see: the CreateGuid
// reservation must already be gone at the instant the parked CREATE's
// terminal status is handed to the wire.
//
// The park resolves, the server writes STATUS_SHARING_VIOLATION for the
// original CREATE, and the client — which learns the CREATE is finished from
// exactly that response — puts its replay on the connection straight away. If
// the reservation outlives the send by even a scheduling quantum, the server
// dispatches that replay against a reservation for a CREATE that has already
// resolved and answers STATUS_FILE_NOT_AVAILABLE where the terminal
// STATUS_SHARING_VIOLATION is required (smbtorture
// smb2.replay.dhv2-pending1n-vs-violation-lease-ack-sane, replay.c io23).
//
// The wait resolves immediately here (no lock manager → the share-conflict
// wait returns at once, as it does when the holder ACKs its break and keeps
// its open), so the whole test is deterministic: it asserts what the callback
// observes, not how fast anything runs.
func TestParkedCreate_ReplayReservationClearedBeforeFinalResponse(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)
	// A nil lock manager makes WaitForShareConflictClear return immediately,
	// standing in for the holder's break having drained with its open intact.
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{}, nil)

	const fileName = "parked.dat"
	existingFile, _, err := rt.GetMetadataService().CreateFile(rootAuth, rootHandle, fileName,
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o777})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	metaHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// The holder: reading the file while denying every share mode, so the
	// parked CREATE below still loses the share-mode contest on recheck.
	h.StoreOpenFile((&OpenFile{
		FileID:         [16]byte{0xAA},
		SessionID:      violationSessionID + 1,
		ShareName:      smbCtx.ShareName,
		DesiredAccess:  0x00000001, // FILE_READ_DATA
		ShareAccess:    0,          // deny read/write/delete
		MetadataHandle: metaHandle,
	}).WithName(OpenName{Path: fileName, FileName: fileName}))

	guid := [16]byte{0x21, 0x9}
	req := dh2qCreateReq(guid, nil)
	req.FileName = fileName
	req.DesiredAccess = 0x00000001
	req.ShareAccess = 0
	req.CreateDisposition = types.FileOpen

	draft := &createDraft{
		req:            req,
		tree:           tree,
		authCtx:        rootAuth,
		filename:       fileName,
		baseName:       fileName,
		parentHandle:   rootHandle,
		existingFile:   existingFile,
		existingHandle: metaHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}

	// Create() reserves the guid before dispatching the break; the parked
	// resume goroutine owns the matching release.
	h.CreateReplayCache.Reserve(smbCtx.SessionID, guid)

	type finalResponse struct {
		status         types.Status
		reservedAtSend bool
	}
	sent := make(chan finalResponse, 1)

	parkCtx := &SMBHandlerContext{
		Context:         context.Background(),
		SessionID:       smbCtx.SessionID,
		TreeID:          smbCtx.TreeID,
		ShareName:       smbCtx.ShareName,
		MessageID:       5,
		TryReserveAsync: func() bool { return true },
		ReleaseAsync:    func() {},
		AsyncCreateCompleteCallback: func(_, _, _ uint64, status types.Status, _ []byte) error {
			sent <- finalResponse{status, h.CreateReplayCache.IsReserved(smbCtx.SessionID, guid)}
			return nil
		},
	}

	asyncId := h.parkCreateOnLeaseBreak(parkCtx, draft, lock.FileHandle(metaHandle),
		[16]byte{}, 2*time.Second, true)
	if asyncId == 0 {
		t.Fatal("parkCreateOnLeaseBreak refused to park")
	}
	if !h.PendingCreateRegistry.MarkStarted(asyncId) {
		t.Fatal("MarkStarted: pending CREATE not registered")
	}

	select {
	case got := <-sent:
		if got.status != types.StatusSharingViolation {
			t.Fatalf("parked CREATE final status = %s, want SHARING_VIOLATION", got.status)
		}
		if got.reservedAtSend {
			t.Fatal("CreateGuid still reserved when the parked CREATE's final response was sent: " +
				"a replay racing that response resolves to FILE_NOT_AVAILABLE instead of the terminal status")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parked CREATE never delivered a final response")
	}
}
