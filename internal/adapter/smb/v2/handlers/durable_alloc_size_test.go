package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestReopen2_V1Oplock_RestoresRequestedAllocSize covers
// smb2.durable-open.alloc-size:
//
// A CREATE that carries an SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create context
// ([MS-SMB2] 2.2.13.2.2) reports a non-zero, cluster-aligned out.alloc_size on
// the initial open (handled by #875 in completeCreateAfterBreak). The test then
// disconnects and durably reopens the handle, asserting the SAME allocation
// size on reopen even though the file is still empty.
//
// The requested reservation is in-memory per-handle state, lost on disconnect.
// Unless it is persisted into the durable handle and restored on reconnect, the
// reconnect CREATE response falls back to the file's bare cluster-aligned size
// (0 for an empty file), so out.alloc_size drops to 0 — exactly the wire
// failure this test reproduces. This exercises the durable-reconnect response
// branch in Create() (distinct from the normal/post-break branch #875 fixed).
func TestReopen2_V1Oplock_RestoresRequestedAllocSize(t *testing.T) {
	e := setupReopen2Env(t)

	const requestedAlloc = uint64(1)      // 1-byte request rounds up to one cluster.
	const wantAlloc = uint64(clusterSize) // 4096 (matches smbtorture initial_alloc_size).

	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, _ := e.h.Registry.GetRootHandle(e.tree.ShareName)
	file, _, _ := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	existingHandle, _ := metadata.EncodeFileHandle(file)

	const sessionID = uint64(1)
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
	smbCtx := e.makeSMBCtx(sessionID)

	// Initial open: BATCH oplock + V1 durable request + an AllocationSize
	// reservation. completeCreateAfterBreak must report a non-zero alloc.
	draft := (&createDraft{
		req: &CreateRequest{
			FileName:           "durable.txt",
			DesiredAccess:      0x001F01FF,
			ShareAccess:        0x07,
			CreateDisposition:  types.FileOpen,
			OplockLevel:        OplockLevelBatch,
			CreateContexts:     []CreateContext{dhnqContext()},
			RequestedAllocSize: requestedAlloc,
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
	}).finalize()

	resp := e.h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial open: status=0x%08x", uint32(resp.Status))
	}
	if resp.AllocationSize != wantAlloc {
		t.Fatalf("initial open AllocationSize = %d, want %d", resp.AllocationSize, wantAlloc)
	}
	of, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("OpenFile not registered")
	}
	if !of.IsDurable {
		t.Fatal("initial open: durability not granted (V1 batch oplock)")
	}
	if of.RequestedAllocSize != requestedAlloc {
		t.Fatalf("initial open: OpenFile.RequestedAllocSize = %d, want %d", of.RequestedAllocSize, requestedAlloc)
	}
	persistedFileID := of.FileID

	// Disconnect, then durably reopen on a fresh session. The reopen must report
	// the same allocation size as the initial open even though the file is
	// empty — the reservation must survive the disconnect via the persisted
	// durable handle.
	e.disconnect(sessionID)

	const sessionID2 = uint64(2)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateContexts:    []CreateContext{dhncContext(persistedFileID)},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	if rcResp.Status != types.StatusSuccess {
		t.Fatalf("reconnect: status=0x%08x, want SUCCESS", uint32(rcResp.Status))
	}
	if rcResp.CreateAction != types.FileOpened {
		t.Errorf("reconnect CreateAction = %d, want FileOpened", rcResp.CreateAction)
	}
	if rcResp.AllocationSize != wantAlloc {
		t.Fatalf("reconnect AllocationSize = %d, want %d (reservation lost across disconnect)",
			rcResp.AllocationSize, wantAlloc)
	}

	// The restored OpenFile must also carry the reservation so a subsequent
	// QUERY_INFO on the reconnected handle stays consistent with CREATE.
	rof, ok := e.h.GetOpenFile(rcResp.FileID)
	if !ok {
		t.Fatal("reconnected OpenFile not registered")
	}
	if rof.RequestedAllocSize != requestedAlloc {
		t.Errorf("reconnected OpenFile.RequestedAllocSize = %d, want %d", rof.RequestedAllocSize, requestedAlloc)
	}
}
