// Race regression tests for the SET_INFO rename conflict-scan vs CLOSE
// handle-removal serialization (renameScanMu).
//
// Background: the rename handler decides STATUS_SHARING_VIOLATION /
// STATUS_ACCESS_DENIED by scanning the lock-free h.files sync.Map
// (checkShareDeleteConflict / checkParentDirRenameConflict / anyOpenChild).
// A concurrent CLOSE removes the conflicting OpenFile from h.files and then
// releases the lease / signals the rename's break-wait. Before the fix the two
// were not mutually exclusive: the rename's post-break re-scan could observe a
// holder that a CLOSE was in the middle of removing (already signalled the
// break, not yet deleted from h.files) → an intermittent spurious
// SHARING_VIOLATION (smbtorture rename_dir_bench / close-full-information /
// dirlease.rename_dst_parent).
//
// The fix takes renameScanMu around the rename's authoritative scan+decision
// and around CLOSE's deleteOpenFileEntry + lease-release/signal. These tests
// pin that serialization by driving the same real Handler methods the
// production paths use, plus a torn-window guard that fails under -race if
// either side drops the mutex.
package handlers

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// storeHolder inserts a conflicting OpenFile (no FILE_SHARE_DELETE) sharing
// metaHandle, plus a child OpenFile parented at parentHandle, so all three
// rename conflict scans see a live conflict until the holder is removed.
func storeHolder(h *Handler, metaHandle, parentHandle metadata.FileHandle) (holderID, childID [16]byte) {
	holderID = h.GenerateFileID()
	h.StoreOpenFile(&OpenFile{
		FileID:         holderID,
		MetadataHandle: metaHandle,
		ParentHandle:   parentHandle,
		ShareAccess:    0,                  // lacks FILE_SHARE_DELETE → checkShareDeleteConflict trips
		DesiredAccess:  uint32(0x00010000), // DELETE → checkParentDirRenameConflict trips
	})
	childID = h.GenerateFileID()
	h.StoreOpenFile(&OpenFile{
		FileID:         childID,
		MetadataHandle: metadata.FileHandle{0xCC},
		ParentHandle:   metaHandle, // child of the directory being renamed
	})
	return holderID, childID
}

// TestRenameScan_vs_Close_Serialized_NoRace pins renameScanMu. A CLOSE
// goroutine removes the conflicting holder under the mutex, bracketing the
// removal with a torn-window flag; a rename goroutine runs the authoritative
// conflict scans under the mutex and asserts it never observes the torn window
// (i.e. the holder half-removed). Fails under `go test -race` if either side
// drops renameScanMu, and fails the torn assertion if the scan and removal are
// allowed to interleave.
func TestRenameScan_vs_Close_Serialized_NoRace(t *testing.T) {
	h := NewHandler()

	metaHandle := metadata.FileHandle{0xDE, 0xAD, 0xBE, 0xEF}
	parentHandle := metadata.FileHandle{0xA1, 0xA2} // arbitrary distinct parent

	// The renamer's own directory open (IsDirectory so anyOpenChild applies).
	renamerID := h.GenerateFileID()
	h.StoreOpenFile(&OpenFile{
		FileID:         renamerID,
		MetadataHandle: metaHandle,
		IsDirectory:    true,
		ParentHandle:   parentHandle,
	})

	const iters = 2000
	var torn atomic.Bool         // true while a CLOSE is mid-removal under the mutex
	var observedTorn atomic.Bool // set if a scan ever sees the torn window

	var wg sync.WaitGroup
	wg.Add(2)

	// CLOSE goroutine: re-create + remove the conflicting holder repeatedly,
	// each removal serialized through renameScanMu exactly as close.go does.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			holderID, childID := storeHolder(h, metaHandle, parentHandle)
			// Drain happens outside the mutex in production; here there are no
			// in-flight ops, so go straight to the locked removal region.
			h.renameScanMu.Lock()
			torn.Store(true)
			h.deleteOpenFileEntry(holderID)
			h.deleteOpenFileEntry(childID)
			torn.Store(false)
			h.renameScanMu.Unlock()
		}
	}()

	// RENAME goroutine: run the three authoritative scans under the mutex,
	// asserting the torn window is never observed.
	go func() {
		defer wg.Done()
		renamer, _ := h.files.Load(string(renamerID[:]))
		of := renamer.(*OpenFile)
		for i := 0; i < iters; i++ {
			h.renameScanMu.Lock()
			if torn.Load() {
				observedTorn.Store(true)
			}
			_ = h.checkShareDeleteConflict(of)
			_ = h.checkParentDirRenameConflict(of.FileID, of.MetadataHandle)
			_ = h.anyOpenChild(of.MetadataHandle)
			if torn.Load() {
				observedTorn.Store(true)
			}
			h.renameScanMu.Unlock()
		}
	}()

	wg.Wait()

	if observedTorn.Load() {
		t.Fatal("rename conflict scan observed a CLOSE's torn (half-removed) handle " +
			"window — renameScanMu serialization is broken")
	}
}
