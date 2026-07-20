package metadata

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// RemoveFile removes a file from its parent directory.
//
// This handles:
//   - Input validation
//   - Permission checking via checkDeletePermission: ctx.HasDeleteAccess
//     (Windows DELETE semantics — authorized upstream, MS-FSA 2.1.5.4) or
//     WRITE on parent (POSIX unlink(2))
//   - Sticky bit enforcement
//   - Hard link management (decrement or set nlink=0)
//   - Parent timestamp updates
//
// Important: Does NOT delete the file's content data.
// The returned File includes PayloadID for caller to coordinate content deletion.
// PayloadID is empty if other hard links still reference the content.
//
// POSIX Compliance:
//   - When last link is removed, nlink is set to 0 (not deleted)
//   - This allows fstat() on open file descriptors to return nlink=0
func (s *Service) RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, *DirWcc, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return nil, nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Get child handle
	fileHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		return nil, nil, err
	}

	// Verify it's not a directory
	if file.Type == FileTypeDirectory {
		return nil, nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
			Path:    name,
		}
	}

	// Check delete permission: WRITE on parent (POSIX) or owner-of-file (Windows DELETE).
	if err := s.checkDeletePermission(ctx, parentHandle, file); err != nil {
		return nil, nil, err
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &file.FileAttr); err != nil {
		return nil, nil, err
	}

	// Recycle instead of destroying when the share has trash enabled. Deletes
	// already inside #recycle, and names matching an exclude glob, fall through
	// to the permanent delete below.
	if s.trashPolicy != nil {
		shareName := shareNameForHandle(parentHandle)
		if cfg, ok := s.trashPolicy.TrashConfigForShare(shareName); ok && cfg.Enabled {
			origRel := strings.TrimPrefix(buildPath(parent.Path, name), "/")
			if !inRecycle(origRel) && !cfg.Excluded(name) {
				// Recycle relocates via Move (a separate metadata op). Bracket
				// it with best-effort pre/post parent reads for WCC; this path
				// is rare and not perf-critical.
				wccBefore := CopyFileAttr(&parent.FileAttr)
				recycled, rErr := s.recycleNode(ctx, shareName, parentHandle, name, origRel)
				if rErr != nil {
					return nil, nil, rErr
				}
				wcc := &DirWcc{Before: wccBefore}
				if after, aErr := store.GetFile(ctx.Context, parentHandle); aErr == nil {
					wcc.After = CopyFileAttr(&after.FileAttr)
				}
				// Recycling still removes the name from its parent, so break the
				// parent's directory leases exactly as the permanent-delete path
				// below does — otherwise a caching client keeps serving a stale
				// listing that still shows the recycled entry. RemoveDirectory's
				// recycle branch notifies for the same reason.
				s.notifyDirChange(shareName, parentHandle, lock.DirChangeRemoveEntry, ctx)
				return recycled, wcc, nil
			}
		}
	}

	// Prepare return value
	returnFile := &File{
		ID:        file.ID,
		ShareName: file.ShareName,
		Path:      file.Path,
		FileAttr:  file.FileAttr,
	}

	// wcc captures the parent directory attributes before and after the
	// mutation. Per #1573 the child-removing transaction no longer touches the
	// parent inode, so WCC.Before is captured here (before the transaction) and
	// WCC.After is synthesized after commit rather than both being read inside
	// the txn.
	wcc := &DirWcc{}

	// Overlay any coalesced parent-directory timestamps onto the pre-op snapshot
	// so WCC.Before reflects what readers currently observe, then capture it
	// before the transaction: the remove no longer reads or writes the parent
	// inode, so there is nothing to re-snapshot inside it (#1573).
	s.mergeDirTimes(parentHandle, &parent.FileAttr)
	wcc.Before = CopyFileAttr(&parent.FileAttr)
	now := time.Now()

	// lastLink records whether this unlink removed the final name for the inode.
	// When it did, any buffered WRITE state for the (still-open) file must be
	// discarded so a later flush cannot resurrect its size/mtime on the removed
	// inode (#1753). A hard-link decrement leaves the file — and its legitimate
	// buffered size — intact, so it is left untouched.
	var lastLink bool

	// Serialize the remove + discard against a concurrent
	// FlushPendingWriteForFile (e.g. an in-flight COMMIT on the same file):
	// hold the per-handle flush lock across both so a popped stale MaxSize
	// cannot be re-applied to the inode after we drop it (#1753).
	flushMu := s.pendingWrites.GetFlushLock(fileHandle)
	flushMu.Lock()
	defer flushMu.Unlock()

	// Execute the child-removing writes in a single transaction. Like create,
	// this transaction does NOT touch the parent inode — the parent timestamp
	// bump that used to serialize concurrent same-dir removes on a BadgerDB SSI
	// hot key is coalesced after commit instead (#1573).
	//
	// Relaxed durability (#1573 Wall 1): these writes are pure namespace — the
	// child unlink and link-count — not paired with block data. A crash can lose
	// the removal (the entry reappears; the client re-unlinks), never corrupt data.
	err = withRelaxedTransaction(store, ctx.Context, func(tx Transaction) error {
		// Read the link count INSIDE the transaction so the read, the
		// branch decision, and the write are atomic. Reading it outside the
		// tx is a TOCTOU race with CreateHardLink: a concurrent link bump in
		// the window would leave us writing a stale (decremented) count and
		// dropping nlink to 0 while a valid link still references the file,
		// making the content eligible for deletion. Reading via tx.GetLinkCount
		// also registers the key for the backend's read-write conflict
		// detection so a racing writer triggers an automatic retry.
		linkCount, lcErr := tx.GetLinkCount(ctx.Context, fileHandle)
		if lcErr != nil {
			// If we can't get link count, assume 1.
			linkCount = 1
		}

		// Handle link count
		if linkCount > 1 {
			// File has other hard links, just decrement count
			// Empty PayloadID signals caller NOT to delete content
			returnFile.PayloadID = ""
			returnFile.Nlink = linkCount - 1
			returnFile.Ctime = now

			// Update file's link count and ctime
			if err := tx.SetLinkCount(ctx.Context, fileHandle, linkCount-1); err != nil {
				return err
			}

			// Update file's ctime
			file.Ctime = now
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		} else {
			// Last link - set nlink=0 but keep metadata for POSIX compliance
			lastLink = true
			returnFile.Nlink = 0
			returnFile.Ctime = now

			// Set link count to 0
			if err := tx.SetLinkCount(ctx.Context, fileHandle, 0); err != nil {
				return err
			}

			// Update file's ctime and nlink
			file.Ctime = now
			file.Nlink = 0
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		}

		// Remove from parent's children
		if err := tx.DeleteChild(ctx.Context, parentHandle, name); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	// The unlink is authoritative; drop any buffered WRITE state so a later
	// flush cannot resurrect size/mtime for the removed file (#1753).
	if lastLink {
		s.pendingWrites.PopPending(fileHandle)
	}

	// Coalesce the parent directory timestamp bump (Mtime/Ctime/Atime per
	// MS-FSA 2.1.4.4) out of the transaction, same as create (#1573).
	s.recordDirTimes(ctx.Context, parentHandle, now)
	parent.Mtime = now
	parent.Ctime = now
	parent.Atime = now
	wcc.After = CopyFileAttr(&parent.FileAttr)

	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeRemoveEntry, ctx)
	return returnFile, wcc, nil
}
