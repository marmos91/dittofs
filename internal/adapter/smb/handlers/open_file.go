package handlers

import (
	"bytes"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// OpenFile is an alias for types.OpenFile. The handle state is read by the
// handlers and by everything that resolves a FileID, so it cannot sit on
// either side of that boundary. The alias keeps the unqualified spelling.
type OpenFile = types.OpenFile

// OpenName is the name triple of an open handle: full path, name within the
// parent, and parent directory handle. SET_INFO rename replaces all three at
// once, so they are published and read as one immutable value.

// handleOpTracker tracks in-flight operations on a FileID via a WaitGroup.
// Created lazily by AcquireOpenFile; AcquireOpenFile adds and ReleaseOpenFile
// calls Done. WaitAndDeleteOpenFile waits for the WaitGroup to drain before
// removing the OpenFile from the map.
type handleOpTracker struct {
	wg sync.WaitGroup
}

// BeginHandleOp registers an in-flight operation on fileID without checking
// whether the OpenFile is currently present. It is intended for the
// connection dispatcher to call synchronously on the read loop BEFORE
// spawning the request goroutine, so that the handleOps counter for the
// FileID is incremented in wire order (independent of goroutine scheduling).
// Without this, a later CLOSE on the same TCP connection can race ahead of
// an earlier request's goroutine, observe handleOps empty, and delete the
// OpenFile before the prior request calls AcquireOpenFile — yielding a
// spurious STATUS_FILE_CLOSED (smbtorture compound_find.compound_find_close).
//
// The returned release func MUST be called exactly once when the request
// completes. A subsequent in-handler AcquireOpenFile/ReleaseOpenFile pair on
// the same FileID still works correctly: the tracker is shared, so the
// nested Add/Done cancel out and the dispatcher's outer Done fires when the
// request finishes.

func (h *Handler) BeginHandleOp(fileID [16]byte) func() {
	key := string(fileID[:])
	v, _ := h.handleOps.LoadOrStore(key, &handleOpTracker{})
	tracker := v.(*handleOpTracker)
	tracker.wg.Add(1)
	return func() { tracker.wg.Done() }
}

// AcquireOpenFile retrieves an open file by FileID and registers an in-flight
// operation on it. The caller MUST call ReleaseOpenFile when done. Returns
// nil,false when the handle is not found. This prevents a CLOSE on a concurrent
// goroutine from deleting the OpenFile before the caller finishes using it
// (smbtorture compound_find.compound_find_close).

func (h *Handler) AcquireOpenFile(fileID [16]byte) (*OpenFile, bool) {
	key := string(fileID[:])
	// Load-or-store the tracker; add to the WaitGroup BEFORE checking the
	// files map so that a concurrent WaitAndDeleteOpenFile sees our Add.
	v, _ := h.handleOps.LoadOrStore(key, &handleOpTracker{})
	tracker := v.(*handleOpTracker)
	tracker.wg.Add(1)

	f, ok := h.files.Load(key)
	if !ok {
		// File was already deleted; undo the Add.
		tracker.wg.Done()
		return nil, false
	}
	return f.(*OpenFile), true
}

// ReleaseOpenFile marks an in-flight operation on fileID as complete.
// Must be called exactly once for each successful AcquireOpenFile.

func (h *Handler) ReleaseOpenFile(fileID [16]byte) {
	key := string(fileID[:])
	if v, ok := h.handleOps.Load(key); ok {
		v.(*handleOpTracker).wg.Done()
	}
}

// WaitAndDeleteOpenFile waits for in-flight operations on fileID to drain,
// then deletes the OpenFile from the map and revokes resume keys. This
// replaces DeleteOpenFile for the CLOSE handler to prevent the race where
// CLOSE deletes a handle that QueryDirectory is about to look up.

func (h *Handler) WaitAndDeleteOpenFile(fileID [16]byte) {
	h.DrainHandleOps(fileID)
	h.deleteOpenFileEntry(fileID)
}

// DrainHandleOps blocks until all in-flight operations registered on fileID
// (via BeginHandleOp / AcquireOpenFile) have completed, then drops the tracker.
// Split out of WaitAndDeleteOpenFile so the CLOSE handler can drain the closing
// handle's in-flight ops OUTSIDE renameScanMu (the drain can be unbounded if a
// slow QueryDirectory is in flight) and take the mutex only for the actual
// map removal + lease release/signal — see close.go step 10/11.

func (h *Handler) DrainHandleOps(fileID [16]byte) {
	key := string(fileID[:])
	if v, ok := h.handleOps.Load(key); ok {
		v.(*handleOpTracker).wg.Wait()
		h.handleOps.Delete(key)
	}
}

// deleteOpenFileEntry removes the OpenFile from the files map and revokes its
// replay/resume state. The caller is responsible for any draining (see
// DrainHandleOps). The CLOSE path holds renameScanMu across this call so a
// concurrent rename's conflict re-scan cannot observe a half-removed handle.

func (h *Handler) deleteOpenFileEntry(fileID [16]byte) {
	key := string(fileID[:])
	h.forgetReplayState(fileID)
	h.files.Delete(key)
	h.resumeKeys.revoke(fileID)
}

// DeleteOpenFile removes an open file by FileID and revokes any
// resume keys issued for this handle (used by FSCTL_SRV_COPYCHUNK and the
// session-cleanup path closeFilesWithFilter). Also clears the handleOps
// tracker so trackers created by BeginHandleOp on this FileID do not leak.

func (h *Handler) DeleteOpenFile(fileID [16]byte) {
	key := string(fileID[:])
	h.forgetReplayState(fileID)
	h.files.Delete(key)
	h.handleOps.Delete(key)
	h.resumeKeys.revoke(fileID)
}

// forgetReplayState drops both CREATE (by CreateGuid via the OpenFile)
// and LOCK (by FileID) replay-cache entries for a handle that is being
// closed. The cache windows are only meaningful while a retry could
// still arrive — once the handle is gone, so is any legitimate replay.

func (h *Handler) forgetReplayState(fileID [16]byte) {
	if h.CreateDRC != nil {
		if v, ok := h.files.Load(string(fileID[:])); ok {
			// Forget by the replay-cache key (the requested CreateGuid),
			// which is set even for non-durable opens that never populate
			// CreateGuid. Falls back to CreateGuid for older code paths.
			of := v.(*OpenFile)
			guid := of.ReplayCreateGuid
			if guid == ([16]byte{}) {
				guid = of.CreateGuid
			}
			if guid != ([16]byte{}) {
				h.CreateDRC.Forget(guid)
			}
		}
	}
	if h.LockDRC != nil {
		h.LockDRC.ForgetFile(fileID)
	}
}

// isFileDeletePending reports whether any existing open on the same file
// (identified by its metadata handle) has DeletePending set. Per MS-FSA
// 2.1.5.1.2 and MS-SMB2 3.3.5.9: a subsequent open on a delete-pending
// file MUST fail with STATUS_DELETE_PENDING. The check runs BEFORE oplock
// break dispatch so the holder's oplock remains intact.
//
// Required by smbtorture smb2.oplock.doc: tree1 opens with Batch oplock,
// sets delete-on-close; tree2's open must return STATUS_DELETE_PENDING
// without triggering a break.
func (h *Handler) isFileDeletePending(fileHandle metadata.FileHandle) bool {
	pending := false
	h.files.Range(func(_, value any) bool {
		existing := value.(*OpenFile)
		existingHandle := existing.GetMetadataHandle()
		if existing.IsPipe || len(existingHandle) == 0 {
			return true
		}
		if !bytes.Equal(existingHandle, fileHandle) {
			return true
		}
		// DeletePending is concurrently written by CLOSE DOC propagation under
		// existing.mu — read it under the read lock.
		existing.RLock()
		dp := existing.DeletePending
		existing.RUnlock()
		if dp {
			pending = true
			return false
		}
		return true
	})
	return pending
}

// isFileOrBaseDeletePending extends isFileDeletePending to also check whether
// a deferred base-file delete is pending across stream/base handles.
//
// Per Samba semantics (also matches WPTS expectations): a stream open does
// NOT inherit the base file's DOC pending state. Streams are tracked as
// independent fsps; the base's mark-for-delete only fails subsequent stream
// opens once the base has actually been unlinked and the delete is being
// deferred for outstanding stream handles (BaseFileDeletePending).
//
// Cases handled here:
//   - Opening a base file: reject if any stream handle on the same base
//     carries BaseFileDeletePending (base was unlinked, delete deferred).
//   - Opening a stream:   reject if any handle on the base file or sibling
//     stream carries BaseFileDeletePending.
//
// The open is identified by its resolved parent directory handle and its
// parent-relative name (e.g. "file" or "file:Stream One"), not by its full
// path. A base file and its streams are siblings in one directory, so that
// pair relates them by identity; the full path cannot, because it reproduces
// whatever spelling the client sent.

func (h *Handler) isFileOrBaseDeletePending(
	fileHandle metadata.FileHandle,
	parentHandle metadata.FileHandle,
	fileName string,
) bool {
	// Fast path: direct metadata-handle match against an existing handle
	// whose own DeletePending is set. Covers the same-file re-open case
	// (smbtorture smb2.oplock.doc, smb2.streams.delete).
	if h.isFileDeletePending(fileHandle) {
		return true
	}

	openBase := adsBaseName(fileName) // non-empty if fileName is a stream
	pending := false
	h.files.Range(func(_, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe || len(existing.GetMetadataHandle()) == 0 {
			return true
		}
		// BaseFileDeletePending is concurrently written by CLOSE deferred-delete
		// propagation under existing.mu — read it under the read lock.
		existing.RLock()
		bdp := existing.BaseFileDeletePending
		existing.RUnlock()
		if !bdp {
			return true
		}
		existingName := existing.Name()
		if !bytes.Equal(existingName.ParentHandle, parentHandle) {
			return true
		}
		existingBase := adsBaseName(existingName.FileName)
		if openBase == "" {
			// Opening a base file: match against any stream of this base.
			if strings.EqualFold(existingBase, fileName) {
				pending = true
				return false
			}
		} else {
			// Opening a stream: match against a sibling stream sharing the
			// same base name, or against a base-file handle of that base.
			if strings.EqualFold(existingBase, openBase) ||
				strings.EqualFold(existingName.FileName, openBase) {
				pending = true
				return false
			}
		}
		return true
	})
	return pending
}

// checkShareModeConflict checks if opening a file with the given access and sharing
// modes would conflict with any existing opens on the same file or related
// streams. Per MS-FSA 2.1.5.1.2.2 ("Algorithm to Check Sharing Access to an Existing Stream or Directory") + Samba semantics, share mode enforcement is:
//   - Same stream (same metadata handle) → always checked
//   - Base file vs its stream (or vice versa) → checked
//   - Stream A vs Stream B (different streams, same base) → NOT checked
//
// Returns true if a conflict exists (CREATE should fail with STATUS_SHARING_VIOLATION).
// The open is identified by its resolved parent directory handle and its
// parent-relative name, for the reason given on isFileOrBaseDeletePending.

func (h *Handler) checkShareModeConflict(
	fileHandle metadata.FileHandle,
	newDesiredAccess, newShareAccess uint32,
	parentHandle metadata.FileHandle,
	fileName string,
) bool {
	const (
		fileShareRead   = uint32(0x01)
		fileShareWrite  = uint32(0x02)
		fileShareDelete = uint32(0x04)

		// Access mask bits per MS-SMB2
		fileReadData   = uint32(0x00000001)
		fileWriteData  = uint32(0x00000002)
		fileAppendData = uint32(0x00000004)
		fileExecute    = uint32(0x00000020)
		deleteAccess   = uint32(0x00010000)
		genericRead    = uint32(0x80000000)
		genericWrite   = uint32(0x40000000)
		genericAll     = uint32(0x10000000)
		maxAllowed     = uint32(0x02000000)
	)

	// Stat-only opens (FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES /
	// READ_CONTROL / SYNCHRONIZE only) impose no share-mode constraint per
	// MS-SMB2 §3.3.5.9 + Samba `share_conflict` (source3/locking/share_mode_lock.c)
	// + `is_stat_open` (source3/smbd/open.c). smbtorture smb2.oplock.batch8 /
	// exclusive4 expect a stat-only second open on a BATCH/EXCLUSIVE holder
	// with ShareAccess=NONE to succeed with NT_STATUS_OK (no break, no
	// sharing violation).
	if isStatOnlyOpen(newDesiredAccess) {
		return false
	}

	// Helper: does access mask imply read?
	hasRead := func(access uint32) bool {
		return access&(fileReadData|fileExecute|genericRead|genericAll|maxAllowed) != 0
	}
	// Helper: does access mask imply write?
	// MAXIMUM_ALLOWED resolves to the maximal granted rights, which include
	// write+delete on a writable handle. Samba's share_conflict evaluates the
	// resolved effective mask; DittoFS keeps the raw 0x02000000 bit in
	// OpenFile.DesiredAccess (ExpandGenericMask strips GENERIC_* but not
	// MAXIMUM_ALLOWED), so it must be treated as write/delete here too —
	// otherwise a MAXIMUM_ALLOWED opener is wrongly treated as read-only and
	// bypasses SHARE_WRITE / SHARE_DELETE enforcement (matches hasRead above and
	// the module-level hasWriteAccess/hasDeleteAccess).
	hasWrite := func(access uint32) bool {
		return access&(fileWriteData|fileAppendData|genericWrite|genericAll|maxAllowed) != 0
	}
	// Helper: does access mask imply delete?
	hasDelete := func(access uint32) bool {
		return access&(deleteAccess|genericAll|maxAllowed) != 0
	}

	newBase := adsBaseName(fileName)

	conflict := false
	h.files.Range(func(key, value any) bool {
		existing := value.(*OpenFile)
		if existing.IsPipe {
			return true
		}
		existingHandle := existing.GetMetadataHandle()
		if len(existingHandle) == 0 {
			return true
		}

		// Same stream (same metadata handle) → full share mode check.
		// Base file vs its stream (or vice versa) → DELETE-only check.
		// Stream A vs stream B (different streams) → skip.
		sameFile := bytes.Equal(existingHandle, fileHandle)
		crossStream := false
		if !sameFile {
			existingName := existing.Name()
			// A base file and its streams live in one directory; a handle
			// anywhere else cannot be related to this open.
			if !bytes.Equal(existingName.ParentHandle, parentHandle) {
				return true
			}
			existingBase := adsBaseName(existingName.FileName)
			baseVsStream := false
			if newBase == "" && existingBase != "" {
				baseVsStream = strings.EqualFold(existingBase, fileName)
			} else if newBase != "" && existingBase == "" {
				baseVsStream = strings.EqualFold(newBase, existingName.FileName)
			}
			if !baseVsStream {
				return true
			}
			crossStream = true
		}

		// Cross-stream: only DELETE sharing enforced per Samba.
		if crossStream {
			if hasDelete(existing.DesiredAccess) && newShareAccess&fileShareDelete == 0 {
				conflict = true
				return false
			}
			if hasDelete(newDesiredAccess) && existing.ShareAccess&fileShareDelete == 0 {
				conflict = true
				return false
			}
			return true
		}

		// Same-stream: full share mode check.
		if !hasRead(existing.DesiredAccess) &&
			!hasWrite(existing.DesiredAccess) &&
			!hasDelete(existing.DesiredAccess) &&
			existing.DesiredAccess&fileAppendData == 0 {
			return true
		}

		if hasRead(existing.DesiredAccess) && newShareAccess&fileShareRead == 0 {
			conflict = true
			return false
		}
		if hasWrite(existing.DesiredAccess) && newShareAccess&fileShareWrite == 0 {
			conflict = true
			return false
		}
		if hasDelete(existing.DesiredAccess) && newShareAccess&fileShareDelete == 0 {
			conflict = true
			return false
		}

		if hasRead(newDesiredAccess) && existing.ShareAccess&fileShareRead == 0 {
			conflict = true
			return false
		}
		if hasWrite(newDesiredAccess) && existing.ShareAccess&fileShareWrite == 0 {
			conflict = true
			return false
		}
		if hasDelete(newDesiredAccess) && existing.ShareAccess&fileShareDelete == 0 {
			conflict = true
			return false
		}

		return true
	})
	return conflict
}

// lookupCaseInsensitive is a thin shim around
// MetadataService.LookupCaseInsensitive that keeps the existing
// (handler, metaSvc, parent, name) call signature used across the SMB
// handlers. NTFS-style paths are case-insensitive; DittoFS preserves the
// original on-disk casing and returns it via the second result.

func (h *Handler) lookupCaseInsensitive(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.Service,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, string, error) {
	return metaSvc.LookupCaseInsensitive(authCtx, parentHandle, name)
}

// checkShareDeleteConflict checks if any other open handle on the same file
// lacks FILE_SHARE_DELETE in its ShareAccess. MS-FSA 2.1.5.15.12
// ("FileRenameInformation") states no share-mode check; requiring all other
// opens to permit delete sharing follows Samba `can_rename`. Returns true if a conflict
// exists (rename should be blocked with STATUS_SHARING_VIOLATION).
func (h *Handler) checkShareDeleteConflict(renameFile *OpenFile) bool {
	const fileShareDelete = uint32(0x04) // FILE_SHARE_DELETE

	renameHandle := renameFile.GetMetadataHandle()
	if len(renameHandle) == 0 {
		return false
	}

	var culprit *OpenFile
	h.files.Range(func(key, value any) bool {
		other := value.(*OpenFile)
		// Skip the handle being renamed
		if other.FileID == renameFile.FileID {
			return true
		}
		// Only check handles to the same file (same metadata handle)
		otherHandle := other.GetMetadataHandle()
		if len(otherHandle) == 0 {
			return true
		}
		if !bytes.Equal(otherHandle, renameHandle) {
			return true
		}
		// If this other handle does not allow delete sharing, conflict
		if other.ShareAccess&fileShareDelete == 0 {
			culprit = other
			return false // Stop iterating
		}
		return true
	})
	if culprit != nil {
		// #1652: dump the offending holder so a spurious/intermittent
		// SHARING_VIOLATION on rename is diagnosable — the call-site log only
		// records the renamer. The conflict is expected only when a live
		// sibling open lacks FILE_SHARE_DELETE; a holder on a different
		// session/tree, a durable handle, or a delete-pending stub pointing
		// here is the fingerprint of a leaked/stale open.
		logRenameConflictHolder("source-file share-delete gate", renameFile, culprit)
		return true
	}
	return false
}

// checkParentDirRenameConflict applies the destination-parent share-mode rule
// from MS-FSA 2.1.5.15.12 ("FileRenameInformation"): the rename opens the destination directory
// with DesiredAccess FILE_ADD_FILE|SYNCHRONIZE and ShareAccess
// FILE_SHARE_READ|FILE_SHARE_WRITE. Linking a new name into a directory is
// therefore a WRITE against that directory, not a delete of it, so an existing
// open conflicts only when it denies write sharing, or already holds DELETE
// access — which the rename's withheld share-delete is incompatible with. A
// holder that merely lacks FILE_SHARE_DELETE does not conflict; nothing in the
// rename asks to delete the destination parent.
//
// Only the renamer's own handle is excluded, by FileID. Another open on the
// renamer's own session still counts, because the implicit open is a fresh
// open evaluated against the whole open list.
//
// Caller passes the destination parent handle (same as source parent for a
// same-directory rename). Returns true on conflict.
func (h *Handler) checkParentDirRenameConflict(renamer *OpenFile, dstParent metadata.FileHandle) bool {
	if len(dstParent) == 0 {
		return false
	}
	var culprit *OpenFile
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == renamer.FileID {
			return true
		}
		otherHandle := other.GetMetadataHandle()
		if len(otherHandle) == 0 {
			return true
		}
		if !bytes.Equal(otherHandle, dstParent) {
			return true
		}
		// Stat-only opens (READ_ATTRIBUTES / WRITE_ATTRIBUTES / SYNCHRONIZE /
		// READ_CONTROL only) impose no share-mode constraint per MS-SMB2
		// §3.3.5.9.8 + Samba `is_lease_stat_open`. smbtorture rename.msword
		// opens the parent dir stat-only with ShareAccess=0 and expects the
		// rename to succeed; without this filter its lack of FILE_SHARE_WRITE
		// would falsely trip the conflict.
		if isStatOnlyOpen(other.DesiredAccess) {
			return true
		}
		if other.ShareAccess&smbShareWrite == 0 || hasDeleteAccess(other.DesiredAccess) {
			culprit = other
			return false
		}
		return true
	})
	if culprit != nil {
		logRenameConflictHolder("dst-parent share-mode gate", renamer, culprit)
		return true
	}
	return false
}

// snapshotOpenChildren returns the metadata handles of every open file whose
// ParentHandle equals dirHandle. Caller must read h.files only once; iterating
// twice could observe inconsistent open state across a concurrent CLOSE.

func (h *Handler) snapshotOpenChildren(dirHandle metadata.FileHandle) []metadata.FileHandle {
	var children []metadata.FileHandle
	h.files.Range(func(_, value any) bool {
		of := value.(*OpenFile)
		parent := of.Name().ParentHandle
		ofHandle := of.GetMetadataHandle()
		if len(parent) == 0 || len(ofHandle) == 0 {
			return true
		}
		if !bytes.Equal(parent, dirHandle) {
			return true
		}
		children = append(children, ofHandle)
		return true
	})
	return children
}

// anyOpenChild reports whether any open file currently has ParentHandle ==
// dirHandle. Cheaper than snapshotOpenChildren when only the boolean is
// needed (post-break recheck in the directory-rename path).

func (h *Handler) anyOpenChild(dirHandle metadata.FileHandle) bool {
	open := false
	h.files.Range(func(_, value any) bool {
		of := value.(*OpenFile)
		parent := of.Name().ParentHandle
		if len(parent) == 0 {
			return true
		}
		if !bytes.Equal(parent, dirHandle) {
			return true
		}
		open = true
		return false
	})
	return open
}

// hasOpenHandleOnFile reports whether any open file handle (other than the
// renamer's own handle) currently references targetMeta. Used by the
// SET_INFO FileRenameInformation handler to enforce MS-FSA §2.1.5.15.12 ("FileRenameInformation")
// "rename overwrite onto an open file" — once any H-lease on the destination
// has been broken to RW, the destination's open handle still blocks the
// overwrite and must surface as STATUS_ACCESS_DENIED.
//
// excludeFileID is the rename's own SMB FileID (the source handle). It is
// excluded from the conflict check so a self-rename via the only handle on
// targetMeta is allowed (degenerate case; matches Samba behavior).

func (h *Handler) hasOpenHandleOnFile(targetMeta metadata.FileHandle, excludeFileID [16]byte) bool {
	if len(targetMeta) == 0 {
		return false
	}
	conflict := false
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == excludeFileID {
			return true
		}
		otherHandle := other.GetMetadataHandle()
		if len(otherHandle) == 0 {
			return true
		}
		if !bytes.Equal(otherHandle, targetMeta) {
			return true
		}
		conflict = true
		return false
	})
	return conflict
}
