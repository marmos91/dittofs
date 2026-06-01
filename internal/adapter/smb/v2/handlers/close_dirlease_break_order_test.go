package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// orderRecordingLockManager wraps a real lock.LockManager and, on each
// ReleaseLeaseForHandle call, invokes a probe so the test can observe handler
// state (the open-file table) at the exact moment the lease record is dropped.
//
// ReleaseLeaseForHandle is the no-ACK break-completion path: when a directory
// lease holder closes its conflicting handle in response to a dir-lease break,
// the release signals any WaitForBreakCompletion waiter (the SET_INFO-rename
// dst-parent Handle-break wait) via signalBreakWaitLocked. The waiter then
// re-runs checkParentDirRenameConflict against h.files. For that recheck to be
// correct, the closing holder's OpenFile MUST already be gone from h.files when
// the release fires — otherwise the woken rename observes a stale holder and
// returns a spurious STATUS_SHARING_VIOLATION (the intermittent
// smb2.dirlease.rename_dst_parent phase-2 flake).
type orderRecordingLockManager struct {
	lock.LockManager
	onRelease func(handleKey string, leaseKey [16]byte)
}

func (m *orderRecordingLockManager) ReleaseLeaseForHandle(ctx context.Context, handleKey string, leaseKey [16]byte) error {
	if m.onRelease != nil {
		m.onRelease(handleKey, leaseKey)
	}
	return m.LockManager.ReleaseLeaseForHandle(ctx, handleKey, leaseKey)
}

// TestClose_DirLeaseRelease_AfterOpenFileRemoval is a regression test for the
// intermittent smbtorture smb2.dirlease.rename_dst_parent phase-2 failure
// ("waiting for a lease break timed out" + STATUS_SHARING_VIOLATION).
//
// Root cause: the CLOSE handler released the per-handle directory lease record
// (which signals WaitForBreakCompletion waiters via the no-ACK break-completion
// path) BEFORE it removed the OpenFile from h.files. A SET_INFO rename parked in
// breakDstParentDirHandleLeasesForRename -> WaitForBreakCompletion would wake on
// that early signal, re-run checkParentDirRenameConflict while the closing
// holder's OpenFile was still in the table, observe the stale holder, and return
// STATUS_SHARING_VIOLATION instead of STATUS_OK. The race was scheduler-timed
// (the rename goroutine usually lost to WaitAndDeleteOpenFile), hence the
// intermittency.
//
// The fix reorders CLOSE so WaitAndDeleteOpenFile (open-file removal) runs
// BEFORE releaseHandleLeaseRecord (lease release + waiter signal). This test
// asserts that invariant directly: at the instant ReleaseLeaseForHandle fires,
// GetOpenFile for the closing handle must already report "not found".
func TestClose_DirLeaseRelease_AfterOpenFileRemoval(t *testing.T) {
	h := NewHandler()

	var openFilePresentAtRelease bool
	var releaseObserved bool

	var fileID [16]byte
	copy(fileID[:], []byte{0xde, 0xad, 0xbe, 0xef})

	probeMgr := &orderRecordingLockManager{
		LockManager: lock.NewManager(),
		onRelease: func(_ string, _ [16]byte) {
			releaseObserved = true
			// Observe the handler's open-file table AT THE MOMENT of release.
			// With the fix, the OpenFile is already gone; with the bug it is
			// still present (release ran before WaitAndDeleteOpenFile).
			if _, ok := h.GetOpenFile(fileID); ok {
				openFilePresentAtRelease = true
			}
		},
	}
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: probeMgr}, nil)

	const shareName = "share"
	dirMetaHandle := metadata.FileHandle("dst-parent-dir-handle")
	leaseKey := [16]byte{0x01}

	// Grant a real RH directory lease on the dst-parent handle so the release
	// path takes the IsDirectory break-completion-signal branch.
	if _, _, err := h.LeaseManager.RequestLease(
		context.Background(),
		lock.FileHandle(dirMetaHandle),
		leaseKey,
		[16]byte{}, // parentLeaseKey
		1,          // sessionID
		[16]byte{}, // clientGUID
		"owner-1",  // ownerID
		"client-1", // clientID
		shareName,
		lock.LeaseStateRead|lock.LeaseStateHandle,
		true, // isDirectory
	); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	// Put the dir lease into the Breaking state, mirroring the rename path's
	// dst-parent dir-lease break. Without a Breaking lease the release path's
	// signal branch (gated on lock.Lease.Breaking) is inert. Use the async
	// (no-ACK, no-wait) dispatch so the lease lands in Breaking=true without
	// blocking the test on a holder ack that never arrives.
	if err := h.LeaseManager.BreakParentDirLeasesOnContentChangeAsync(
		lock.FileHandle(dirMetaHandle),
		shareName,
		"",
		[16]byte{},
		false,
	); err != nil {
		t.Fatalf("BreakParentDirLeasesOnContentChangeAsync: %v", err)
	}

	// Register the holder's OpenFile (the conflicting dst-parent dir handle).
	openFile := &OpenFile{
		FileID:         fileID,
		FileName:       "dst-parent",
		Path:           "/share/dst-parent",
		IsDirectory:    true,
		ShareName:      shareName,
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		MetadataHandle: dirMetaHandle,
	}
	h.StoreOpenFile(openFile)

	ctx := &SMBHandlerContext{
		Context:   context.Background(),
		SessionID: 1,
		ShareName: shareName,
	}

	resp, err := h.Close(ctx, &CloseRequest{FileID: fileID, Flags: 0})
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Close returned nil response")
	}

	if !releaseObserved {
		t.Fatal("ReleaseLeaseForHandle was never called during CLOSE; " +
			"test cannot validate the ordering invariant")
	}
	if openFilePresentAtRelease {
		t.Fatal("CLOSE released the directory lease (and signaled break-completion " +
			"waiters) while the closing holder's OpenFile was STILL in h.files. " +
			"A SET_INFO-rename waiting in WaitForBreakCompletion would wake and " +
			"observe the stale holder, returning a spurious STATUS_SHARING_VIOLATION " +
			"(smb2.dirlease.rename_dst_parent phase-2 flake). The open-file removal " +
			"must precede the lease release.")
	}

	// Sanity: the OpenFile must be gone after CLOSE.
	if _, ok := h.GetOpenFile(fileID); ok {
		t.Fatal("OpenFile still present after CLOSE")
	}
}
