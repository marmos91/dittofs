package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// Coverage for #765: the metadata-store TOCTOU create-race recovery branch in
// completeCreateAfterBreak must replay the share-mode + DACL access gates
// against the RACE WINNER before the destructive overwrite/open proceeds.
//
// The first gate invocation at the top of completeCreateAfterBreak runs against
// the draft's pre-race view (fileExists=false / existingFile=nil), so it is a
// no-op for a FILE_CREATED draft. When createNewFile then loses the race with
// ErrAlreadyExists for OVERWRITE_IF / SUPERSEDE, the draft is resynced to the
// winner. Without the replay, a winner carrying an incompatible share-mode or a
// denying DACL would slip past every access check and be silently overwritten.
//
// These tests drive the branch directly: they pre-seed the winner in the store,
// then hand completeCreateAfterBreak a FILE_CREATED draft for the same name so
// createNewFile fails with ErrAlreadyExists and the recovery path runs.

// raceParentDir creates a world-writable subdirectory under the share root so a
// non-root requester passes the parent create-access check inside createNewFile
// (letting the create reach the ErrAlreadyExists race instead of failing early
// on parent permissions).
func raceParentDir(
	t *testing.T,
	metaSvc *metadata.MetadataService,
	rootAuth *metadata.AuthContext,
	rootHandle metadata.FileHandle,
	name string,
) (metadata.FileHandle, *metadata.File) {
	t.Helper()
	dir, _, err := metaSvc.CreateDirectory(rootAuth, rootHandle, name,
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		})
	if err != nil {
		t.Fatalf("CreateDirectory parent: %v", err)
	}
	h, err := metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle parent: %v", err)
	}
	return h, dir
}

// raceWinnerDraft builds a FILE_CREATED draft (pre-race view: no existing file)
// for the given disposition. completeCreateAfterBreak will attempt createNewFile,
// hit the pre-seeded winner, and enter the ErrAlreadyExists recovery branch.
func raceWinnerDraft(
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	baseName string,
	desiredAccess uint32,
	shareAccess uint32,
	disposition types.CreateDisposition,
) *createDraft {
	return &createDraft{
		req: &CreateRequest{
			FileName:          baseName,
			DesiredAccess:     desiredAccess,
			ShareAccess:       shareAccess,
			CreateDisposition: disposition,
			CreateOptions:     0,
		},
		tree:         tree,
		authCtx:      authCtx,
		filename:     baseName,
		baseName:     baseName,
		parentHandle: parentHandle,
		// Pre-race view: the draft believed the target did not exist, so it
		// resolved to FILE_CREATED with no existing file. This is the state
		// Create() hands completeCreateAfterBreak before the metadata-store
		// transaction reveals the concurrent winner.
		existingFile:   nil,
		existingHandle: nil,
		fileExists:     false,
		createAction:   types.FileCreated,
	}
}

// TestCreate_RaceRecovery_SupersedeDeniedByWinnerDACL asserts that a SUPERSEDE
// that loses the create race against a winner whose DACL explicitly denies
// WRITE to the requester is rejected with STATUS_ACCESS_DENIED — and that the
// winner's content is NOT destroyed.
//
// The winner carries an explicit ACE4_ACCESS_DENIED for WRITE_DATA. With the
// #765 replay the SMB DACL gate refuses the destructive open up front. (The
// metadata layer's own write-permission check provides defense-in-depth on this
// path, so the *content* survives either way — the load-bearing discriminator
// for the gate replay itself is the share-mode test below, which exercises a
// pure SMB-handler concern the metadata layer knows nothing about.) This test
// pins the open-time status and the no-truncation guarantee for the DACL arm.
func TestCreate_RaceRecovery_SupersedeDeniedByWinnerDACL(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()
	parentHandle, _ := raceParentDir(t, metaSvc, rootAuth, rootHandle, "race-dacl")

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Pre-seed the winner: owned by the requester (so the POSIX overwrite check
	// passes) but carrying an explicit DENY for WRITE_DATA. The open-time DACL
	// gate must still refuse the destructive open. Seeded with a non-zero size
	// so a successful supersede would be observable as truncation to zero.
	denyWriteACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_WRITE_DATA,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	winner, _, err := metaSvc.CreateFile(rootAuth, parentHandle, "winner.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o700,
			UID:  requesterUID,
			GID:  2001,
			ACL:  denyWriteACL,
		})
	if err != nil {
		t.Fatalf("seed winner: %v", err)
	}
	winnerHandle, err := metadata.EncodeFileHandle(winner)
	if err != nil {
		t.Fatalf("EncodeFileHandle winner: %v", err)
	}
	// CreateFile forces Size=0 (ApplyCreateDefaults); set a non-zero size as
	// root so a successful supersede would be observable as truncation to zero.
	winnerSize := uint64(4096)
	if _, err := metaSvc.SetFileAttributes(rootAuth, winnerHandle, &metadata.SetAttrs{Size: &winnerSize}); err != nil {
		t.Fatalf("seed winner size: %v", err)
	}

	requesterGID := uint32(2001)
	sidStr := requesterSID
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
		BypassTraverseChecking: true, // mirror real SMB sessions
	}

	// SUPERSEDE with attrs-only DesiredAccess. The implied FILE_WRITE_DATA from
	// the destructive disposition is what the DACL must deny.
	const fileReadAttributes uint32 = 0x00000080
	draft := raceWinnerDraft(tree, requesterAuth, parentHandle, "winner.txt",
		fileReadAttributes, 0x07, types.FileSupersede)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("SUPERSEDE losing race vs denying DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}

	// The denied open must not have truncated the winner.
	after, err := metaSvc.GetFile(context.Background(), winnerHandle)
	if err != nil {
		t.Fatalf("GetFile winner after denial: %v", err)
	}
	if after.Size != 4096 {
		t.Fatalf("winner content destroyed despite ACCESS_DENIED: size = %d, expected 4096", after.Size)
	}
}

// TestCreate_RaceRecovery_OverwriteIfDeniedByShareMode asserts that an
// OVERWRITE_IF that loses the create race against a winner already opened with
// a SHARE_READ-only share mode is rejected with STATUS_SHARING_VIOLATION — the
// destructive (write-implying) open conflicts with the existing reader that did
// not grant SHARE_WRITE.
func TestCreate_RaceRecovery_OverwriteIfDeniedByShareMode(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()
	parentHandle, _ := raceParentDir(t, metaSvc, rootAuth, rootHandle, "race-share")

	// Pre-seed a permissive winner (nil DACL → no DACL denial) so the rejection
	// is unambiguously from the share-mode gate, not the DACL gate.
	winner, _, err := metaSvc.CreateFile(rootAuth, parentHandle, "winner.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
		})
	if err != nil {
		t.Fatalf("seed winner: %v", err)
	}
	winnerHandle, err := metadata.EncodeFileHandle(winner)
	if err != nil {
		t.Fatalf("EncodeFileHandle winner: %v", err)
	}

	// Existing open on the winner: READ access, SHARE_READ only (no SHARE_WRITE).
	const (
		fileReadData      uint32 = 0x00000001
		fileShareReadOnly uint32 = 0x00000001
	)
	h.StoreOpenFile(&OpenFile{
		FileID:         h.GenerateFileID(),
		TreeID:         smbCtx.TreeID,
		SessionID:      smbCtx.SessionID,
		Path:           "winner.txt",
		ShareName:      smbCtx.ShareName,
		DesiredAccess:  fileReadData,
		ShareAccess:    fileShareReadOnly,
		MetadataHandle: winnerHandle,
	})

	requesterUID := uint32(0)
	requesterGID := uint32(0)
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
		},
	}

	// OVERWRITE_IF with READ_DATA. The destructive disposition folds in
	// FILE_WRITE_DATA, so the new open implies write and conflicts with the
	// SHARE_READ-only existing holder.
	draft := raceWinnerDraft(tree, requesterAuth, parentHandle, "winner.txt",
		fileReadData, 0x07, types.FileOverwriteIf)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSharingViolation {
		t.Fatalf("OVERWRITE_IF losing race vs SHARE_READ holder: status = 0x%08x, expected STATUS_SHARING_VIOLATION (0x%08x)",
			uint32(resp.Status), uint32(types.StatusSharingViolation))
	}
}

// TestCreate_RaceRecovery_OpenIfMkdirDupStillSucceeds is the regression guard
// for the asserted smbtorture path (smb2.create.mkdir-dup): two parallel
// OPEN_IF on the same directory name must still yield a successful open for the
// loser. The replayed gate runs against a fresh, share-compatible directory
// winner with no denying DACL, so it must be a no-op here.
func TestCreate_RaceRecovery_OpenIfMkdirDupStillSucceeds(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()
	parentHandle, _ := raceParentDir(t, metaSvc, rootAuth, rootHandle, "race-mkdir")

	// Pre-seed the winning directory (the "other" parallel OPEN_IF that won).
	if _, _, err := metaSvc.CreateDirectory(rootAuth, parentHandle, "dup-dir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		}); err != nil {
		t.Fatalf("seed winner dir: %v", err)
	}

	requesterUID := uint32(0)
	requesterGID := uint32(0)
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
		},
	}

	// OPEN_IF directory CREATE that loses the race. DesiredAccess READ_DATA,
	// permissive share mode → resolves to FILE_OPENED on the winner.
	const fileReadData uint32 = 0x00000001
	draft := raceWinnerDraft(tree, requesterAuth, parentHandle, "dup-dir",
		fileReadData, 0x07, types.FileOpenIf)
	draft.isDirectoryRequest = true

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("OPEN_IF mkdir-dup loser: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
	if resp.CreateAction != types.FileOpened {
		t.Fatalf("OPEN_IF mkdir-dup loser: createAction = %d, expected FileOpened (%d)",
			resp.CreateAction, types.FileOpened)
	}
}

// TestCreate_RaceRecovery_OpenIfNonRootInheritedACLSucceeds guards the replay
// against regressing the realistic (non-root) mkdir-dup path. The winner is a
// directory whose ACL was inherited from a parent that grants the requester
// full inheritable access, so the newly-added DACL gate on the OPEN_IF race
// path must still grant the open — the replay must not turn a legitimate loser
// into STATUS_ACCESS_DENIED.
func TestCreate_RaceRecovery_OpenIfNonRootInheritedACLSucceeds(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Parent directory grants the requester full access, inheritable to
	// children (so the winning directory's inherited ACL grants the requester).
	fullInheritACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:  "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_READ_ATTRIBUTES |
					acl.ACE4_WRITE_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_ADD_FILE |
					acl.ACE4_ADD_SUBDIRECTORY | acl.ACE4_SYNCHRONIZE,
				Flag: acl.ACE4_FILE_INHERIT_ACE | acl.ACE4_DIRECTORY_INHERIT_ACE,
			},
		},
	}
	parentDir, _, err := metaSvc.CreateDirectory(rootAuth, rootHandle, "race-inherit",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			ACL:  fullInheritACL,
		})
	if err != nil {
		t.Fatalf("CreateDirectory parent: %v", err)
	}
	parentHandle, err := metadata.EncodeFileHandle(parentDir)
	if err != nil {
		t.Fatalf("EncodeFileHandle parent: %v", err)
	}

	requesterGID := uint32(2001)
	sidStr := requesterSID
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
		// Mirror real SMB sessions, which set this so a parent DACL lacking
		// FILE_TRAVERSE does not block resolution of a child the requester may
		// open (MS-FSA §2.1.5.1.1; handler.go sets it on every SMB AuthContext).
		BypassTraverseChecking: true,
	}

	// Pre-seed the winning directory as the requester so it inherits the
	// parent's ACL granting the requester full access.
	if _, _, err := metaSvc.CreateDirectory(requesterAuth, parentHandle, "dup-dir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		}); err != nil {
		t.Fatalf("seed winner dir: %v", err)
	}

	const fileReadData uint32 = 0x00000001
	draft := raceWinnerDraft(tree, requesterAuth, parentHandle, "dup-dir",
		fileReadData, 0x07, types.FileOpenIf)
	draft.isDirectoryRequest = true

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("non-root OPEN_IF mkdir-dup loser w/ inherited grant: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
	if resp.CreateAction != types.FileOpened {
		t.Fatalf("non-root OPEN_IF mkdir-dup loser: createAction = %d, expected FileOpened (%d)",
			resp.CreateAction, types.FileOpened)
	}
}
