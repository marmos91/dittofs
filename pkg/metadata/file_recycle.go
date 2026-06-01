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
// stamping recycle metadata (DeletedAt/OriginalPath/DeletedBy) on the moved
// root. It returns a copy of the victim's pre-move *File with PayloadID
// cleared, so adapters skip block deletion (deferred reaping).
//
// origRel is the share-relative path the victim occupies before the move, with
// no leading slash (e.g. "documents/report.pdf"). On ANY failure it returns an
// error WITHOUT having destroyed the node — the caller must never fall back to
// a hard delete after a recycle attempt fails.
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
	rel := strings.TrimPrefix(origRel, "/")
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return nil, &StoreError{Code: ErrInvalidArgument, Message: "empty recycle path"}
	}
	destName := parts[len(parts)-1]
	destParent := binHandle
	for _, dir := range parts[:len(parts)-1] {
		destParent, err = s.ensureChildDir(ctx, destParent, dir, 0o700)
		if err != nil {
			return nil, err
		}
	}

	// 4. On a name collision in the bin, suffix " (<unixSeconds>)" Synology-style.
	now := time.Now().UTC()
	if _, gcErr := s.GetChild(ctx.Context, destParent, destName); gcErr == nil {
		destName = destName + " (" + strconv.FormatInt(now.Unix(), 10) + ")"
	}

	// 5. Move the victim (atomic, metadata-only — a directory moves its whole
	//    subtree as one entry).
	if err := s.Move(ctx, parentHandle, name, destParent, destName); err != nil {
		return nil, err
	}

	// 6. Stamp recycle metadata on the moved root node.
	movedHandle, err := s.GetChild(ctx.Context, destParent, destName)
	if err != nil {
		return nil, err
	}
	if err := s.stampRecycled(ctx, movedHandle, now, rel, principalOf(ctx)); err != nil {
		return nil, err
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
		return h, nil
	}
	attr := &FileAttr{Type: FileTypeDirectory, Mode: perm}
	if _, err := s.CreateDirectory(ctx, parentHandle, name, attr); err != nil {
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
