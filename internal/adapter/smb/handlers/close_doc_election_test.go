// Regression coverage for the last-handle delete-on-close election.
//
// Per MS-FSA 2.1.5.4 the unlink fires when the LAST handle on a file closes.
// Both close paths — the explicit CLOSE handler and the LOGOFF /
// tree-disconnect / transport-drop teardown — decide "am I the last handle?"
// by scanning the open-file table for a sibling on the same MetadataHandle,
// and remove themselves from that table later. When two closers on the same
// file both scan before either removes itself, each sees the other, each
// defers the delete to the other, and the file is never unlinked.
//
// Both tests below drive that exact interleaving deterministically by holding
// renameScanMu, which every table removal takes: the closers run their
// last-handle decision and then park on the mutex, so both decisions provably
// precede both removals.
package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// settleForDecision gives both closers time to run their last-handle decision
// and park on renameScanMu. Everything they do first is in-memory (metadata
// and block stores are both the memory backends), so this is generous.
const settleForDecision = 300 * time.Millisecond

// fileStillExists reports whether name is still present under parent.
func fileStillExists(t *testing.T, h *Handler, parent metadata.FileHandle, name string) bool {
	t.Helper()
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	f, _, err := h.Registry.GetMetadataService().LookupCaseInsensitive(authCtx, parent, name)
	return err == nil && f != nil
}

// cloneOpenFileForSecondHandle registers a second open on the same metadata
// file as src, mirroring a second CREATE with FILE_DELETE_ON_CLOSE.
func cloneOpenFileForSecondHandle(h *Handler, src *OpenFile, fileID [16]byte, sessionID uint64, treeID uint32) *OpenFile {
	name := src.Name()
	clone := (&OpenFile{
		FileID:               fileID,
		TreeID:               treeID,
		SessionID:            sessionID,
		ShareName:            src.ShareName,
		DesiredAccess:        src.DesiredAccess,
		GrantedAccess:        src.GrantedAccess,
		MetadataHandle:       src.MetadataHandle,
		PayloadID:            src.GetPayloadID(),
		ShareAccess:          0x07,
		CreateOptions:        types.FileDeleteOnClose,
		InitialDeleteOnClose: true,
	}).WithName(name)
	h.StoreOpenFile(clone)
	return clone
}

// TestClose_ConcurrentLastHandles_StillUnlink drives two concurrent CLOSEs of
// the only two handles on a file, both carrying FILE_DELETE_ON_CLOSE. Exactly
// one of them is the last handle, so the file must be gone once both return.
func TestClose_ConcurrentLastHandles_StillUnlink(t *testing.T) {
	h, smbCtx, _, fileIDA := setupWriteTestShare(t, nil)

	rootHandle, err := h.Registry.GetRootHandle(smbCtx.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	openA, ok := h.GetOpenFile(fileIDA)
	if !ok {
		t.Fatalf("seed open file missing")
	}
	openA.ShareAccess = 0x07
	openA.CreateOptions = types.FileDeleteOnClose
	openA.InitialDeleteOnClose = true

	fileIDB := [16]byte{8}
	cloneOpenFileForSecondHandle(h, openA, fileIDB, smbCtx.SessionID, smbCtx.TreeID)

	// Park both closers after their last-handle decision: every removal from
	// the open-file table takes renameScanMu.
	h.renameScanMu.Lock()

	var wg sync.WaitGroup
	statuses := make([]types.Status, 2)
	for i, fileID := range [][16]byte{fileIDA, fileIDB} {
		wg.Add(1)
		go func(slot int, fid [16]byte) {
			defer wg.Done()
			resp, closeErr := h.Close(&SMBHandlerContext{
				Context:   smbCtx.Context,
				SessionID: smbCtx.SessionID,
				TreeID:    smbCtx.TreeID,
				ShareName: smbCtx.ShareName,
			}, &CloseRequest{FileID: fid})
			if closeErr != nil {
				t.Errorf("Close(%x): %v", fid, closeErr)
				return
			}
			statuses[slot] = resp.Status
		}(i, fileID)
	}

	time.Sleep(settleForDecision)
	h.renameScanMu.Unlock()
	wg.Wait()

	for i, st := range statuses {
		if st != types.StatusSuccess {
			t.Errorf("CLOSE %d returned %v, want STATUS_SUCCESS", i, st)
		}
	}

	if fileStillExists(t, h, rootHandle, "data") {
		t.Fatal("delete-on-close was lost: both closers deferred the unlink to each other and the file survived")
	}
}

// TestCloseFilesWithFilter_TeardownSiblingLeaving_StillUnlink covers the same
// defect on the LOGOFF / tree-disconnect / transport-drop path.
//
// The symmetric two-teardown interleaving the issue sketches does not actually
// lose the unlink: closeFilesWithFilter re-reads openFile.DeletePending at its
// delete gate, after the sibling scan, so whichever teardown propagates second
// still sees the flag the first one wrote. What it does lose is a delete
// deferred TO it: a teardown handle carrying no delete-on-close of its own
// makes its decision in pass 1 and leaves the open-file table only in pass 3,
// so a concurrent closer that scans in between sees a live sibling, defers,
// and nobody unlinks.
//
// The ordering here is deterministic: the teardown is parked on renameScanMu
// (already past its own decision) before the explicit CLOSE starts scanning.
func TestCloseFilesWithFilter_TeardownSiblingLeaving_StillUnlink(t *testing.T) {
	h, smbCtx, _, fileIDA := setupWriteTestShare(t, nil)

	rootHandle, err := h.Registry.GetRootHandle(smbCtx.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	openA, ok := h.GetOpenFile(fileIDA)
	if !ok {
		t.Fatalf("seed open file missing")
	}
	openA.ShareAccess = 0x07

	// A second session holding the same file, this one with
	// FILE_DELETE_ON_CLOSE. It is the handle that must perform the unlink.
	sessB := h.CreateSession("127.0.0.1:12346", false, "test-user", "")
	uidB, gidB := uint32(0), uint32(0)
	sessB.User = &models.User{Username: "test-user", UID: &uidB, Groups: []models.Group{{GID: &gidB}}}
	const treeIDB uint32 = 2
	h.StoreTree(&TreeConnection{
		TreeID:     treeIDB,
		SessionID:  sessB.SessionID,
		ShareName:  smbCtx.ShareName,
		Permission: models.PermissionReadWrite,
	})
	fileIDB := [16]byte{9}
	cloneOpenFileForSecondHandle(h, openA, fileIDB, sessB.SessionID, treeIDB)

	h.renameScanMu.Lock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.CloseAllFilesForSession(smbCtx.Context, smbCtx.SessionID, true /* transport disconnect */)
	}()

	// Let the teardown finish its own delete-on-close decision and park on
	// renameScanMu before the CLOSE below scans for siblings.
	time.Sleep(settleForDecision)

	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, closeErr := h.Close(&SMBHandlerContext{
			Context:   smbCtx.Context,
			SessionID: sessB.SessionID,
			TreeID:    treeIDB,
			ShareName: smbCtx.ShareName,
		}, &CloseRequest{FileID: fileIDB})
		if closeErr != nil {
			t.Errorf("Close: %v", closeErr)
			return
		}
		if resp.Status != types.StatusSuccess {
			t.Errorf("CLOSE returned %v, want STATUS_SUCCESS", resp.Status)
		}
	}()

	time.Sleep(settleForDecision)
	h.renameScanMu.Unlock()
	wg.Wait()

	if fileStillExists(t, h, rootHandle, "data") {
		t.Fatal("delete-on-close was lost: the closer deferred the unlink to a handle that had already decided to leave")
	}
}

// TestElectDeleteOnClose_LastEntrantWins pins the election contract directly:
// closers of one file are totally ordered, every closer but the last defers,
// and the last one deletes — including when it is a handle that carried no
// delete-on-close of its own until an earlier closer propagated one to it.
func TestElectDeleteOnClose_LastEntrantWins(t *testing.T) {
	h := NewHandler()
	metaHandle := metadata.FileHandle("file-1")

	newOpen := func(id byte, initialDOC bool) *OpenFile {
		of := (&OpenFile{
			FileID:               [16]byte{id},
			MetadataHandle:       metaHandle,
			InitialDeleteOnClose: initialDOC,
		}).WithName(OpenName{Path: "f", FileName: "f", ParentHandle: metadata.FileHandle("root")})
		h.StoreOpenFile(of)
		return of
	}

	// Two handles, only the first carrying FILE_DELETE_ON_CLOSE.
	docHolder := newOpen(1, true)
	plain := newOpen(2, false)

	if got, _ := h.electDeleteOnClose(docHolder); got != docDecisionDeferred {
		t.Fatalf("first closer: got %v, want docDecisionDeferred", got)
	}
	if !plain.DeletePending {
		t.Fatal("first closer did not propagate the delete-on-close to the surviving handle")
	}
	if got, _ := h.electDeleteOnClose(plain); got != docDecisionDelete {
		t.Fatalf("last closer: got %v, want docDecisionDelete", got)
	}

	// Handles still in the table but already past their own election never hold
	// a delete back: a third closer arriving behind both of them is itself the
	// last handle.
	third := newOpen(3, true)
	if got, _ := h.electDeleteOnClose(third); got != docDecisionDelete {
		t.Fatalf("closer behind two leaving handles: got %v, want docDecisionDelete", got)
	}
}

// TestCloseFilesWithFilter_DurablePersistedSibling_StillUnlink covers the
// third way a handle leaves the open-file table: the transport-drop path
// persists a durable handle for later reconnect and drops it from the table
// exactly as a full close does. A closer that scans while that handle is still
// listed would defer the unlink to a struct nothing will ever read again, so
// the persist path has to run the election first like every other departure.
func TestCloseFilesWithFilter_DurablePersistedSibling_StillUnlink(t *testing.T) {
	h, smbCtx, _, fileIDA := setupWriteTestShare(t, nil)
	h.DurableStore = newMockDurableStore()

	rootHandle, err := h.Registry.GetRootHandle(smbCtx.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	// The durable handle carries no delete-on-close of its own, so it is
	// eligible to be persisted rather than closed.
	openA, ok := h.GetOpenFile(fileIDA)
	if !ok {
		t.Fatalf("seed open file missing")
	}
	openA.ShareAccess = 0x07
	openA.IsDurable = true
	openA.DurableTimeoutMs = 60000

	// A second session holding the same file with FILE_DELETE_ON_CLOSE.
	sessB := h.CreateSession("127.0.0.1:12347", false, "test-user", "")
	uidB, gidB := uint32(0), uint32(0)
	sessB.User = &models.User{Username: "test-user", UID: &uidB, Groups: []models.Group{{GID: &gidB}}}
	const treeIDB uint32 = 3
	h.StoreTree(&TreeConnection{
		TreeID:     treeIDB,
		SessionID:  sessB.SessionID,
		ShareName:  smbCtx.ShareName,
		Permission: models.PermissionReadWrite,
	})
	fileIDB := [16]byte{10}
	cloneOpenFileForSecondHandle(h, openA, fileIDB, sessB.SessionID, treeIDB)

	h.renameScanMu.Lock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.CloseAllFilesForSession(smbCtx.Context, smbCtx.SessionID, true /* transport disconnect */)
	}()

	// Let the durable handle be persisted and park on renameScanMu before the
	// CLOSE below scans for siblings.
	time.Sleep(settleForDecision)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, closeErr := h.Close(&SMBHandlerContext{
			Context:   smbCtx.Context,
			SessionID: sessB.SessionID,
			TreeID:    treeIDB,
			ShareName: smbCtx.ShareName,
		}, &CloseRequest{FileID: fileIDB}); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	}()

	time.Sleep(settleForDecision)
	h.renameScanMu.Unlock()
	wg.Wait()

	if fileStillExists(t, h, rootHandle, "data") {
		t.Fatal("delete-on-close was lost: the closer deferred the unlink to a handle that had been persisted away")
	}
}
