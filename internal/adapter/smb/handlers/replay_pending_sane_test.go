package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// noOplockNoneOpen drives a real CREATE of durable.txt with OplockLevel=NONE
// and a DH2Q CreateGuid through completeCreateAfterBreak, mirroring client2's
// io21 in smb2.replay.dhv2-pending1n-vs-{oplock,lease}-sane. NONE never grants
// V2 durability, so openFile.CreateGuid stays zero — but the open must still be
// replay-cacheable via ReplayCreateGuid.
func (e *reopen2Env) noOplockNoneOpen(t *testing.T, sessionID uint64, createGuid [16]byte) (*CreateResponse, *OpenFile) {
	t.Helper()

	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, err := e.h.Registry.GetRootHandle(e.tree.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	file, _, err := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	if err != nil || file == nil {
		t.Fatalf("lookup durable.txt: file=%v err=%v", file, err)
	}
	existingHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	const fullAccess uint32 = 0x001F01FF
	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     fullAccess,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelNone,
			CreateContexts:    []CreateContext{dh2qContext(createGuid, 300000)},
		},
		tree:           e.tree,
		authCtx:        rootAuthCtx(),
		filename:       "durable.txt",
		baseName:       "durable.txt",
		parentHandle:   rootHandle,
		existingFile:   file,
		existingHandle: existingHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}

	smbCtx := e.makeSMBCtx(sessionID)
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")

	resp := e.h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("none-oplock open: status=0x%08x, want SUCCESS", uint32(resp.Status))
	}
	openFile, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("none-oplock open: OpenFile not registered")
	}
	return resp, openFile
}

// TestReplay_NoOplockOpenIsReplayCacheable is the live regression for
// smb2.replay.dhv2-pending1n-vs-{oplock,lease}-sane. The original prior fix
// (#966) only exercised the resolveCreateReplay unit, which passed while the
// wire failed because a NONE-oplock open never set CreateGuid and was therefore
// never stored in the replay cache — so the replayed io24 created a fresh open
// with a new FileID (off-by-one) instead of replaying the original handle.
//
// This drives the real CREATE path: a NONE-oplock DH2Q open must be cached by
// the requested CreateGuid (ReplayCreateGuid), and a subsequent replayed CREATE
// (FLAGS_REPLAY_OPERATION) must return the SAME FileID via the replay cache,
// not allocate a new open.
func TestReplay_NoOplockOpenIsReplayCacheable(t *testing.T) {
	e := setupReopen2Env(t)
	const sessionID = uint64(31)
	createGuid := [16]byte{0xAB, 0xCD, 0xEF, 0x01}

	resp, openFile := e.noOplockNoneOpen(t, sessionID, createGuid)

	// NONE oplock must NOT have granted V2 durability...
	if openFile.CreateGuid != ([16]byte{}) {
		t.Fatalf("NONE oplock unexpectedly granted V2 durability (CreateGuid=%x)", openFile.CreateGuid)
	}
	// ...but it MUST still be replay-cache-keyed on the requested guid.
	if openFile.ReplayCreateGuid != createGuid {
		t.Fatalf("ReplayCreateGuid=%x, want %x", openFile.ReplayCreateGuid, createGuid)
	}
	if e.h.CreateReplayCache.Lookup(sessionID, createGuid) == nil {
		t.Fatal("NONE-oplock DH2Q open was not stored in the replay cache")
	}

	origFileID := resp.FileID

	// Replay the CREATE (same guid, FLAGS_REPLAY_OPERATION). It must resolve via
	// the replay cache and return the SAME FileID, not a freshly allocated open.
	replayCtx := e.makeSMBCtx(sessionID)
	replayCtx.IsReplay = true
	replayResp, err := e.h.Create(replayCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelNone,
		CreateContexts:    []CreateContext{dh2qContext(createGuid, 300000)},
	})
	if err != nil {
		t.Fatalf("replay Create: %v", err)
	}
	if replayResp.Status != types.StatusSuccess {
		t.Fatalf("replay status=0x%08x, want SUCCESS", uint32(replayResp.Status))
	}
	if replayResp.FileID != origFileID {
		t.Fatalf("replay FileID=%x, want original %x (replay missed the cache → fresh open)",
			replayResp.FileID, origFileID)
	}
}

// TestReplay_PendingReservationFileNotAvailable is the live regression for the
// pending-vs-break window: while the original NONE-oplock CREATE is still parked
// (reserved), a replayed CREATE for the same guid must fail fast with
// STATUS_FILE_NOT_AVAILABLE rather than running a fresh conflict resolution that
// would yield SHARING_VIOLATION (the #966 wire symptom).
func TestReplay_PendingReservationFileNotAvailable(t *testing.T) {
	e := setupReopen2Env(t)
	const sessionID = uint64(32)
	createGuid := [16]byte{0x11, 0x22, 0x33, 0x44}

	// Simulate the parked original: reserve the guid (as Create does before it
	// parks on a pending break) without yet storing a completed entry.
	e.h.CreateReplayCache.Reserve(sessionID, createGuid)

	replayCtx := e.makeSMBCtx(sessionID)
	replayCtx.IsReplay = true
	resp, handled := e.h.resolveCreateReplay(replayCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelNone,
		CreateContexts:    []CreateContext{dh2qContext(createGuid, 300000)},
	})
	if !handled {
		t.Fatal("replay against a reserved (parked) guid must be handled")
	}
	if resp.Status != types.StatusFileNotAvailable {
		t.Fatalf("replay-vs-pending status=0x%08x, want FILE_NOT_AVAILABLE", uint32(resp.Status))
	}
}
