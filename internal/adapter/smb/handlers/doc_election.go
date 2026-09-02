package handlers

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// docDecision is the outcome of the delete-on-close last-handle election for
// one closing handle (MS-FSA 2.1.5.5 ("Server Requests Closing an Open"): the unlink happens when the LAST handle
// on the file closes).
type docDecision int

const (
	// docDecisionNone: the closing handle carries no delete-on-close.
	docDecisionNone docDecision = iota

	// docDecisionDeferred: handles that can still honour the delete-on-close
	// remain open. They now carry it, and the last of them unlinks.
	docDecisionDeferred

	// docDecisionDelete: no such handle remains. The caller performs the
	// unlink.
	docDecisionDelete
)

// docTarget names what the electing handle must unlink when the election
// returns docDecisionDelete. It is snapshotted inside the election so a rename
// landing afterwards cannot make the scan and the unlink disagree about which
// entry the delete-on-close was decided for.
type docTarget struct {
	// Name is the closing handle's own name at election time.
	Name OpenName

	// ParentHandle and FileName address the directory entry to remove. For a
	// stream handle carrying a base-file delete they address the base file,
	// not the stream — the stream is a child of the base.
	ParentHandle metadata.FileHandle
	FileName     string

	// IsBaseFile is set when the entry above is the base file behind a stream
	// handle rather than the closing handle's own name.
	IsBaseFile bool

	// DocSetterParentKey is the parent lease key recorded by whoever committed
	// the delete-on-close, snapshotted under the handle lock. The unlink
	// suppresses the parent dir lease only when it matches the closer's own
	// parent key; when they differ every parent dir lease breaks.
	DocSetterParentKey    [16]byte
	HasDocSetterParentKey bool
}

// electDeleteOnClose runs the delete-on-close last-handle election for a
// closing handle and returns what that handle must do about it.
//
// Both close paths — the explicit CLOSE handler and the LOGOFF /
// tree-disconnect / transport-drop teardown — decide whether they are the last
// handle on a file well before they remove themselves from the open-file
// table: CLOSE scans at step 8 and removes at step 10, and the teardown scans
// in its first pass and removes in its third. Two closers whose scans both land
// before either removal each see the other as a surviving handle, each defers
// the unlink to the other, and the file is never deleted — the client is told
// its delete succeeded and the file survives.
//
// Narrowing that span cannot fix it; the decision and the departure have to be
// one step. They are made so here by an explicit departure mark taken under
// docElectionMu together with the scan that reads it:
//
//   - Every closing handle marks itself as leaving, whether or not it carries a
//     delete-on-close of its own. A handle that has already run this election
//     cannot honour a delete-on-close propagated to it afterwards, so it must
//     stop counting as a surviving handle at exactly that moment.
//   - The scan ignores handles already marked, so the closers of one file are
//     totally ordered by the mutex and only the last one in sees no survivor.
//     Every earlier closer sees a later one and propagates the delete-on-close
//     to it, which the later closer reads inside its own election.
//
// The mark deliberately does NOT remove the handle from h.files. It stays
// visible to the CREATE delete-pending gate, the share-mode and oplock scans
// and the rename conflict re-scan until the caller's own removal step, so a
// CREATE arriving between this election and the unlink still answers
// STATUS_DELETE_PENDING rather than opening a file that is about to vanish.
//
// docElectionMu is a leaf: the election takes only per-OpenFile locks under it
// and performs no I/O, so it never nests inside renameScanMu, the LockManager,
// or the lease locks.
func (h *Handler) electDeleteOnClose(openFile *OpenFile) (docDecision, docTarget) {
	h.docElectionMu.Lock()
	defer h.docElectionMu.Unlock()

	openFile.docLeaving = true

	// Promote the per-handle InitialDeleteOnClose from CREATE
	// FILE_DELETE_ON_CLOSE to the shared committed flag, mirroring Samba
	// close.c::close_normal_file: if this handle requested initial DOC and
	// nobody else committed a shared DOC via SET_INFO disposition, set
	// DeletePending so the deletion fires — either here, or on the handle this
	// election propagates to. InitialDeleteOnClose must NOT block opens before
	// this point (smbtorture smb2.dirlease.{unlink_same,unlink_different}
	// _initial_and_close), which is why the promotion happens at CLOSE and not
	// at CREATE.
	//
	// The read of DeletePending happens inside the election, not before it, so
	// a closer that was propagated to by an earlier closer observes the flag
	// and takes its turn as the last handle.
	openFile.mu.Lock()
	if openFile.InitialDeleteOnClose && !openFile.DeletePending {
		openFile.DeletePending = true
	}
	deletePending := openFile.DeletePending
	baseFileDeletePending := openFile.BaseFileDeletePending
	baseFileParentHandle := openFile.BaseFileDeleteParentHandle
	baseFileName := openFile.BaseFileDeleteFileName
	docSetterKey := openFile.DeleteOnCloseParentKey
	hasDocSetterKey := openFile.HasDeleteOnCloseParentKey
	openFile.mu.Unlock()

	if !deletePending && !baseFileDeletePending {
		return docDecisionNone, docTarget{}
	}

	// One snapshot of the name: the delete must not mix the parent directory
	// from one rename with the file name from another.
	docName := openFile.Name()

	// Opens on the same metadata file that are not themselves leaving. The DOC
	// is propagated to them in the same pass: not being the last handle means
	// the actual delete fires when they close. The DOC-setter's parent key is
	// preserved so the eventual closer can compare it for dir-lease
	// suppression (test_unlink_different_* vs test_unlink_same_*).
	otherHandleExists := false
	if len(openFile.MetadataHandle) > 0 {
		h.files.Range(func(_, value any) bool {
			other := value.(*OpenFile)
			if other.FileID == openFile.FileID || other.docLeaving {
				return true
			}
			if !bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
				return true
			}
			otherHandleExists = true
			// Guard the write: concurrent QUERY_INFO/WRITE goroutines on the
			// same session may be reading these fields on `other`. The write
			// lands on the pointer the handle table already holds, so no
			// re-Store follows: re-Storing would resurrect a handle whose own
			// CLOSE removed it, leaving a delete-pending entry nothing ever
			// reaps and every later CREATE on the path answered DELETE_PENDING.
			other.mu.Lock()
			other.DeletePending = true
			other.DeleteOnCloseParentKey = docSetterKey
			other.HasDeleteOnCloseParentKey = hasDocSetterKey
			other.mu.Unlock()
			return true
		})
	}

	if otherHandleExists {
		logger.Debug("CLOSE: DOC propagated to other handles (not last)",
			"path", docName.Path)
		return docDecisionDeferred, docTarget{}
	}

	// For base-file DOC on a non-stream handle, open stream handles (ADS) on
	// the same base file also hold the deletion back. Per MS-FSA 2.1.5.5 ("Server Requests Closing an Open") /
	// MS-SMB2 3.3.5.10 the removal is deferred until all handles — including
	// stream handles — are closed. Marking them with BaseFileDeletePending
	// makes the CLOSE of the last stream trigger the base file removal
	// (smbtorture smb2.streams.delete).
	isBaseFile := !strings.Contains(docName.FileName, ":")
	if isBaseFile && !baseFileDeletePending {
		streamHandleExists := false
		h.rangeLiveStreamsOfBase(openFile.FileID, docName.ParentHandle, docName.FileName+":", func(other *OpenFile) bool {
			streamHandleExists = true
			// Guard the write: concurrent readers on the stream handle
			// (QUERY_INFO / open path via isFileOrBaseDeletePending) may be
			// reading these fields on `other`.
			other.mu.Lock()
			other.BaseFileDeletePending = true
			other.BaseFileDeleteParentHandle = docName.ParentHandle
			other.BaseFileDeleteFileName = docName.FileName
			other.DeleteOnCloseParentKey = docSetterKey
			other.HasDeleteOnCloseParentKey = hasDocSetterKey
			other.mu.Unlock()
			return true
		})
		if streamHandleExists {
			logger.Debug("CLOSE: base file DOC deferred to stream handles",
				"path", docName.Path)
			return docDecisionDeferred, docTarget{}
		}
	}

	// A stream handle carrying a base-file DOC waits for every other stream
	// handle carrying the same base-file delete.
	if baseFileDeletePending {
		siblingStreamExists := false
		h.rangeLiveStreamsOfBase(openFile.FileID, baseFileParentHandle, baseFileName+":", func(other *OpenFile) bool {
			if other.BaseFileDeletePending {
				siblingStreamExists = true
				return false
			}
			return true
		})
		if siblingStreamExists {
			logger.Debug("CLOSE: base file DOC deferred to remaining stream handles",
				"path", docName.Path)
			return docDecisionDeferred, docTarget{}
		}
	}

	target := docTarget{
		Name:                  docName,
		ParentHandle:          docName.ParentHandle,
		FileName:              docName.FileName,
		DocSetterParentKey:    docSetterKey,
		HasDocSetterParentKey: hasDocSetterKey,
	}
	if baseFileDeletePending {
		target.ParentHandle = baseFileParentHandle
		target.FileName = baseFileName
		target.IsBaseFile = true
	}
	return docDecisionDelete, target
}

// rangeLiveStreamsOfBase iterates over the open files that are streams of a
// base file, filtering out self (by FileID), pipes, handles already leaving
// via their own election, handles in a different parent directory, and names
// that don't start with basePrefix. The caller's fn receives each matching
// *OpenFile and returns true to continue or false to stop.
//
// Callers hold docElectionMu: the docLeaving reads below are only meaningful
// under it, and a stream handle already past its own election can no longer
// honour a base-file delete marked on it.
func (h *Handler) rangeLiveStreamsOfBase(selfFileID [16]byte, parentHandle metadata.FileHandle, basePrefix string, fn func(*OpenFile) bool) {
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == selfFileID || other.IsPipe || other.docLeaving {
			return true
		}
		otherName := other.Name()
		if !bytes.Equal(otherName.ParentHandle, parentHandle) {
			return true
		}
		if len(otherName.FileName) <= len(basePrefix) || !strings.EqualFold(otherName.FileName[:len(basePrefix)], basePrefix) {
			return true
		}
		return fn(other)
	})
}

// removeElectedTarget removes the directory entry a delete-on-close election
// resolved, together with everything that must accompany a removed entry:
// cascading the base file's ADS streams, purging its block-store payload, and
// restoring the parent directory's frozen timestamps. It reports whether the
// entry was actually removed and returns the removal error so the caller can
// surface or log it.
//
// A directory that still holds entries is not one of those errors. Per MS-FSA
// 2.1.5.5 phase 1 a close carrying FILE_DELETE_ON_CLOSE marks the link deleted
// only when the directory's entry list is empty; otherwise it leaves the
// directory in place and the close still completes with STATUS_SUCCESS. The
// refusal a client is meant to see comes from setting the delete disposition
// (MS-FSA 2.1.5.15.3), not from the close. Such a close reports removed=false
// with a nil error, so the caller neither fails the close nor announces a
// removal that did not happen.
//
// Both close paths go through it. They previously removed the entry themselves
// and drifted: the teardown never cascaded, so once it started removing the
// resolved target — a base file, for a stream handle carrying a base-file
// delete — the base entry went and its `name:stream` siblings stayed behind as
// orphans that the next enumeration of that name counted.
//
// Two things stay with the callers because they differ by close path rather
// than by what was removed:
//
//   - Lease breaks. CLOSE relies on the removal's own dir-change notification
//     to break the parent's leases and deliberately does not dispatch a second
//     one; the teardown dispatches both — the file's Handle leases and the
//     parent's — once this call reports the entry actually gone, because no
//     SET_INFO disposition preceded it.
//   - The SMB CHANGE_NOTIFY event. CLOSE emits one; the teardown never has,
//     and starting to emit one there is a change to the notify registry's
//     traffic rather than to this removal.
func (h *Handler) removeElectedTarget(
	ctx context.Context,
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	target docTarget,
	caller string,
) (removedIsDir, removed bool, err error) {
	metaSvc := h.Registry.GetMetadataService()

	// For a deferred base-file delete (reached via a stream handle), resolve
	// the actual target type — stream handles always have IsDirectory=false.
	//
	// Capture the delete target's metadata handle BEFORE removal so the
	// block-store payload purge can be routed afterwards (the handle encodes
	// share identity and stays valid). The PayloadID to purge comes from
	// RemoveFile's RETURN value, which is empty when content must survive (a
	// surviving hard link, or a recycle to trash) — purging the open handle's
	// PayloadID instead would destroy still-referenced content.
	isDeleteTargetDir := openFile.IsDirectory
	deleteTargetHandle := openFile.MetadataHandle
	if target.IsBaseFile {
		if targetFile, _, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, target.ParentHandle, target.FileName); lookupErr == nil && targetFile != nil {
			isDeleteTargetDir = targetFile.Type == metadata.FileTypeDirectory
			if encoded, encErr := metadata.EncodeFileHandle(targetFile); encErr == nil {
				deleteTargetHandle = encoded
			}
		}
	}

	var removedPayloadID metadata.PayloadID
	if isDeleteTargetDir {
		_, err = metaSvc.RemoveDirectory(authCtx, target.ParentHandle, target.FileName)
		var storeErr *metadata.StoreError
		if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrNotEmpty {
			logger.Debug(caller+": delete-on-close left a non-empty directory in place",
				"path", target.Name.Path,
				"deleteTarget", target.FileName)
			return true, false, nil
		}
	} else {
		var removedFile *metadata.File
		removedFile, _, err = metaSvc.RemoveFile(authCtx, target.ParentHandle, target.FileName)
		if removedFile != nil {
			removedPayloadID = removedFile.PayloadID
		}
	}
	if err != nil {
		logger.Debug(caller+": failed to delete",
			"path", target.Name.Path,
			"deleteTarget", target.FileName,
			"isDir", isDeleteTargetDir,
			"error", err)
		return isDeleteTargetDir, false, err
	}

	logger.Debug(caller+": deleted",
		"path", target.Name.Path,
		"deleteTarget", target.FileName,
		"isDir", isDeleteTargetDir,
		"isBaseFileDelete", target.IsBaseFile)

	// Per MS-FSA 2.1.5.1.2.1 ("Algorithm to Check Access to an Existing File") deleting a file deletes all its streams. The
	// streams are sibling entries named `base:stream`, so removing the base
	// entry alone leaves them behind.
	if !isDeleteTargetDir && !strings.Contains(target.FileName, ":") {
		cascadeOF := openFile
		if target.IsBaseFile {
			cascadeOF = (&OpenFile{}).WithName(OpenName{
				Path:         target.Name.Path,
				FileName:     target.FileName,
				ParentHandle: target.ParentHandle,
			})
		}
		h.cascadeDeleteADSStreams(authCtx, metaSvc, cascadeOF)
	}

	h.purgeBlockStorePayload(ctx, deleteTargetHandle, removedPayloadID, target.Name.Path, caller)
	h.restoreParentDirFrozenTimestamps(authCtx, target.ParentHandle)

	// The name is free again, so the directory's delete-pending marker must go
	// with it: a CHANGE_NOTIFY on a directory later created under the same name
	// would otherwise be answered STATUS_DELETE_PENDING forever. This sits here
	// rather than in the two callers because it is unconditionally right for
	// both — it retires state the removal has just made false, not a
	// notification whose shape differs by close path.
	if isDeleteTargetDir && !target.IsBaseFile && h.NotifyRegistry != nil {
		h.NotifyRegistry.ClearDeletePendingMark(openFile.ShareName, target.Name.Path)
	}

	return isDeleteTargetDir, true, nil
}
