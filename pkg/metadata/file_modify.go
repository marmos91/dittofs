package metadata

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

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
	// Overlay coalesced directory timestamps so "." and the dir's own attrs
	// reflect not-yet-persisted create/remove bumps (#1573).
	s.mergeDirTimes(dirHandle, &dir.FileAttr)

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
	// §2.1.5.1 Phase 6 ("Location of file")). Every SMB session sets ctx.BypassTraverseChecking so
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
		// s.GetFile overlays coalesced dir timestamps if the parent is a
		// directory with pending create/remove bumps (#1573).
		return s.GetFile(ctx.Context, parentHandle)
	}

	// Regular name lookup
	childHandle, err := store.GetChild(ctx.Context, dirHandle, name)
	if err != nil {
		return nil, err
	}

	// s.GetFile overlays coalesced dir timestamps when the child is itself a
	// directory that has had entries created/removed in it (#1573).
	return s.GetFile(ctx.Context, childHandle)
}

// EqualFoldName reports whether two directory entry names match under SMB's
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
func EqualFoldName(a, b string) bool {
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
		entries, nextCursor, listErr := store.ListChildren(ctx.Context, dirHandle, cursor, 500, NamesOnly)
		if listErr != nil {
			if IsNotFoundError(listErr) {
				return nil, "", nil
			}
			return nil, "", listErr
		}
		for _, entry := range entries {
			if EqualFoldName(entry.Name, name) {
				// Fast path: stores populate entry.Handle (DirEntry.Handle is
				// MUST-populate). The initial exact-case s.Lookup above already
				// validated the directory and execute/traverse permission, so a
				// second s.Lookup would redundantly re-run GetFile(dir) + type
				// check + permission check + GetChild. Fetch the matched file
				// directly from its handle instead.
				if len(entry.Handle) != 0 {
					match, matchErr := store.GetFile(ctx.Context, entry.Handle)
					if matchErr != nil {
						if IsNotFoundError(matchErr) {
							continue
						}
						return nil, "", matchErr
					}
					return match, entry.Name, nil
				}
				// Fallback for stores that leave Handle empty: full Lookup.
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

// RestoreChangeTimeIfUnchanged writes want into the file's ChangeTime, but only
// while the stored value is still expected. It is the conditional half of the
// SMB rename ChangeTime preserve: the renaming handle reads a ChangeTime before
// Move and asks for it back afterwards, and this refuses to put it back once
// anything else has advanced the same file's ChangeTime in the meantime.
//
// The comparison and the write happen inside one transaction, against a row read
// inside that transaction, so on a backend whose transaction serialises that
// read against concurrent writers a WRITE either loses the race and is
// overwritten by a value newer than its own, or wins it and is left alone.
// Comparing outside the transaction would only move the window rather than
// narrow it. What makes this safe is the store's isolation, not the shape of
// this function — and every backend now provides it, postgres because its
// transactions run at REPEATABLE READ.
//
// On the losing path nothing is written at all. On the winning path the row
// read in this transaction is written back with nothing but ChangeTime changed,
// which spares a concurrent size or mtime advance because that read is
// serialised against the writer. Were it not, a write committing between the
// read and the update would be overwritten wholesale — the residual half of the
// lost-update shape the rename path shares, which writing the in-transaction
// row narrows but only the store's isolation can close.
//
// There is no permission check: the caller is the rename itself, which the
// metadata layer has already authorized on the parent directories, and the
// stored value is only ever moved back to one this file already had. A
// read-only share cannot reach here, because Move would have refused first.
func (s *Service) RestoreChangeTimeIfUnchanged(ctx context.Context, handle FileHandle, expected, want time.Time) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	return withRelaxedTransaction(store, ctx, func(tx Transaction) error {
		current, err := tx.GetFile(ctx, handle)
		if err != nil {
			return err
		}
		if !current.Ctime.Equal(expected) {
			return nil
		}
		current.Ctime = want
		return tx.UpdateAttrs(ctx, current)
	})
}

// RestoreFrozenTimestamps puts timestamps a handle froze back into the file
// after an operation stamped over them.
//
// ChangeTime is gated: it is written only while preOpCtime — the value the file
// held immediately before the operation this restore is undoing — is still the
// frozen value. That is the whole test for "nobody else moved it": the
// operation's own stamp is indistinguishable from a peer's advance once it has
// landed, so the only point at which they can be told apart is before it. A
// peer that advanced ChangeTime between the freeze and the operation therefore
// keeps its advance, instead of having it dragged backwards — and NFSv4 encodes
// its change attribute from Ctime, where a value that goes backwards lets a
// client keep serving a cache it should have dropped.
//
// A zero preOpCtime means the caller could not read the file before its
// operation. ChangeTime is then left alone rather than restored unverified,
// because an unverified restore is exactly the backwards write this guards.
//
// The remaining fields are written unconditionally: none carries a forward-only
// contract. LastWriteTime and LastAccessTime are expected to move in either
// direction, and CreationTime is not restamped by the operations that reach
// here.
//
// The read and the write share one transaction, so the comparison cannot be
// invalidated between them, and the row read in that transaction is written
// back wholesale, which spares a concurrent size or mode advance on a store
// whose transaction serialises the read against the writer.
//
// There is no permission check. The values restored are ones this file already
// held, and the handle asking for them held FILE_WRITE_ATTRIBUTES when it froze
// them; re-authorizing the restore would let a file that changed owner between
// the freeze and the operation be refused its own timestamp, which is the
// failure the SMB layer carries a grant through to avoid.
//
// decision: the gate reads the pre-operation value from the caller rather than
// from inside this transaction, so a peer that advances ChangeTime after the
// caller's operation committed but before this transaction opens is still
// overwritten by the frozen value. Closing that means reading the pre-op value
// inside the same transaction as the operation's own stamp, which is a change
// to CommitWrite and CreateHardLink rather than to this function. The window is
// one store round trip on the restoring handle, against a peer advance landing
// inside it, and the current behaviour it replaces has no window at all.
func (s *Service) RestoreFrozenTimestamps(ctx context.Context, handle FileHandle, preOpCtime time.Time, want *SetAttrs) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	return withRelaxedTransaction(store, ctx, func(tx Transaction) error {
		current, err := tx.GetFile(ctx, handle)
		if err != nil {
			return err
		}
		changed := false
		if want.Ctime != nil && !preOpCtime.IsZero() && preOpCtime.Equal(*want.Ctime) {
			current.Ctime = *want.Ctime
			changed = true
		}
		if want.Mtime != nil {
			current.Mtime = *want.Mtime
			changed = true
		}
		if want.Atime != nil {
			current.Atime = *want.Atime
			changed = true
		}
		if want.CreationTime != nil {
			current.CreationTime = *want.CreationTime
			changed = true
		}
		if !changed {
			return nil
		}
		return tx.UpdateAttrs(ctx, current)
	})
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

	// When this SetAttr targets a directory and sets its timestamps, hold the
	// per-directory flush lock across the whole persist-then-Clear sequence so a
	// concurrent flushDirTimes cannot interleave: without it a flush that already
	// captured the pending bump could commit AFTER we persist a deliberately
	// OLDER time (e.g. an SMB frozen-timestamp restore) and resurrect the bump
	// durably (#1573). Only the time-setting case needs this — a mode/owner-only
	// change never lowers a timestamp, so a racing flush is harmless there.
	//
	// Ctime counts as an explicit directory-timestamp set like the others. It
	// was missing, and the omission had a consequence: a restore that freezes
	// ONLY the change time (restoreParentDirFrozenTimestamps sends Ctime alone
	// when mtime and atime are not frozen) left the coalesced create/remove bump
	// pending, and the next held write lifted the row back up to it — walking
	// the frozen value forward, which is the one thing freezing it is for.
	dirTimeSet := file.Type == FileTypeDirectory &&
		(attrs.Mtime != nil || attrs.Atime != nil || attrs.Ctime != nil ||
			attrs.MtimeNow || attrs.AtimeNow)
	if dirTimeSet {
		lock := s.dirTimes.FlushLock(handle)
		lock.Lock()
		defer lock.Unlock()
	}

	// The coalesced directory bump this call must not lose, read AFTER the flush
	// lock above: a create or remove landing while this was waiting for the lock
	// records a bump that belongs to the state this write is about to commit,
	// and a value captured before the wait would not carry it.
	//
	// Only the pending bump, not a pre-call snapshot of the whole change time.
	// A held change time means "leave the stored value as it is", so the stored
	// value is the authority — including when a peer deliberately lowered it,
	// which is what an SMB frozen-timestamp restore does. Carrying a pre-call
	// snapshot forward would write that restore back up to whatever this call
	// happened to read first. The pending bump is the one exception, because it
	// is newer than the row by construction and has simply not been flushed.
	var pendingDirCtime time.Time
	if file.Type == FileTypeDirectory {
		if _, ctime, _, ok := s.dirTimes.GetPending(handle); ok {
			pendingDirCtime = ctime
		}
	}

	// The row as this call found it, captured before the coalesced-timestamp
	// overlay and before any mutation below. Every field the commit writes back
	// is decided by comparing against this, so the overlay counts as one of
	// this call's changes and is persisted exactly as it is today, while a
	// field neither the caller nor the overlay touched is left to the row.
	pre := CopyFileAttr(&file.FileAttr)

	// Overlay any coalesced (not-yet-persisted) directory timestamps so the WCC
	// pre-op snapshot and the returned post-op attrs match what a concurrent
	// GETATTR/LOOKUP on this directory would report right now (#1573).
	s.mergeDirTimes(handle, &file.FileAttr)

	// Which timestamp columns the commit below must write back. Seeded from the
	// overlay just applied, which counts as one of this call's own changes: an
	// explicit directory-timestamp set clears the pending entry at the end of
	// this function, so a bump the write does not carry is lost outright.
	atimeSet := !file.Atime.Equal(pre.Atime)
	mtimeSet := !file.Mtime.Equal(pre.Mtime)
	ctimeSet := !file.Ctime.Equal(pre.Ctime)
	creationTimeSet := false

	// Capture pre-op attributes: a copy of the file as observed by this
	// operation, before any mutation is applied below. This is exactly the
	// state WCC "before" must describe.
	wcc := &DirWcc{Before: CopyFileAttr(&file.FileAttr)}

	// Check permissions based on what's being changed
	identity := ctx.Identity
	isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0

	// Read-only share ceiling — both the per-user flag (#1276) and the
	// store-level ShareOptions.ReadOnly. Every SETATTR mutation — chmod,
	// chown/chgrp, ACL replace, explicit/utime-now timestamps, truncate — is a
	// write, so neither a read-only user nor any user on a read-only share may
	// perform them. This guard sits BEFORE the owner/root bypass below: without
	// it, the isOwner short-circuit would let an owner mutate their own file on
	// a read-only share (most dangerously, install a permissive DACL on it).
	// shareForbidsWrites checks both ceilings (per-user and store-level).
	if s.shareForbidsWrites(ctx, handle) {
		// A read-only denial returns ErrReadOnly so NFSv3 SETATTR yields
		// NFS3ERR_ROFS (EROFS) per RFC 1813 §3.3.7. NOTE: unlike the data
		// WRITE/CREATE/DELETE path — which distinguishes a per-user read-only
		// permission level (→ ErrAccessDenied/EACCES) from a genuinely
		// read-only share (→ ErrReadOnly/EROFS) via shareIsReadOnly — SETATTR
		// still maps the per-user ceiling to EROFS. Aligning it is a deferred
		// follow-up; SMB renders ErrReadOnly as STATUS_ACCESS_DENIED regardless.
		return nil, &StoreError{
			Code:    ErrReadOnly,
			Message: "read-only share: attribute change denied",
			Path:    file.Path,
		}
	}

	// The attributes that require ownership rather than mere write permission.
	//
	// decision: the mode-bit masks count as ownership-requiring, the same as an
	// absolute Mode. They flip DOS attribute bits, and a DOS attribute change is
	// ownership-gated in its own right — so none of the write-permission
	// relaxations below may admit one, however small the truncate or timestamp
	// bump it is bundled with. Every relaxation keyed on this flag inherits the
	// rule; onlyClearingSuidSgid does not use it and carries its own clause.
	// Withdraw this only if DOS attributes stop being ownership-gated, not
	// because a caller finds a pairing convenient.
	noOwnershipAttrs := attrs.Mode == nil && attrs.UID == nil && attrs.GID == nil &&
		attrs.ModeOrMask == nil && attrs.ModeAndNotMask == nil

	// POSIX: For utimensat() with UTIME_NOW, write permission is sufficient.
	onlySettingTimesToNow := noOwnershipAttrs && attrs.Size == nil &&
		(attrs.AtimeNow || attrs.MtimeNow)

	// POSIX: truncate() requires write access, not ownership.
	onlySettingSize := noOwnershipAttrs && attrs.Size != nil &&
		!attrs.AtimeNow && !attrs.MtimeNow

	// POSIX: setxattr()/removexattr() on an existing xattr require write
	// permission on the file, not ownership — same rule as truncate() and
	// utimensat(UTIME_NOW). An EA-mutations-only SetAttrs is that case, so a
	// caller with write access (the SMB EA case authorizes the open handle's
	// FILE_WRITE_EA bit at the handler layer) may apply it without owning the
	// file.
	// Explicit timestamp pointers are excluded: an EA write must not grant
	// the right to back- or forward-date the file, only to let the server
	// stamp the mutation time (UTIME_NOW semantics above).
	onlyEAMutations := noOwnershipAttrs && attrs.Size == nil &&
		!attrs.AtimeNow && !attrs.MtimeNow &&
		attrs.Atime == nil && attrs.Mtime == nil && attrs.Ctime == nil &&
		attrs.CreationTime == nil &&
		attrs.Hidden == nil && attrs.ACL == nil &&
		len(attrs.EAMutations) > 0

	// POSIX: When a non-owner writes to a file with SUID/SGID bits set, those
	// bits must be cleared. The Linux NFS client implements this via
	// file_remove_privs() which sends SETATTR(mode = current & ~06000) before
	// the WRITE. We must allow this SETATTR even from non-owners who have write
	// permission, as long as the ONLY mode change is clearing SUID/SGID bits.
	onlyClearingSuidSgid := false
	// The mask clause mirrors noOwnershipAttrs, which this predicate cannot use
	// because it requires a non-nil Mode: a privilege strip bundled with a DOS
	// attribute flip is not a privilege strip.
	if attrs.Mode != nil && attrs.UID == nil && attrs.GID == nil && attrs.Size == nil &&
		attrs.ModeOrMask == nil && attrs.ModeAndNotMask == nil {
		clearedMode := file.Mode & ^uint32(0o6000)
		if *attrs.Mode == clearedMode && file.Mode&0o6000 != 0 {
			onlyClearingSuidSgid = true
		}
	}

	// Both timestamp-now and truncate-only operations allow write permission
	// as an alternative to ownership (POSIX semantics).
	writePermSufficient := onlySettingTimesToNow || onlySettingSize || onlyClearingSuidSgid || onlyEAMutations

	// SMB authorizes an explicit timestamp write by FILE_WRITE_ATTRIBUTES on the
	// open handle rather than by ownership, so such a handle satisfies the
	// ownership gate below — but only for a SetAttrs that changes nothing except
	// the four timestamps, and never past an explicit DENY ACE, which encodes
	// intent POSIX bits cannot express (mirroring the handle write bypass in
	// checkFilePermissions). Both read-only ceilings are already enforced by
	// shareForbidsWrites above. See AuthContext.TimestampAuthorizedByHandle.
	onlySettingExplicitTimes := noOwnershipAttrs && attrs.Size == nil &&
		attrs.Hidden == nil && attrs.ACL == nil && len(attrs.EAMutations) == 0 &&
		!attrs.AtimeNow && !attrs.MtimeNow &&
		(attrs.Atime != nil || attrs.Mtime != nil ||
			attrs.Ctime != nil || attrs.CreationTime != nil)
	timestampAuthorizedByHandle := ctx.TimestampAuthorizedByHandle &&
		onlySettingExplicitTimes && !acl.HasExplicitDeny(file.ACL)

	// decision: an EA-only write is authorized by FILE_WRITE_EA on the open
	// handle instead of by the file's POSIX mode. The right the protocol checked
	// at the SET_INFO gate stands in for ownership here, on the same terms as
	// the timestamp case above, and the bypass is held to two limits: the
	// SetAttrs must change nothing but extended attributes, so it cannot carry a
	// mode or owner change through on the same call; and it never runs past an
	// explicit DENY ACE. Withdraw it if a handle can ever be opened with
	// FILE_WRITE_EA without the share-level check having run.
	//
	// The DENY limit is deliberately blunter than it reads: HasExplicitDeny
	// matches any deny ACE on the file, whatever principal or right it names, so
	// a DENY of an unrelated right to an unrelated principal also withdraws the
	// bypass and sends the write back to the POSIX check. That refuses some EA
	// writes a precise evaluation would allow, which is the safe direction for a
	// gate whose whole purpose is to let a write past the POSIX mode. Narrow it
	// to a deny that actually covers this principal and FILE_WRITE_EA only with
	// a test that pins which ACEs newly stop withdrawing it.
	eaAuthorizedByHandle := ctx.EAAuthorizedByHandle &&
		onlyEAMutations && !acl.HasExplicitDeny(file.ACL)

	// True when the ownership gate below is what authorizes this call, and so
	// the one decision the write closure has to recheck against the row its
	// transaction reads. A right the open handle carries was granted at open
	// and a later chown does not revoke it, so those two are left alone.
	//
	// A write-permission-gated operation is NOT exempt merely for being one:
	// the switch below lets ownership answer first, so an owner reaches a
	// truncate or a utimes-to-now without the write check ever running, and
	// exempting them would let a caller keep operating on a file a peer chown
	// has handed to someone else — the hole the recheck exists to close. Only
	// a caller who is not the owner got there through checkWritePermission.
	//
	// decision: for that caller the recheck is skipped outright rather than
	// re-run against the row, so a chown that moves them between the group and
	// other bit triples is not caught and their truncate or utimes proceeds on
	// permission they no longer have. Re-running it here means a share-options
	// read on the store's own connection from inside an open transaction, and
	// the guard is already blind to the peer chmod that changes the same
	// answer. Close it if the permission decision can ever be recomputed from
	// the row alone.
	ownershipAuthorized := isOwner && !isRoot &&
		!eaAuthorizedByHandle && !timestampAuthorizedByHandle

	switch {
	case isOwner || isRoot:
		// Ownership answers for everything below.
	case eaAuthorizedByHandle || timestampAuthorizedByHandle:
		// A right the protocol names on the open stands in for ownership, and
		// for the POSIX write check the alternatives below would run.
	case writePermSufficient:
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return nil, err
		}
	default:
		return nil, &StoreError{
			Code:    ErrPermissionDenied,
			Message: "operation not permitted",
			Path:    file.Path,
		}
	}

	now := time.Now()
	modified := false

	// Set by either POSIX setid strip below. The strip is a read-modify-write,
	// so the commit re-applies it to the row rather than copying a mode derived
	// from the pre-transaction read over a concurrent chmod.
	clearSetIDBits := false
	// The mode a chmod asks the ACL to be adjusted for, once the SUID/SGID
	// stripping above has had its say. Nil when this call is not a chmod. The
	// later ModeOrMask/ModeAndNotMask bits are deliberately not included: they
	// carry DOS attribute flags, which no ACE expresses.
	var aclAdjustMode *uint32

	// True when a decision below read the file's own GID, which is the only
	// thing that makes a peer chgrp able to invalidate it. Ownership itself is
	// a UID comparison, so an owner stays the owner across a chgrp.
	groupConsulted := false
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
				// For SGID, caller must be owner AND member of file's group.
				// This membership test is the one place an ownership-authorized
				// call reads the file's GID, so the row it commits to has to
				// still carry the group the answer was computed against.
				if isOwner {
					groupConsulted = true
				}
				if !isOwner || !identity.HasGID(file.GID) {
					newMode &= ^uint32(0o2000)
				}
			}
		}

		file.Mode = newMode | (file.Mode & dosAttributeModeBits)

		// RFC 7530 Section 6.4.1: chmod adjusts OWNER@/GROUP@/EVERYONE@ ACEs
		// to match the new mode bits when an ACL is present. The adjustment is
		// redone against the committed row inside the transaction; this one
		// keeps the in-memory copy consistent for the post-op attributes.
		if file.ACL != nil {
			file.ACL = acl.AdjustACLForMode(file.ACL, newMode)
		}
		aclAdjustMode = &newMode

		modified = true
	}

	// Atomic mode-bit masks: applied within the same store read-modify-write
	// as the rest of SetFileAttributes so concurrent bit flips (e.g. SET_SPARSE
	// racing SET_COMPRESSION) cannot clobber each other with a stale snapshot.
	//
	// These fields exist solely for DOS attribute bits (high word) — the
	// FSCTL-managed compression and sparse flags, and the attributes an SMB
	// FileAttributes update sets and clears.
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
			ownershipChanged = true
		}
		// Set whether or not the value moved. Naming the owner this call has
		// already read is still a request, and only an open transaction can
		// write it back over a chown that landed in the gap. ownershipChanged
		// stays keyed off the move, because it drives the POSIX setid strip,
		// which answers to an ownership change and not to a chown request.
		modified = true
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
			ownershipChanged = true
		}
		modified = true
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
		clearSetIDBits = true
	}

	if attrs.Size != nil {
		// Only a regular file has a length a caller can set. A directory's size
		// is bookkeeping the store owns, and a symlink, device, fifo or socket
		// has no byte stream to lengthen or discard, so accepting the value
		// would record a size no read could ever agree with.
		if file.Type == FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrIsDirectory,
				Message: "cannot set size of a directory",
				Path:    file.Path,
			}
		}
		if file.Type != FileTypeRegular {
			return nil, &StoreError{
				Code:    ErrInvalidArgument,
				Message: "cannot set size of a non-regular file",
				Path:    file.Path,
			}
		}

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
		// The prune itself is derived inside the transaction, from the row that
		// attempt actually read: a list trimmed from this pre-transaction copy
		// would be written back over whatever a concurrent WRITE committed in
		// the gap, discarding its new ranges, and on a retry it would re-apply
		// the same stale trim rather than re-deriving it. Deciding it here
		// cannot see that writer at all — the row is already in hand before the
		// transaction opens.
		file.Size = *attrs.Size
		modified = true

		// POSIX: truncate updates mtime and ctime when size changes
		// The server must do this even if the client doesn't send TIME_MODIFY_SET,
		// because POSIX requires it and NFS clients may rely on server-side updates.
		file.Mtime = now
		mtimeSet = true
		// Stamped unconditionally, including under PreserveCtime: the write
		// closure below is the one place that decides what a held change time
		// ends up as, and it keeps the row's own value over whatever this call
		// stamped. A second guard here would be a second answer to the same
		// question, and the two could drift.
		file.Ctime = now
		ctimeSet = true

		// POSIX: Clear SUID/SGID bits on truncate for non-root users (like write)
		if file.Type == FileTypeRegular && !isRoot {
			file.Mode &= ^uint32(0o6000)
			clearSetIDBits = true
		}
	}

	if attrs.Atime != nil {
		file.Atime = *attrs.Atime
		atimeSet = true
		modified = true
	}

	if attrs.Mtime != nil {
		file.Mtime = *attrs.Mtime
		mtimeSet = true
		modified = true
	}

	if attrs.AtimeNow {
		file.Atime = now
		atimeSet = true
		modified = true
	}

	if attrs.MtimeNow {
		file.Mtime = now
		mtimeSet = true
		modified = true
	}

	if attrs.CreationTime != nil {
		file.CreationTime = *attrs.CreationTime
		creationTimeSet = true
		modified = true
	}

	if attrs.Ctime != nil {
		file.Ctime = *attrs.Ctime
		ctimeSet = true
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
		// Same three answers as the gate above, and for the same reasons: a
		// handle carrying FILE_WRITE_EA authorizes the mutation on its own, and
		// everyone else needs POSIX write. Kept in step with that gate — a check
		// here that the gate does not make refuses what the gate just allowed.
		if !isOwner && !isRoot && !eaAuthorizedByHandle {
			if err := s.checkWritePermission(ctx, handle); err != nil {
				return nil, err
			}
		}
		file.ApplyEAMutations(attrs.EAMutations)
		modified = true
	}

	// The attributes the transaction actually committed, left nil when nothing
	// was modified and no transaction ran.
	var writtenAttr *FileAttr

	// Auto-update ctime when attributes change, unless explicitly set
	if modified {
		if attrs.Ctime == nil && !attrs.PreserveCtime {
			file.Ctime = now
			ctimeSet = true
		}

		// writeRow re-reads the inode inside the transaction and copies onto it
		// exactly the fields this call changed, so every other column comes from
		// committed state instead of from the copy read before the transaction
		// opened. Writing that earlier copy back reverts whatever a concurrent
		// writer committed in the gap — a chmod undoing a WRITE's size and
		// mtime, the backwards move NFSv4's change attribute must never make —
		// and no isolation level can help, because the stale row is already in
		// hand before the transaction starts. Exact on a backend whose
		// transaction serialises the read against concurrent writers; on one
		// whose in-transaction read takes no row lock this narrows the window
		// rather than closing it, the same residue
		// RestoreChangeTimeIfUnchanged documents.
		//
		// Which fields it copies is decided by what the caller asked for, never
		// by whether the value this call computed differs from the one it read.
		// A request whose value already equals the pre-read value is still a
		// request: skipping the column there leaves a concurrent writer's value
		// standing while this call reports success, which is the same lost
		// update in the other direction.
		//
		// A read-modify-write — the mode masks, the setid strip, the EA
		// mutations — is re-applied to the row instead of copied from the
		// pre-read copy, so it composes with whatever the peer committed rather
		// than replacing it.
		//
		// The re-read's failure is the operation's failure. Falling back to the
		// pre-transaction snapshot would write back a row this call has already
		// been told not to trust, and UpdateAttrs is allowed to create a row
		// that is missing — so a file deleted between the two reads would be
		// recreated carrying the stale state, which is a worse outcome than
		// refusing the attribute change.
		writeRow := func(tx Transaction) error {
			// Before the read, so the fold below runs against the committed map
			// on a backend that refuses the second writer only once its update
			// is reached — the same reason xattr.go's own EA path takes it.
			// Scoped to an EA write rather than taken unconditionally: every
			// other field here is either replaced outright or recomputed from
			// the row on each attempt, so the store's conflict-and-retry is
			// what orders those and a lock on every chmod and utimes would buy
			// nothing.
			if len(attrs.EAMutations) > 0 {
				if locker, ok := tx.(FileRowLocker); ok {
					if err := locker.LockFileRow(ctx.Context, handle); err != nil {
						return err
					}
				}
			}

			row, err := tx.GetFile(ctx.Context, handle)
			if err != nil {
				return err
			}
			if row == nil {
				return &StoreError{
					Code:    ErrNotFound,
					Message: "file disappeared while its attributes were being written",
					Path:    file.Path,
				}
			}

			// Per attempt, not per call: a retry re-derives the prune from the
			// row it just read, and a stale true from an earlier attempt would
			// send an unchanged manifest through SetManifest.
			pruneManifest := false

			// The ownership gate above ran against the copy read before the
			// transaction, so an owner-authorized change would otherwise land
			// on a row a concurrent chown has since handed to someone else —
			// the gate and the write describing different files. Every other
			// justification holds whoever owns the file, so refusing those here
			// would be a spurious EPERM on an operation whose authorization
			// never read the owner — see ownershipAuthorized above. No
			// isolation level catches this: the chown committed before this
			// transaction opened, so its row is simply what the snapshot sees
			// and nothing conflicts.
			//
			// Only on the fields the decision actually read. Ownership is a UID
			// comparison, so a peer chgrp leaves the owner the owner and must
			// not refuse them; the file's GID matters only to the SGID grant,
			// which records that it read it.
			if ownershipAuthorized &&
				(row.UID != pre.UID || (groupConsulted && row.GID != pre.GID)) {
				return &StoreError{
					Code:    ErrPermissionDenied,
					Message: "ownership changed while the attribute change was being applied",
					Path:    file.Path,
				}
			}

			// Folded onto the row under the lock taken above rather than copied
			// from the pre-read map, so a peer's concurrent EA write keeps its
			// own keys instead of being replaced wholesale. Re-applying the
			// same mutations on a retry lands the same map.
			if len(attrs.EAMutations) > 0 {
				row.ApplyEAMutations(attrs.EAMutations)
			}

			if attrs.Mode != nil {
				// The mask path below owns only the DOS bits and the absolute-Mode
				// path owns only the permission bits, so neither can overwrite
				// what the other manages. Carry the row's DOS bits across rather
				// than copying the pre-transaction snapshot's: an absolute mode
				// replaces the permission triple, but a peer's concurrent
				// FSCTL_SET_COMPRESSION or attribute flip is still committed state
				// this call never addressed.
				row.Mode = (row.Mode & dosAttributeModeBits) | (file.Mode & 0o7777)
			}
			if attrs.ModeOrMask != nil {
				row.Mode |= *attrs.ModeOrMask & dosAttributeModeBits
			}
			if attrs.ModeAndNotMask != nil {
				row.Mode &^= *attrs.ModeAndNotMask & dosAttributeModeBits
			}
			if clearSetIDBits {
				row.Mode &^= 0o6000
			}
			if attrs.UID != nil {
				row.UID = file.UID
			}
			if attrs.GID != nil {
				row.GID = file.GID
			}
			if attrs.Hidden != nil {
				row.Hidden = file.Hidden
			}
			if atimeSet {
				row.Atime = file.Atime
			}
			if mtimeSet {
				row.Mtime = file.Mtime
			}
			if creationTimeSet {
				row.CreationTime = file.CreationTime
			}
			// A held change time means the stored value is the authority — a
			// peer's commit stands, higher or lower — so the row keeps its own
			// Ctime whatever the branches above stamped.
			//
			// Lifted only by a directory bump that is recorded but not yet
			// flushed: that value is newer than the row by construction, and
			// the Clear that follows an explicit directory-time set would
			// otherwise discard it for good, moving a peer's visible change
			// time backwards.
			switch {
			case attrs.PreserveCtime && attrs.Ctime == nil:
				if pendingDirCtime.After(row.Ctime) {
					row.Ctime = pendingDirCtime
				}
			case ctimeSet:
				row.Ctime = file.Ctime
			}
			// An explicit ACL replaces the stored one outright. A chmod only
			// rewrites the mode's OWNER@/GROUP@/EVERYONE@ ACEs, and it has to
			// rewrite them on the ACL the row actually holds: the adjustment
			// made before the transaction opened was computed from a copy that
			// a concurrent ACL write may since have superseded, and copying it
			// over would discard that write wholesale.
			switch {
			case attrs.ACL != nil:
				row.ACL = file.ACL
			case aclAdjustMode != nil && row.ACL != nil:
				row.ACL = acl.AdjustACLForMode(row.ACL, *aclAdjustMode)
			}
			if attrs.Size != nil {
				row.Size = file.Size
			}
			// Derive the prune from the row this attempt read, so a retry
			// re-derives it and a concurrent writer's ranges are not discarded.
			// The manifest changed only when the trim actually dropped
			// something, which is also what selects SetManifest over UpdateAttrs
			// below: a pure grow keeps the stored list and takes the relaxed
			// path.
			if attrs.Size != nil {
				pruned := block.PruneChunkRefsToSize(row.Blocks, *attrs.Size)
				if len(pruned) != len(row.Blocks) {
					row.Blocks = pruned
					// Keep ObjectID (the Merkle root over Blocks) consistent with
					// the trimmed list, or zero it when no blocks remain so the
					// file reads as "never quiesced" instead of carrying a stale
					// dedup pointer.
					switch {
					case len(pruned) == 0 && !row.ObjectID.IsZero():
						row.ObjectID = block.ObjectID{}
					case len(pruned) > 0:
						row.ObjectID = block.ComputeObjectID(pruned)
					}
					pruneManifest = true
				}
			}

			// The post-op attributes this call reports must describe the row it
			// actually wrote. Recorded beside `file` rather than onto it: the
			// transaction is retried on a transient conflict, and a later
			// attempt still has to compare this call's mutations against `pre`.
			writtenAttr = CopyFileAttr(&row.FileAttr)

			if pruneManifest {
				return tx.SetManifest(ctx.Context, row)
			}
			return tx.UpdateAttrs(ctx.Context, row)
		}
		// A size change (truncate/grow) is data-paired: the new size must
		// survive a crash together with the block data, or a read past the new
		// EOF returns stale-tail / silent-truncation bytes (#588). Persist it
		// through a fully-durable transaction so relaxed-durability mode still
		// fsyncs it. Pure-attribute changes (mode/owner/times/xattr/ACL) are not
		// data-paired, so they take the relaxed path — note store.UpdateAttrs would
		// force an inline fsync (it wraps the durable WithTransaction), so it is
		// deliberately bypassed here in favor of withRelaxedTransaction to
		// actually defer the write (#1573 Wall 1).
		if attrs.Size != nil {
			// A truncate/grow makes the committed size authoritative and
			// supersedes any buffered WRITE. Hold the per-handle flush lock
			// across the durable size commit AND the discard so a concurrent
			// FlushPendingWriteForFile cannot pop a stale MaxSize and re-grow the
			// file between the two — silently undoing the truncate (#1753).
			mu := s.pendingWrites.GetFlushLock(handle)
			mu.Lock()
			err := store.WithTransaction(ctx.Context, writeRow)
			if err == nil {
				// Discard, don't flush: a buffered MaxSize would resurrect the
				// pre-truncate size on the next flush.
				s.pendingWrites.PopPending(handle)
			}
			mu.Unlock()
			if err != nil {
				return nil, err
			}
		} else {
			if err := withRelaxedTransaction(store, ctx.Context, writeRow); err != nil {
				return nil, err
			}
			// Invalidate cached file in pending writes to ensure subsequent
			// writes use fresh attributes (e.g., mode changes for SUID/SGID clearing)
			s.pendingWrites.InvalidateCache(handle)
		}
	}

	// An explicit directory-timestamp set supersedes any coalesced create/remove
	// bump: the persisted mtime/atime (which may be deliberately OLDER, e.g. an
	// SMB frozen-timestamp restore) is now authoritative, so drop the pending
	// overlay that would otherwise resurrect the newer create time (#1573). Runs
	// under the flush lock acquired above, so no concurrent flush can re-persist
	// the bump between the store write and this Clear.
	//
	// Conditional on the bump this call actually accounted for, because the
	// flush lock does not hold the writers back: recordDirTimes runs after a
	// create's or remove's transaction and takes no lock at all, so one can land
	// between the read above and here. An unconditional Clear would drop that
	// newer bump from the overlay and from durable state both — a create whose
	// directory timestamp simply vanishes. ClearIfFlushed keeps the entry when
	// a newer bump raced in, and the next flush picks it up.
	if dirTimeSet {
		s.dirTimes.ClearIfFlushed(handle, pendingDirCtime)
	}

	// Post-op attributes reflect the resulting file state: the row the
	// transaction wrote when one ran, otherwise the unchanged snapshot (which
	// equals Before).
	wcc.After = writtenAttr
	if wcc.After == nil {
		wcc.After = CopyFileAttr(&file.FileAttr)
	}
	return wcc, nil
}

// Move moves or renames a file or directory atomically.
//
// POSIX rename silently unlinks an existing destination, and Move never
// touches the block store. The clobbered victim is returned as the first
// result so the caller can coordinate content deletion, the same contract
// RemoveFile carries: nil when the rename replaced nothing, replaced a
// directory (which owns no content), or recycled the victim into the trash bin
// rather than destroying it; and a non-nil File whose PayloadID is empty when
// a remaining hard link means the content must survive.
func (s *Service) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) (*File, *RenameWcc, error) {
	store, err := s.storeForHandle(fromDir)
	if err != nil {
		return nil, nil, err
	}

	// A move re-parents one entry, so both directories must live in the same
	// share.
	if err := requireSameShare(fromDir, toDir, "move an entry", toName); err != nil {
		return nil, nil, err
	}

	// Validate names
	if err := ValidateName(fromName); err != nil {
		return nil, nil, err
	}
	if err := ValidateName(toName); err != nil {
		return nil, nil, err
	}

	// Same directory and same name - no-op (POSIX rename semantics)
	if string(fromDir) == string(toDir) && fromName == toName {
		return nil, nil, nil
	}

	// Get source directory
	srcDir, err := store.GetFile(ctx.Context, fromDir)
	if err != nil {
		return nil, nil, err
	}
	if srcDir.Type != FileTypeDirectory {
		return nil, nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "source parent is not a directory",
		}
	}

	// Get destination directory
	dstDir, err := store.GetFile(ctx.Context, toDir)
	if err != nil {
		return nil, nil, err
	}
	if dstDir.Type != FileTypeDirectory {
		return nil, nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Validate destination path length (POSIX PATH_MAX compliance)
	destPath := buildPath(dstDir.Path, toName)
	if err := ValidatePath(destPath); err != nil {
		return nil, nil, err
	}

	// Check write permission on both directories
	if err := s.checkWritePermission(ctx, fromDir); err != nil {
		return nil, nil, err
	}
	if err := s.checkWritePermission(ctx, toDir); err != nil {
		return nil, nil, err
	}

	// Get source file
	srcHandle, err := store.GetChild(ctx.Context, fromDir, fromName)
	if err != nil {
		return nil, nil, err
	}
	srcFile, err := store.GetFile(ctx.Context, srcHandle)
	if err != nil {
		return nil, nil, err
	}

	// Check sticky bit on source directory
	if err := CheckStickyBitRestriction(ctx, &srcDir.FileAttr, &srcFile.FileAttr); err != nil {
		return nil, nil, err
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
			return nil, nil, &StoreError{
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
		// Both names already resolve to the same file, so they are hard links
		// of each other and the rename has nothing to move: renaming one over
		// the other would destroy a link. POSIX rename(2) and RFC 7530
		// section 16.27.4 both make this a successful no-op.
		if string(srcHandle) == string(dstHandle) {
			return nil, nil, nil
		}

		// Destination exists - check compatibility
		dstFile, err = store.GetFile(ctx.Context, dstHandle)
		if err != nil {
			return nil, nil, err
		}

		// Check sticky bit on destination directory
		if err := CheckStickyBitRestriction(ctx, &dstDir.FileAttr, &dstFile.FileAttr); err != nil {
			return nil, nil, err
		}

		// Type compatibility checks
		if srcFile.Type == FileTypeDirectory {
			if dstFile.Type != FileTypeDirectory {
				return nil, nil, &StoreError{
					Code:    ErrNotDirectory,
					Message: "cannot overwrite non-directory with directory",
				}
			}
			// Check if destination directory is empty
			entries, _, err := store.ListChildren(ctx.Context, dstHandle, "", 1, NamesOnly)
			if err == nil && len(entries) > 0 {
				return nil, nil, &StoreError{
					Code:    ErrNotEmpty,
					Message: "destination directory not empty",
				}
			}
		} else {
			if dstFile.Type == FileTypeDirectory {
				return nil, nil, &StoreError{
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
							return nil, nil, err // never silently clobber
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
		return nil, nil, err
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

	// A cross-parent directory rename decrements fromDir's link-count key and
	// increments toDir's (the ".." reference moves parents, below). Those are the
	// same shared counter keys mkdir/rmdir bump, so without serialization a rename
	// re-introduces the BadgerDB SSI conflict #1571 fixes — racing a concurrent
	// mkdir/rmdir/rename on either parent. Serialize both parents for the whole
	// transaction. File moves never touch parent nlink and stay lock-free.
	if srcFile.Type == FileTypeDirectory && !sameDir {
		defer s.lockParentLinks(fromDir, toDir)()
	}

	// Execute all write operations in a single transaction for better performance.
	// Relaxed durability (#1573 Wall 1): rename rewrites only directory entries
	// and inode paths — the moved file's size and block manifest are untouched,
	// so this is pure namespace. A crash can lose the rename (old name
	// persists), never corrupt data.
	now := time.Now()
	// Link count the transaction below leaves the clobbered destination with.
	// Only meaningful when a non-directory victim exists.
	var clobberedNlink uint32
	txErr := withRelaxedTransaction(store, ctx.Context, func(tx Transaction) error {
		// The GetChild lookups above ran outside this transaction and are
		// advisory only: no lock covers a file rename, so a concurrent rename
		// or unlink can retarget either name in the gap. Re-resolve both
		// namespace edges here and abort when they no longer match, so two
		// renames onto the same destination cannot both commit and orphan an
		// inode. Reading the child keys through the transaction also enters
		// them in its read set, which is what lets an optimistic backend see
		// the write-write conflict at all.
		txSrcHandle, srcErr := tx.GetChild(ctx.Context, fromDir, fromName)
		if srcErr != nil {
			return srcErr
		}
		if string(txSrcHandle) != string(srcHandle) {
			return &StoreError{
				Code:    ErrConflict,
				Message: "source entry changed during rename",
				Path:    fromName,
			}
		}
		txDstHandle, dstErr := tx.GetChild(ctx.Context, toDir, toName)
		if dstErr != nil && !IsNotFoundError(dstErr) {
			return dstErr
		}
		dstNowExists := dstErr == nil
		if dstNowExists != (dstFile != nil) ||
			(dstNowExists && string(txDstHandle) != string(dstHandle)) {
			return &StoreError{
				Code:    ErrConflict,
				Message: "destination entry changed during rename",
				Path:    toName,
			}
		}

		// A directory moving to a different parent is the only rename that can
		// make a directory its own ancestor: renaming within one parent leaves
		// every parent edge above the source where it was.
		if srcFile.Type == FileTypeDirectory && !sameDir {
			if err := refuseDirectoryLoop(ctx.Context, tx, srcHandle, toDir); err != nil {
				return err
			}
		}

		// Re-read the source/destination directories inside the transaction so
		// the pre-op snapshots and the timestamp mutations derive from the same
		// committed state (After then monotonic w.r.t. Before).
		if txSrc, sErr := tx.GetFile(ctx.Context, fromDir); sErr == nil && txSrc != nil {
			srcDir = txSrc
		}
		// Overlay any pending coalesced bump so Before reflects the same mtime a
		// concurrent GETATTR would see (the tx read only sees durable state), so
		// WCC stays continuous across rapid same-dir mutations (#1573).
		s.mergeDirTimes(fromDir, &srcDir.FileAttr)
		rename.FromDir.Before = CopyFileAttr(&srcDir.FileAttr)
		if !sameDir {
			if txDst, dErr := tx.GetFile(ctx.Context, toDir); dErr == nil && txDst != nil {
				dstDir = txDst
			}
			s.mergeDirTimes(toDir, &dstDir.FileAttr)
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
				// Report content ownership from the count actually written.
				// Assigned unconditionally: an optimistic backend may run this
				// closure more than once, and only the committing attempt's
				// value must survive.
				clobberedNlink = newCount
				// Update ctime on the file being unlinked (affects remaining
				// hard links).
				//
				// Write the row this transaction read, not the copy taken
				// before it opened. Ctime is the only column an overwriting
				// rename changes on the victim, so every other one must come
				// from committed state: writing the earlier snapshot back
				// would restore whatever Size or Mtime a concurrent write to
				// the victim had already committed. No isolation level closes
				// this — the stale copy is in hand before the transaction
				// starts, so its snapshot already contains that write and the
				// update conflicts with nothing.
				//
				// The re-read also feeds the clobbered-victim report below,
				// whose PayloadID must name the content actually committed.
				// Assigned unconditionally: an optimistic backend may run this
				// closure more than once, and only the committing attempt's
				// value must survive.
				victim, err := tx.GetFile(ctx.Context, dstHandle)
				if err != nil {
					return err
				}
				dstFile = victim
				victim.Ctime = now
				if err := tx.UpdateAttrs(ctx.Context, victim); err != nil {
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

		// Bump ctime on the renamed inode. The namespace edge has already been
		// relinked above (DeleteChild/SetChild/SetParent); File.Path is no
		// longer stored — every backend derives it on read from the
		// parent_child_map / parent edges (#1166), so a rename just moves the
		// edge and the new path is reconstructed fresh on the next GetFile.
		// This is what makes hard links correct: renaming one name can never
		// stale another name's path. UpdateAttrs is tx-critical: a failed ctime
		// write must roll the whole rename back.
		//
		// Read the inode inside the transaction, both before and after the
		// stamp, so a caller that wants to put the pre-rename ChangeTime back
		// gets values the store actually holds.
		//
		// Before, because srcFile was read outside this transaction: anything
		// that advanced the inode's ChangeTime since then is already committed,
		// and restoring the outside-tx value would erase it. After, because the
		// SQL backends store timestamps as FILETIME ticks and truncate; an
		// in-memory time.Time would not compare equal to what was written, so a
		// conditional restore keyed on it would silently never fire.
		//
		// Neither read may be discarded on error: a failed "before" with a
		// successful "after" leaves a zero SourcePreCtime that still matches,
		// and the restore would then write a zero ChangeTime.
		//
		// The "before" read covers the window only on backends whose
		// transaction serialises it against concurrent writers, which is all of
		// them: postgres runs this transaction at REPEATABLE READ, so a write
		// that commits between this read and the row lock the update takes
		// aborts the update rather than being erased by it.
		pre, err := tx.GetFile(ctx.Context, srcHandle)
		if err != nil {
			return err
		}
		rename.SourcePreCtime = pre.Ctime
		// Write the row this transaction read, not the one read before it
		// opened. Ctime is the only field a rename changes on the source
		// inode, so every other column must come from committed state:
		// writing the earlier snapshot back would silently restore whatever
		// Size or Mtime a concurrent write had already committed.
		pre.Ctime = now
		if err := tx.UpdateAttrs(ctx.Context, pre); err != nil {
			return err
		}
		post, err := tx.GetFile(ctx.Context, srcHandle)
		if err != nil {
			return err
		}
		rename.SourceCtime = post.Ctime

		return nil
	})

	if txErr != nil {
		return nil, nil, txErr
	}

	// Report the clobbered victim, if the rename replaced a file. Directories
	// own no content, and dstFile is nil both when the destination was absent
	// and when trash recycled it above, so neither case reports one.
	var clobbered *File
	if dstFile != nil && dstFile.Type != FileTypeDirectory {
		victim := *dstFile
		victim.FileAttr = *CopyFileAttr(&dstFile.FileAttr)
		victim.Nlink = clobberedNlink
		if clobberedNlink > 0 {
			// Another hard link still references the content, so it must
			// survive. An empty PayloadID is how RemoveFile says that too.
			victim.PayloadID = ""
		}
		clobbered = &victim
	}

	// Coalesce the parent directory mtime/ctime bumps out of the transaction so
	// a rename never writes the shared source/destination parent-inode keys
	// inside the txn — the same treatment create/unlink/rmdir already get
	// (#1573/#1643). The read overlay (mergeDirTimes) keeps the bump visible.
	s.recordDirTimes(ctx.Context, fromDir, now)
	srcDir.Mtime = now
	srcDir.Ctime = now
	rename.FromDir.After = CopyFileAttr(&srcDir.FileAttr)
	if !sameDir {
		s.recordDirTimes(ctx.Context, toDir, now)
		dstDir.Mtime = now
		dstDir.Ctime = now
		rename.ToDir.After = CopyFileAttr(&dstDir.FileAttr)
	}

	// Notify directory change after successful move
	s.notifyDirChange(shareNameForHandle(fromDir), fromDir, lock.DirChangeRenameEntry, ctx)
	if string(fromDir) != string(toDir) {
		// Cross-directory move: derive share from toDir in case it differs
		s.notifyDirChange(shareNameForHandle(toDir), toDir, lock.DirChangeAddEntry, ctx)
	}

	return clobbered, rename, nil
}

// refuseDirectoryLoop refuses a rename that would make srcHandle its own
// ancestor, by walking dstDir's parent edges looking for srcHandle. The chain
// ends at the share root, which has no parent edge.
//
// Every edge is read through tx, not through the store, which is what keeps the
// answer true when the rename commits: each edge the walk touches enters the
// transaction's read set, so a concurrent rename that re-parents any inode on
// the walked chain aborts this one. The same walk run before the transaction
// opens answers a question that can dissolve before the write lands — a racing
// rename can move dstDir under srcHandle in the gap, and the entry
// re-resolution above cannot see it, because it compares only the two edges
// this rename names.
//
// decision: three of the four backends cannot serve the walk a stale answer at
// all — memory holds a store-wide mutex for the whole closure, sqlite admits one
// transaction at a time, and badger alone actually detects the conflict, by SSI
// over the keys the walk read. Postgres runs at REPEATABLE READ, which is
// snapshot isolation: two renames writing disjoint parent edges are not a
// write-write conflict, so there the walk can be stale. Composing a cycle needs
// two cross-parent directory renames racing whose four parent handles all miss
// each other's lockParentLinks shards; a single rename, which is all one client
// can drive, is refused on every backend. Lock the walked rows (SELECT ... FOR
// SHARE) or run rename at SERIALIZABLE if a cycle is ever observed on postgres.
// Badger's guarantee is the operator's to keep: its options are passed through
// verbatim, so disabling conflict detection voids it silently.
func refuseDirectoryLoop(ctx context.Context, tx Transaction, srcHandle, dstDir FileHandle) error {
	_, srcID, err := DecodeFileHandle(srcHandle)
	if err != nil {
		return err
	}
	handle := dstDir

	// The walk records the ids it has visited and stops when one repeats, rather
	// than trusting that directory hard links are refused: a namespace an
	// unguarded build already corrupted would otherwise spin here forever. A
	// repeat is a cycle that predates this rename, so it reports EIO against the
	// store rather than EINVAL against the caller.
	//
	// decision: a depth bound would be the cheaper stop and a large one would in
	// fact hold today, because a rename's destination must pass ValidatePath and
	// so sits at most MaxPathLen/2 levels down. The visited set is used anyway
	// because that argument lives in another check and is easy to invalidate:
	// ValidatePath measures only the moved entry's own path, never the paths
	// beneath it, so subtree moves already stack depth past what mkdir allows,
	// and a constant justified by a second function's cap silently becomes a
	// refusal of valid renames if that cap moves. This stop needs no such
	// argument.
	seen := make(map[uuid.UUID]struct{})
	for {
		// Compare decoded ids, not handle strings. One inode has many handle
		// spellings — DecodeFileHandle splits on the first colon and accepts any
		// spelling uuid.Parse does — and dstDir arrives from the client while
		// srcHandle comes from the store, so a client that re-cases the UUID it
		// was handed walks straight past a string comparison and closes the loop
		// this function exists to refuse.
		_, id, err := DecodeFileHandle(handle)
		if err != nil {
			return err
		}
		if id == srcID {
			return &StoreError{
				Code:    ErrInvalidArgument,
				Message: "cannot move a directory into itself or one of its own descendants",
			}
		}
		if _, repeat := seen[id]; repeat {
			return &StoreError{
				Code:    ErrIOError,
				Message: "directory parent chain already contains a cycle",
			}
		}
		seen[id] = struct{}{}

		parent, err := tx.GetParent(ctx, handle)
		if IsNotFoundError(err) {
			// Reached the share root without meeting the source, so the
			// destination does not sit below it.
			return nil
		}
		if err != nil {
			return err
		}
		handle = parent
	}
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
	return store.UpdateAttrs(ctx.Context, file)
}
