package metadata

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This handles:
//   - Special names: "." (current dir), ".." (parent dir)
//   - Permission checking (execute on directory for search)
//   - Name resolution in directory
func (s *Service) Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check execute/search permission on directory.
	//
	// Skipped when the caller holds "Bypass traverse checking"
	// (Windows SeChangeNotifyPrivilege, MS-DTYP §2.5.3.2 + MS-FSA
	// §2.1.5.1.1). Every SMB session sets ctx.BypassTraverseChecking so
	// that a parent directory whose DACL omits FILE_TRAVERSE does not
	// block resolution of a child whose own DACL grants the request. NFS
	// callers leave the flag false and continue to enforce POSIX execute
	// semantics on each path component.
	if !ctx.BypassTraverseChecking {
		if err := s.checkExecutePermission(ctx, dirHandle); err != nil {
			return nil, err
		}
	}

	// Handle special names
	if name == "." {
		return dir, nil
	}

	if name == ".." {
		parentHandle, err := store.GetParent(ctx.Context, dirHandle)
		if err != nil {
			// No parent means this is root, return self
			return dir, nil
		}
		return store.GetFile(ctx.Context, parentHandle)
	}

	// Regular name lookup
	childHandle, err := store.GetChild(ctx.Context, dirHandle, name)
	if err != nil {
		return nil, err
	}

	return store.GetFile(ctx.Context, childHandle)
}

// equalFoldName reports whether two directory entry names match under SMB's
// case-insensitive rules. It is strings.EqualFold for well-formed UTF-8, but
// falls back to byte-exact comparison when either name is not valid UTF-8.
//
// SMB filenames are arbitrary 16-bit code-unit sequences and may contain
// unpaired surrogates ([MS-SMB2] 2.1); DittoFS preserves those as WTF-8 (e.g.
// the lone surrogates {U+D800} and {U+DC00} the smb2.charset.Testing suite
// CREATEs as two distinct names). strings.EqualFold decodes every invalid byte
// sequence to U+FFFD before folding, so it would report those two distinct
// names as equal and make the second CREATE collide with the first. Requiring
// both sides to be valid UTF-8 before folding keeps malformed names distinct
// while leaving ordinary case-insensitive matching unchanged.
func equalFoldName(a, b string) bool {
	if !utf8.ValidString(a) || !utf8.ValidString(b) {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// LookupCaseInsensitive resolves a name within a directory like Lookup, but
// falls back to a case-insensitive scan of the directory's children when the
// exact-case lookup returns ErrNotFound. It is intended for SMB callers, which
// treat NTFS-style paths as case-insensitive while DittoFS stores names with
// their original case on disk.
//
// Returns:
//   - (file, matchedName, nil) on success — matchedName is the on-disk name
//     (case-preserved) that satisfied the match.
//   - (nil, "", nil) when no entry matches.
//   - (nil, "", err) for any non-NotFound error (NotDirectory, permission,
//     transport, …); callers should map this to the appropriate SMB status.
//
// Special names "." and ".." short-circuit to the exact-case Lookup path.
//
// Note: NFS callers must keep using Lookup directly — POSIX paths are
// case-sensitive.
func (s *Service) LookupCaseInsensitive(ctx *AuthContext, dirHandle FileHandle, name string) (*File, string, error) {
	f, err := s.Lookup(ctx, dirHandle, name)
	if err == nil {
		return f, name, nil
	}
	if !IsNotFoundError(err) {
		return nil, "", err
	}
	if name == "" || name == "." || name == ".." {
		return nil, "", nil
	}

	store, storeErr := s.storeForHandle(dirHandle)
	if storeErr != nil {
		return nil, "", storeErr
	}

	cursor := ""
	for {
		entries, nextCursor, listErr := store.ListChildren(ctx.Context, dirHandle, cursor, 500)
		if listErr != nil {
			if IsNotFoundError(listErr) {
				return nil, "", nil
			}
			return nil, "", listErr
		}
		for _, entry := range entries {
			if equalFoldName(entry.Name, name) {
				match, matchErr := s.Lookup(ctx, dirHandle, entry.Name)
				if matchErr != nil {
					if IsNotFoundError(matchErr) {
						continue
					}
					return nil, "", matchErr
				}
				return match, entry.Name, nil
			}
		}
		if nextCursor == "" {
			return nil, "", nil
		}
		cursor = nextCursor
	}
}

// ReadSymlink reads the target path of a symbolic link.
func (s *Service) ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return "", nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return "", nil, err
	}

	// Verify it's a symlink
	if file.Type != FileTypeSymlink {
		return "", nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "not a symbolic link",
			Path:    file.Path,
		}
	}

	return file.LinkTarget, file, nil
}

// SetFileAttributes updates file attributes with validation and access control.
//
// Only attributes with non-nil pointers in attrs are modified. The returned
// DirWcc carries the target file's own pre/post attributes for protocol WCC
// data: Before is the state the operation transitioned from (the attributes the
// mutation observed), After is the resulting state (H9). For SETATTR the WCC
// subject is the file itself, not a parent directory.
func (s *Service) SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) (*DirWcc, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Capture pre-op attributes: a copy of the file as observed by this
	// operation, before any mutation is applied below. This is exactly the
	// state WCC "before" must describe.
	wcc := &DirWcc{Before: CopyFileAttr(&file.FileAttr)}

	// Check permissions based on what's being changed
	identity := ctx.Identity
	isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0

	noOwnershipAttrs := attrs.Mode == nil && attrs.UID == nil && attrs.GID == nil

	// POSIX: For utimensat() with UTIME_NOW, write permission is sufficient.
	onlySettingTimesToNow := noOwnershipAttrs && attrs.Size == nil &&
		(attrs.AtimeNow || attrs.MtimeNow)

	// POSIX: truncate() requires write access, not ownership.
	onlySettingSize := noOwnershipAttrs && attrs.Size != nil &&
		!attrs.AtimeNow && !attrs.MtimeNow

	// POSIX: When a non-owner writes to a file with SUID/SGID bits set, those
	// bits must be cleared. The Linux NFS client implements this via
	// file_remove_privs() which sends SETATTR(mode = current & ~06000) before
	// the WRITE. We must allow this SETATTR even from non-owners who have write
	// permission, as long as the ONLY mode change is clearing SUID/SGID bits.
	onlyClearingSuidSgid := false
	if attrs.Mode != nil && attrs.UID == nil && attrs.GID == nil && attrs.Size == nil {
		clearedMode := file.Mode & ^uint32(0o6000)
		if *attrs.Mode == clearedMode && file.Mode&0o6000 != 0 {
			onlyClearingSuidSgid = true
		}
	}

	// Both timestamp-now and truncate-only operations allow write permission
	// as an alternative to ownership (POSIX semantics).
	writePermSufficient := onlySettingTimesToNow || onlySettingSize || onlyClearingSuidSgid

	if writePermSufficient && !isOwner && !isRoot {
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return nil, err
		}
	} else if !isOwner && !isRoot {
		return nil, &StoreError{
			Code:    ErrPermissionDenied,
			Message: "operation not permitted",
			Path:    file.Path,
		}
	}

	now := time.Now()
	modified := false

	// Apply requested changes
	if attrs.Mode != nil {
		newMode := *attrs.Mode

		// POSIX: Non-root users cannot set SUID/SGID bits arbitrarily
		// - SUID (04000) can only be set by owner or root
		// - SGID (02000) can only be set by owner who is member of file's group, or root
		if !isRoot {
			// Strip SUID bit if caller doesn't own the file
			if newMode&0o4000 != 0 && !isOwner {
				newMode &= ^uint32(0o4000)
			}
			// Strip SGID bit if caller is not a member of the file's group
			if newMode&0o2000 != 0 {
				// For SGID, caller must be owner AND member of file's group
				if !isOwner || !identity.HasGID(file.GID) {
					newMode &= ^uint32(0o2000)
				}
			}
		}

		file.Mode = newMode

		// RFC 7530 Section 6.4.1: chmod adjusts OWNER@/GROUP@/EVERYONE@ ACEs
		// to match the new mode bits when an ACL is present.
		if file.ACL != nil {
			file.ACL = acl.AdjustACLForMode(file.ACL, newMode)
		}

		modified = true
	}

	// Atomic mode-bit masks: applied within the same store read-modify-write
	// as the rest of SetFileAttributes so concurrent bit flips (e.g. SET_SPARSE
	// racing SET_COMPRESSION) cannot clobber each other with a stale snapshot.
	//
	// These fields exist solely for FSCTL-managed DOS attribute bits (high word).
	// They are applied AFTER the POSIX SUID/SGID stripping above, so they must
	// not be allowed to carry permission/setid/sticky bits — otherwise a caller
	// could set e.g. SGID via ModeOrMask and bypass that validation. Whitelist
	// the masks down to the known DOS attribute bits before applying.
	if attrs.ModeOrMask != nil {
		file.Mode |= *attrs.ModeOrMask & dosAttributeModeBits
		modified = true
	}
	if attrs.ModeAndNotMask != nil {
		file.Mode &^= *attrs.ModeAndNotMask & dosAttributeModeBits
		modified = true
	}

	// Track if ownership changed (for SUID/SGID clearing)
	ownershipChanged := false

	if attrs.UID != nil {
		// Only root can change owner to a different UID
		// Owner can set UID to their own UID (no-op for chown(file, same_uid, new_gid))
		if *attrs.UID != file.UID && !isRoot {
			return nil, &StoreError{
				Code:    ErrPermissionDenied,
				Message: "only root can change owner",
				Path:    file.Path,
			}
		}
		if *attrs.UID != file.UID {
			logger.Debug("SetFileAttributes: UID changed",
				"path", file.Path,
				"old_uid", file.UID,
				"new_uid", *attrs.UID)
			file.UID = *attrs.UID
			modified = true
			ownershipChanged = true
		}
	}

	if attrs.GID != nil {
		// Root can change to any group
		// Owner can change to their own supplementary groups
		if !isRoot {
			isPrimaryGroup := identity.GID != nil && *identity.GID == *attrs.GID
			if !isPrimaryGroup && !identity.HasGID(*attrs.GID) {
				return nil, &StoreError{
					Code:    ErrPermissionDenied,
					Message: "not a member of target group",
					Path:    file.Path,
				}
			}
		}
		if *attrs.GID != file.GID {
			file.GID = *attrs.GID
			modified = true
			ownershipChanged = true
		}
	}

	// POSIX: Clear SUID/SGID bits when ownership changes on non-directory files
	// This is a security measure to prevent privilege escalation.
	// For directories, SGID has different meaning (inherit group) and should NOT be cleared.
	// For symlinks, permissions aren't used (target permissions matter), so we skip them.
	// Note: This clears SUID/SGID regardless of who does the chown (including root),
	// matching Linux kernel behavior.
	if ownershipChanged && file.Type != FileTypeDirectory && file.Type != FileTypeSymlink {
		// Clear SUID (04000) and SGID (02000) bits
		file.Mode &= ^uint32(0o6000)
	}

	if attrs.Size != nil {
		// Size change requires write permission
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return nil, err
		}
		// A size-down truncate must trim the content-addressed block list to
		// the new size, otherwise stale-tail refs past EOF survive in
		// FileAttr.Blocks: the snapshot manifest (built from FileAttr.Blocks)
		// over-references them, the block-store GC holds them, and a restore
		// would emit a file longer than the current size (#817). Refs
		// straddling the new EOF are kept — the tail bytes past EOF are
		// ignored on read. Block refcounts are reconciled by the block-store
		// GC, the same as RemoveFile, which drops a file's entire block list
		// without inline decrements.
		if *attrs.Size < file.Size && len(file.Blocks) > 0 {
			file.Blocks = block.PruneBlockRefsToSize(file.Blocks, *attrs.Size)
			// Keep ObjectID (the Merkle root over Blocks) consistent with the
			// trimmed list, or zero it when no blocks remain so the file reads
			// as "never quiesced" instead of carrying a stale dedup pointer.
			if !file.ObjectID.IsZero() {
				if len(file.Blocks) == 0 {
					file.ObjectID = block.ObjectID{}
				} else {
					file.ObjectID = block.ComputeObjectID(file.Blocks)
				}
			}
		}
		file.Size = *attrs.Size
		modified = true

		// POSIX: truncate updates mtime and ctime when size changes
		// The server must do this even if the client doesn't send TIME_MODIFY_SET,
		// because POSIX requires it and NFS clients may rely on server-side updates.
		file.Mtime = now
		file.Ctime = now

		// POSIX: Clear SUID/SGID bits on truncate for non-root users (like write)
		if file.Type == FileTypeRegular && !isRoot {
			file.Mode &= ^uint32(0o6000)
		}
	}

	if attrs.Atime != nil {
		file.Atime = *attrs.Atime
		modified = true
	}

	if attrs.Mtime != nil {
		file.Mtime = *attrs.Mtime
		modified = true
	}

	if attrs.CreationTime != nil {
		file.CreationTime = *attrs.CreationTime
		modified = true
	}

	if attrs.Ctime != nil {
		file.Ctime = *attrs.Ctime
		modified = true
	}

	// Handle ACL setting
	if attrs.ACL != nil {
		if err := acl.ValidateACL(attrs.ACL); err != nil {
			return nil, &StoreError{
				Code:    ErrInvalidArgument,
				Message: fmt.Sprintf("invalid ACL: %v", err),
				Path:    file.Path,
			}
		}
		file.ACL = attrs.ACL
		modified = true
	}

	if attrs.Hidden != nil {
		file.Hidden = *attrs.Hidden
		modified = true
	}

	// Apply extended-attribute set/delete mutations. EA writes require write
	// access to the file (the SMB layer additionally gates on FILE_WRITE_EA at
	// CREATE time); owner/root already passed the ownership gate above, a
	// non-owner must hold write permission.
	if len(attrs.EAMutations) > 0 {
		if !isOwner && !isRoot {
			if err := s.checkWritePermission(ctx, handle); err != nil {
				return nil, err
			}
		}
		file.ApplyEAMutations(attrs.EAMutations)
		modified = true
	}

	// Auto-update ctime when attributes change, unless explicitly set
	if modified {
		if attrs.Ctime == nil {
			file.Ctime = now
		}
		if err := store.PutFile(ctx.Context, file); err != nil {
			return nil, err
		}

		// Invalidate cached file in pending writes to ensure subsequent
		// writes use fresh attributes (e.g., mode changes for SUID/SGID clearing)
		s.pendingWrites.InvalidateCache(handle)
	}

	// Post-op attributes reflect the resulting file state (mutated in place
	// above; equals Before when nothing changed).
	wcc.After = CopyFileAttr(&file.FileAttr)
	return wcc, nil
}

// Move moves or renames a file or directory atomically.
func (s *Service) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) (*RenameWcc, error) {
	store, err := s.storeForHandle(fromDir)
	if err != nil {
		return nil, err
	}

	// Validate names
	if err := ValidateName(fromName); err != nil {
		return nil, err
	}
	if err := ValidateName(toName); err != nil {
		return nil, err
	}

	// Same directory and same name - no-op (POSIX rename semantics)
	if string(fromDir) == string(toDir) && fromName == toName {
		return nil, nil
	}

	// Get source directory
	srcDir, err := store.GetFile(ctx.Context, fromDir)
	if err != nil {
		return nil, err
	}
	if srcDir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "source parent is not a directory",
		}
	}

	// Get destination directory
	dstDir, err := store.GetFile(ctx.Context, toDir)
	if err != nil {
		return nil, err
	}
	if dstDir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Validate destination path length (POSIX PATH_MAX compliance)
	destPath := buildPath(dstDir.Path, toName)
	if err := ValidatePath(destPath); err != nil {
		return nil, err
	}

	// Check write permission on both directories
	if err := s.checkWritePermission(ctx, fromDir); err != nil {
		return nil, err
	}
	if err := s.checkWritePermission(ctx, toDir); err != nil {
		return nil, err
	}

	// Get source file
	srcHandle, err := store.GetChild(ctx.Context, fromDir, fromName)
	if err != nil {
		return nil, err
	}
	srcFile, err := store.GetFile(ctx.Context, srcHandle)
	if err != nil {
		return nil, err
	}

	// Check sticky bit on source directory
	if err := CheckStickyBitRestriction(ctx, &srcDir.FileAttr, &srcFile.FileAttr); err != nil {
		return nil, err
	}

	// POSIX: When moving a directory to a different parent from a sticky directory,
	// the caller must own the directory being moved (not just the sticky directory).
	// This is because the ".." link inside the moved directory must be updated,
	// which requires ownership of the directory being moved.
	// See rename(2) man page: "If oldpath refers to a directory, then ... if the
	// sticky bit is set on the directory containing oldpath ... the process must
	// own the file being renamed."
	if srcFile.Type == FileTypeDirectory && string(fromDir) != string(toDir) && srcDir.Mode&ModeSticky != 0 {
		callerUID := ^uint32(0) // Invalid UID
		if ctx.Identity != nil && ctx.Identity.UID != nil {
			callerUID = *ctx.Identity.UID
		}
		// Root can always move directories
		if callerUID != 0 && srcFile.UID != callerUID {
			logger.Debug("Move: cross-directory move denied by sticky bit",
				"reason", "caller does not own directory being moved",
				"src_file_uid", srcFile.UID,
				"caller_uid", callerUID)
			return nil, &StoreError{
				Code:    ErrAccessDenied,
				Message: "sticky bit set: cannot move directory you don't own to different parent",
			}
		}
	}

	// Check if destination exists and gather info before transaction
	var dstHandle FileHandle
	var dstFile *File
	dstHandle, err = store.GetChild(ctx.Context, toDir, toName)
	if err == nil {
		// Destination exists - check compatibility
		dstFile, err = store.GetFile(ctx.Context, dstHandle)
		if err != nil {
			return nil, err
		}

		// Check sticky bit on destination directory
		if err := CheckStickyBitRestriction(ctx, &dstDir.FileAttr, &dstFile.FileAttr); err != nil {
			return nil, err
		}

		// Type compatibility checks
		if srcFile.Type == FileTypeDirectory {
			if dstFile.Type != FileTypeDirectory {
				return nil, &StoreError{
					Code:    ErrNotDirectory,
					Message: "cannot overwrite non-directory with directory",
				}
			}
			// Check if destination directory is empty
			entries, _, err := store.ListChildren(ctx.Context, dstHandle, "", 1)
			if err == nil && len(entries) > 0 {
				return nil, &StoreError{
					Code:    ErrNotEmpty,
					Message: "destination directory not empty",
				}
			}
		} else {
			if dstFile.Type == FileTypeDirectory {
				return nil, &StoreError{
					Code:    ErrIsDirectory,
					Message: "cannot overwrite directory with non-directory",
				}
			}

			// Replace-overwrite: the destination file genuinely exists and is
			// about to be clobbered by the rename below. When the destination
			// share has trash enabled, recycle the victim first so it is
			// preserved instead of being silently destroyed. Reached ONLY in
			// the dest-exists file branch, so a non-clobbering rename never
			// recycles.
			//
			// No recursion: recycleNode performs its own s.Move of the victim
			// into #recycle, but that internal move's destination name is picked
			// by freeBinName to be guaranteed-absent, so its dest-exists branch
			// (and this recycle block) does not fire. inRecycle(victimRel) also
			// keeps us from recycling when the destination already lives in the
			// bin.
			if s.trashPolicy != nil {
				shareName := shareNameForHandle(toDir)
				if cfg, ok := s.trashPolicy.TrashConfigForShare(shareName); ok && cfg.Enabled {
					victimRel := strings.TrimPrefix(buildPath(dstDir.Path, toName), "/")
					if !inRecycle(victimRel) && !cfg.Excluded(toName) {
						// Discard the recycled node: Move only relocates the victim
						// into #recycle (the reaper frees its blocks later), so we
						// have no blocks to release here.
						if _, err := s.recycleNode(ctx, shareName, toDir, toName, victimRel); err != nil {
							return nil, err // never silently clobber
						}
						// The victim has moved into #recycle, so the destination
						// name is now free. Drop the cached dest-exists state so
						// the transaction below CREATEs toName fresh instead of
						// trying to remove an entry that no longer exists.
						dstFile = nil
						dstHandle = nil
					}
				}
			}
		}
	} else if !IsNotFoundError(err) {
		return nil, err
	}

	// rename carries the source/destination directory pre/post attributes for
	// WCC, captured inside the transaction below (H9). For an intra-directory
	// move FromDir and ToDir reference the same DirWcc.
	sameDir := string(fromDir) == string(toDir)
	rename := &RenameWcc{FromDir: &DirWcc{}}
	if sameDir {
		rename.ToDir = rename.FromDir
	} else {
		rename.ToDir = &DirWcc{}
	}

	// Execute all write operations in a single transaction for better performance.
	txErr := store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Re-read the source/destination directories inside the transaction so
		// the pre-op snapshots and the timestamp mutations derive from the same
		// committed state (After then monotonic w.r.t. Before).
		if txSrc, sErr := tx.GetFile(ctx.Context, fromDir); sErr == nil && txSrc != nil {
			srcDir = txSrc
		}
		rename.FromDir.Before = CopyFileAttr(&srcDir.FileAttr)
		if !sameDir {
			if txDst, dErr := tx.GetFile(ctx.Context, toDir); dErr == nil && txDst != nil {
				dstDir = txDst
			}
			rename.ToDir.Before = CopyFileAttr(&dstDir.FileAttr)
		}

		// Handle destination removal if it exists
		if dstFile != nil {
			// Remove destination
			if dstFile.Type == FileTypeDirectory {
				if err := tx.DeleteFile(ctx.Context, dstHandle); err != nil {
					return err
				}
			} else {
				// For files, decrement link count or set to 0
				// POSIX: ctime must be updated when link count changes.
				// The read is tx-critical: a failed GetLinkCount must roll the
				// rename back, not fall through with count 0 and commit a wrong
				// link count.
				linkCount, err := tx.GetLinkCount(ctx.Context, dstHandle)
				if err != nil {
					return err
				}
				now := time.Now()
				newCount := uint32(0)
				if linkCount > 1 {
					newCount = linkCount - 1
				}
				if err := tx.SetLinkCount(ctx.Context, dstHandle, newCount); err != nil {
					return err
				}
				// Update ctime on the file being unlinked (affects remaining hard links)
				dstFile.Ctime = now
				if err := tx.PutFile(ctx.Context, dstFile); err != nil {
					return err
				}
			}

			// Remove destination from children
			if err := tx.DeleteChild(ctx.Context, toDir, toName); err != nil {
				return err
			}
		}

		// Remove source from old parent
		if err := tx.DeleteChild(ctx.Context, fromDir, fromName); err != nil {
			return err
		}

		// Add source to new parent
		if err := tx.SetChild(ctx.Context, toDir, toName, srcHandle); err != nil {
			return err
		}

		// Update parent reference if directories are different. These are
		// tx-critical: a failed SetParent/SetLinkCount leaves the entry
		// relinked but the parent pointer or directory nlink wrong, which a
		// dir move can skew permanently. Return the error so the whole rename
		// rolls back atomically.
		if string(fromDir) != string(toDir) {
			if err := tx.SetParent(ctx.Context, srcHandle, toDir); err != nil {
				return err
			}

			// Update link counts for directory moves
			if srcFile.Type == FileTypeDirectory {
				// Decrement source parent's link count. The read is tx-critical
				// (a failed GetLinkCount must abort the rename, not silently
				// skip the parent-nlink update).
				srcLinkCount, err := tx.GetLinkCount(ctx.Context, fromDir)
				if err != nil {
					return err
				}
				if srcLinkCount > 0 {
					if err := tx.SetLinkCount(ctx.Context, fromDir, srcLinkCount-1); err != nil {
						return err
					}
				}
				// Increment destination parent's link count.
				dstLinkCount, err := tx.GetLinkCount(ctx.Context, toDir)
				if err != nil {
					return err
				}
				if err := tx.SetLinkCount(ctx.Context, toDir, dstLinkCount+1); err != nil {
					return err
				}
			}
		}

		// Update path and timestamps. PutFile(srcFile) is tx-critical: a stale
		// File.Path breaks the Path-keyed postgres snapshot/restore and the
		// descendant-path rewrite below. Return the error so a failure rolls
		// the whole rename back rather than committing a relinked entry with
		// the old Path.
		now := time.Now()
		oldPath := srcFile.Path
		srcFile.Path = destPath
		srcFile.Ctime = now
		if err := tx.PutFile(ctx.Context, srcFile); err != nil {
			return err
		}

		// For directory renames, recursively update all descendants' paths.
		// Propagate the error: a partial descendant rewrite leaves stale
		// child Paths that diverge from the parent, corrupting the Path-keyed
		// postgres namespace. Roll the rename back instead.
		if srcFile.Type == FileTypeDirectory {
			if err := s.updateDescendantPaths(ctx.Context, tx, srcHandle, oldPath, destPath); err != nil {
				return err
			}
		}

		srcDir.Mtime = now
		srcDir.Ctime = now
		rename.FromDir.After = CopyFileAttr(&srcDir.FileAttr)
		if err := tx.PutFile(ctx.Context, srcDir); err != nil {
			return err
		}

		if !sameDir {
			dstDir.Mtime = now
			dstDir.Ctime = now
			rename.ToDir.After = CopyFileAttr(&dstDir.FileAttr)
			if err := tx.PutFile(ctx.Context, dstDir); err != nil {
				return err
			}
		}

		return nil
	})

	if txErr != nil {
		return nil, txErr
	}

	// Notify directory change after successful move
	s.notifyDirChange(shareNameForHandle(fromDir), fromDir, lock.DirChangeRenameEntry, ctx)
	if string(fromDir) != string(toDir) {
		// Cross-directory move: derive share from toDir in case it differs
		s.notifyDirChange(shareNameForHandle(toDir), toDir, lock.DirChangeAddEntry, ctx)
	}

	return rename, nil
}

// updateDescendantPaths recursively updates the Path field of all descendants
// of a renamed directory. Uses iterative (queue-based) traversal to avoid
// stack overflow on deep trees.
func (s *Service) updateDescendantPaths(ctx context.Context, tx Transaction, dirHandle FileHandle, oldPrefix, newPrefix string) error {
	queue := []FileHandle{dirHandle}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		cursor := ""
		for {
			entries, nextCursor, err := tx.ListChildren(ctx, current, cursor, 100)
			if err != nil {
				return fmt.Errorf("list children for path update: %w", err)
			}

			for _, entry := range entries {
				child, err := tx.GetFile(ctx, entry.Handle)
				if err != nil {
					logger.Debug("updateDescendantPaths: skip unreadable child",
						"name", entry.Name, "error", err)
					continue
				}

				// Replace old path prefix with new prefix
				if strings.HasPrefix(child.Path, oldPrefix) {
					child.Path = newPrefix + child.Path[len(oldPrefix):]
					_ = tx.PutFile(ctx, child)
				}

				// Enqueue subdirectories for recursive traversal
				if child.Type == FileTypeDirectory {
					queue = append(queue, entry.Handle)
				}
			}

			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}
	}

	return nil
}

// MarkFileAsOrphaned sets a file's link count to 0, marking it as orphaned.
//
// This is used by NFS handlers for "silly rename" behavior.
func (s *Service) MarkFileAsOrphaned(ctx *AuthContext, handle FileHandle) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Only mark regular files as orphaned (directories don't have silly rename)
	if file.Type == FileTypeDirectory {
		return nil
	}

	// Set link count to 0
	if err := store.SetLinkCount(ctx.Context, handle, 0); err != nil {
		return err
	}

	// Update file's nlink and ctime
	now := time.Now()
	file.Nlink = 0
	file.Ctime = now
	return store.PutFile(ctx.Context, file)
}
