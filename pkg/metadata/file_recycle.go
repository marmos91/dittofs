package metadata

import (
	stderrors "errors"
	"strconv"
	"strings"
	"time"
)

// inRecycle reports whether a share-relative path lies inside (or is) the
// per-share recycle bin. Deletes of such paths are permanent: the bin cannot
// be recycled into itself, and emptying the bin must really destroy content.
func inRecycle(relPath string) bool {
	rel := strings.TrimPrefix(relPath, "/")
	return rel == RecycleDirName || strings.HasPrefix(rel, RecycleDirName+"/")
}

// principalOf derives a display principal for the DeletedBy stamp: the
// authenticated username when known, else the numeric UID, else "".
func principalOf(ctx *AuthContext) string {
	if ctx == nil || ctx.Identity == nil {
		return ""
	}
	if ctx.Identity.Username != "" {
		return ctx.Identity.Username
	}
	if ctx.Identity.UID != nil {
		return strconv.FormatUint(uint64(*ctx.Identity.UID), 10)
	}
	return ""
}

// recycleNode moves the child named name under parentHandle into the share's
// #recycle bin, recreating the original parent subtree beneath the bin and
// stamping recycle metadata (DeletedAt/OriginalPath/DeletedBy) on the victim.
// It returns a copy of the victim's pre-move *File with PayloadID cleared, so
// adapters skip block deletion (deferred reaping).
//
// origRel is the share-relative path the victim occupies before the move, with
// no leading slash (e.g. "documents/report.pdf"). On ANY failure it returns an
// error WITHOUT having destroyed the node — the caller must never fall back to
// a hard delete after a recycle attempt fails.
//
// The node is stamped BEFORE the move (at its original handle) so that a node
// living in the bin ALWAYS carries DeletedAt: if the move failed after a
// post-move stamp, the node would sit in the bin invisible to listing/reaping.
// Move is metadata-only and preserves FileAttr, so the moved node carries the
// stamp. If the move fails after stamping, the three fields are cleared on the
// source (best-effort) so a live file is not left marked-deleted.
func (s *MetadataService) recycleNode(ctx *AuthContext, shareName string, parentHandle FileHandle, name string, origRel string) (*File, error) {
	// 1. Capture the victim's pre-move identity/attrs so we can return a copy
	//    with PayloadID cleared even after the move relocates it.
	victimHandle, err := s.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}
	victim, err := s.GetFile(ctx.Context, victimHandle)
	if err != nil {
		return nil, err
	}

	// 2. Resolve the share root and ensure the #recycle bin exists.
	rootHandle, err := s.GetRootHandle(ctx.Context, shareName)
	if err != nil {
		return nil, err
	}
	binHandle, err := s.ensureChildDir(ctx, rootHandle, RecycleDirName, 0o700)
	if err != nil {
		return nil, err
	}

	// 3. Recreate the original parent subtree under the bin. For origRel
	//    "a/b/c" we ensure #recycle/a/b exists; the destination dir is that.
	//    A top-level victim ("report.pdf") has no parent dirs: destParent is the
	//    bin root and destName is the whole name.
	rel := strings.TrimPrefix(origRel, "/")
	parts := strings.Split(rel, "/")
	destName := parts[len(parts)-1]
	destParent := binHandle
	for _, dir := range parts[:len(parts)-1] {
		destParent, err = s.ensureChildDir(ctx, destParent, dir, 0o700)
		if err != nil {
			return nil, err
		}
	}

	// 4. Pick a free name in the bin so an existing recycled entry is never
	//    overwritten. Try the plain name first, then suffix with the delete
	//    instant in nanoseconds, then disambiguate with an incrementing counter.
	destName, err = s.freeBinName(ctx, destParent, destName)
	if err != nil {
		return nil, err
	}

	// 5. Stamp recycle metadata on the victim at its ORIGINAL handle, BEFORE
	//    moving. Move preserves FileAttr, so the stamp travels into the bin.
	now := time.Now().UTC()
	if err := s.stampRecycled(ctx, victimHandle, now, rel, principalOf(ctx)); err != nil {
		return nil, err
	}

	// 6. Move the (now-stamped) victim into the bin (atomic, metadata-only —
	//    a directory moves its whole subtree as one entry). If this fails, the
	//    source is still live but marked-deleted: clear the stamp best-effort so
	//    a live file is not left looking recycled, then surface the move error.
	if _, err := s.Move(ctx, parentHandle, name, destParent, destName); err != nil {
		if clearErr := s.clearRecycleStamp(ctx, victimHandle); clearErr != nil {
			return nil, &StoreError{
				Code:    ErrIOError,
				Message: "recycle move failed and stamp rollback also failed: move=" + err.Error() + " rollback=" + clearErr.Error(),
			}
		}
		return nil, &StoreError{
			Code:    ErrIOError,
			Message: "recycle move failed (stamp rolled back): " + err.Error(),
		}
	}

	// 7. Return a copy of the pre-move file with PayloadID cleared so the
	//    adapter skips block deletion.
	out := &File{
		ID:        victim.ID,
		ShareName: victim.ShareName,
		Path:      victim.Path,
		FileAttr:  victim.FileAttr,
	}
	out.PayloadID = ""
	return out, nil
}

// ensureChildDir resolves a child directory by name under parentHandle,
// creating it (mode perm) if absent. ErrAlreadyExists from a racing creator is
// treated as success followed by a re-lookup. Returns the child's handle.
func (s *MetadataService) ensureChildDir(ctx *AuthContext, parentHandle FileHandle, name string, perm uint32) (FileHandle, error) {
	if h, err := s.GetChild(ctx.Context, parentHandle, name); err == nil {
		// An existing child must actually be a directory. A regular file (or
		// other non-dir) squatting at the expected path would otherwise be
		// returned as a "bin" subtree, and the subsequent Move/CreateDirectory
		// against it would fail later with a confusing error. Surface a clear
		// ErrNotDirectory at the point of detection instead.
		existing, getErr := s.GetFile(ctx.Context, h)
		if getErr != nil {
			return nil, getErr
		}
		if existing.Type != FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrNotDirectory,
				Message: "recycle path component is not a directory: " + name,
				Path:    existing.Path,
			}
		}
		return h, nil
	}
	attr := &FileAttr{Type: FileTypeDirectory, Mode: perm}
	if _, _, err := s.CreateDirectory(ctx, parentHandle, name, attr); err != nil {
		var storeErr *StoreError
		if !stderrors.As(err, &storeErr) || storeErr.Code != ErrAlreadyExists {
			return nil, err
		}
	}
	return s.GetChild(ctx.Context, parentHandle, name)
}

// stampRecycled loads the node, sets the three trash fields, and persists it.
func (s *MetadataService) stampRecycled(ctx *AuthContext, handle FileHandle, deletedAt time.Time, origRel, deletedBy string) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}
	at := deletedAt
	file.DeletedAt = &at
	file.OriginalPath = origRel
	file.DeletedBy = deletedBy
	return store.PutFile(ctx.Context, file)
}

// clearRecycleStamp reverts the three trash fields on a node, used to roll back
// a pre-move stamp when the subsequent Move into the bin fails.
func (s *MetadataService) clearRecycleStamp(ctx *AuthContext, handle FileHandle) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}
	file.DeletedAt = nil
	file.OriginalPath = ""
	file.DeletedBy = ""
	return store.PutFile(ctx.Context, file)
}

// maxBinNameAttempts bounds the collision-resolution loop in freeBinName. A
// nanosecond-stamped name colliding even once is astronomically unlikely, so
// this is only a guard against pathological clocks / unbounded loops.
const maxBinNameAttempts = 1000

// freeBinName returns a child name under destParent that is guaranteed not to
// collide with an existing bin entry, so a Move into the bin never overwrites a
// previously recycled node. It tries name first, then " (<unixNanos>)", then
// " (<unixNanos>-<n>)" for increasing n. The returned name only disambiguates
// placement in the bin; OriginalPath stays the pre-collision original path.
func (s *MetadataService) freeBinName(ctx *AuthContext, destParent FileHandle, name string) (string, error) {
	candidate := name
	nanos := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	for attempt := 0; attempt < maxBinNameAttempts; attempt++ {
		_, err := s.GetChild(ctx.Context, destParent, candidate)
		if err != nil {
			if IsNotFoundError(err) {
				return candidate, nil
			}
			return "", err
		}
		// Name is taken: derive the next candidate.
		switch attempt {
		case 0:
			candidate = name + " (" + nanos + ")"
		default:
			candidate = name + " (" + nanos + "-" + strconv.Itoa(attempt) + ")"
		}
	}
	return "", &StoreError{
		Code:    ErrAlreadyExists,
		Message: "could not find a free recycle-bin name for " + name,
	}
}
