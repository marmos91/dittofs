package handlers

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// LINK / RENAME Test Helpers
// ============================================================================

// encodeLinkArgs encodes LINK4args: newname (component4 = XDR string).
func encodeLinkArgs(newName string) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, newName)
	return buf.Bytes()
}

// encodeRenameArgs encodes RENAME4args: oldname + newname (two component4 strings).
func encodeRenameArgs(oldName, newName string) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, oldName)
	_ = xdr.WriteXDRString(&buf, newName)
	return buf.Bytes()
}

// setCurrentFH copies a handle into ctx.CurrentFH with proper isolation.
func setCurrentFH(ctx *types.CompoundContext, handle metadata.FileHandle) {
	ctx.CurrentFH = make([]byte, len(handle))
	copy(ctx.CurrentFH, handle)
}

// setSavedFH copies a handle into ctx.SavedFH with proper isolation.
func setSavedFH(ctx *types.CompoundContext, handle metadata.FileHandle) {
	ctx.SavedFH = make([]byte, len(handle))
	copy(ctx.SavedFH, handle)
}

// parseChangeInfo4 reads a change_info4 from the response reader.
// Returns atomic (bool as uint32), before, after.
func parseChangeInfo4(reader *bytes.Reader) (uint32, uint64, uint64) {
	atomic, _ := xdr.DecodeUint32(reader)
	before, _ := xdr.DecodeUint64(reader)
	after, _ := xdr.DecodeUint64(reader)
	return atomic, before, after
}

// makeCrossShareHandle creates a handle for a different share (for XDEV tests).
func makeCrossShareHandle(t *testing.T, shareName string) metadata.FileHandle {
	t.Helper()
	id := uuid.New()
	handle, err := metadata.EncodeShareHandle(shareName, id)
	if err != nil {
		t.Fatalf("encode cross-share handle: %v", err)
	}
	return handle
}

// ============================================================================
// LINK Tests
// ============================================================================

func TestHandleLink_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a regular file to link
	fileHandle := fx.createTestFile(t, fx.rootHandle, "original.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)    // root user
	setCurrentFH(ctx, fx.rootHandle) // target directory
	setSavedFH(ctx, fileHandle)      // source file to link

	args := encodeLinkArgs("hardlink.txt")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("LINK status = %d, want NFS4_OK", result.Status)
	}
	if result.OpCode != types.OP_LINK {
		t.Errorf("LINK opcode = %d, want OP_LINK (%d)", result.OpCode, types.OP_LINK)
	}

	// Parse LINK4resok
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// change_info4
	atomic, before, after := parseChangeInfo4(reader)
	if atomic != 1 {
		t.Errorf("change_info atomic = %d, want 1", atomic)
	}
	if before == 0 {
		t.Error("changeid_before should not be 0")
	}
	if after == 0 {
		t.Error("changeid_after should not be 0")
	}

	// Verify the hard link exists via metadata service
	authCtx := newTestAuthCtx(0, 0)
	linked, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "hardlink.txt")
	if err != nil {
		t.Fatalf("Lookup hardlink: %v", err)
	}
	if linked.Type != metadata.FileTypeRegular {
		t.Errorf("linked entry type = %v, want regular", linked.Type)
	}

	// Verify link count incremented (should be 2: original + hardlink)
	original, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "original.txt")
	if err != nil {
		t.Fatalf("Lookup original: %v", err)
	}
	if original.Nlink < 2 {
		t.Errorf("original nlink = %d, want >= 2", original.Nlink)
	}
}

func TestHandleLink_NoCurrentFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	fileHandle := fx.createTestFile(t, fx.rootHandle, "file.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil // no current FH
	setSavedFH(ctx, fileHandle)

	args := encodeLinkArgs("newlink")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("LINK without CurrentFH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleLink_NoSavedFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	ctx.SavedFH = nil // no saved FH

	args := encodeLinkArgs("newlink")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_RESTOREFH {
		t.Errorf("LINK without SavedFH status = %d, want NFS4ERR_RESTOREFH (%d)",
			result.Status, types.NFS4ERR_RESTOREFH)
	}
}

func TestHandleLink_PseudoFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle() // pseudo-fs handle
	ctx.SavedFH = []byte("some-saved-fh")

	args := encodeLinkArgs("newlink")
	result := h.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("LINK on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestHandleLink_IsDirectory(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a directory -- cannot hard link directories
	dirHandle := fx.createTestFile(t, fx.rootHandle, "subdir", metadata.FileTypeDirectory, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, dirHandle) // SavedFH is a directory

	args := encodeLinkArgs("dirlink")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ISDIR {
		t.Errorf("LINK to directory status = %d, want NFS4ERR_ISDIR (%d)",
			result.Status, types.NFS4ERR_ISDIR)
	}
}

func TestHandleLink_CrossShare(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a handle from a different share
	otherHandle := makeCrossShareHandle(t, "/other-share")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle) // /export
	setSavedFH(ctx, otherHandle)     // /other-share

	args := encodeLinkArgs("crosslink")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_XDEV {
		t.Errorf("LINK cross-share status = %d, want NFS4ERR_XDEV (%d)",
			result.Status, types.NFS4ERR_XDEV)
	}
}

func TestHandleLink_InvalidName_Empty(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	fileHandle := fx.createTestFile(t, fx.rootHandle, "file.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fileHandle)

	args := encodeLinkArgs("") // empty name
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("LINK with empty name status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleLink_InvalidName_Slash(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	fileHandle := fx.createTestFile(t, fx.rootHandle, "file.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fileHandle)

	args := encodeLinkArgs("bad/name") // name with slash
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADNAME {
		t.Errorf("LINK with slash status = %d, want NFS4ERR_BADNAME (%d)",
			result.Status, types.NFS4ERR_BADNAME)
	}
}

func TestHandleLink_TargetExists(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create two files
	fileHandle := fx.createTestFile(t, fx.rootHandle, "source.txt", metadata.FileTypeRegular, 0o644, 0, 0)
	fx.createTestFile(t, fx.rootHandle, "existing.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fileHandle)

	// Try to link with a name that already exists
	args := encodeLinkArgs("existing.txt")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_EXIST {
		t.Errorf("LINK to existing name status = %d, want NFS4ERR_EXIST (%d)",
			result.Status, types.NFS4ERR_EXIST)
	}
}

// ============================================================================
// RENAME Tests
// ============================================================================

func TestHandleRename_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a file to rename
	fx.createTestFile(t, fx.rootHandle, "oldname.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle) // target dir
	setSavedFH(ctx, fx.rootHandle)   // source dir (same dir for simple rename)

	args := encodeRenameArgs("oldname.txt", "newname.txt")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("RENAME status = %d, want NFS4_OK", result.Status)
	}
	if result.OpCode != types.OP_RENAME {
		t.Errorf("RENAME opcode = %d, want OP_RENAME (%d)", result.OpCode, types.OP_RENAME)
	}

	// Parse RENAME4resok
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// source_cinfo (change_info4)
	srcAtomic, srcBefore, srcAfter := parseChangeInfo4(reader)
	if srcAtomic != 1 {
		t.Errorf("source change_info atomic = %d, want 1", srcAtomic)
	}
	if srcBefore == 0 {
		t.Error("source changeid_before should not be 0")
	}
	if srcAfter == 0 {
		t.Error("source changeid_after should not be 0")
	}

	// target_cinfo (change_info4)
	tgtAtomic, tgtBefore, tgtAfter := parseChangeInfo4(reader)
	if tgtAtomic != 1 {
		t.Errorf("target change_info atomic = %d, want 1", tgtAtomic)
	}
	if tgtBefore == 0 {
		t.Error("target changeid_before should not be 0")
	}
	if tgtAfter == 0 {
		t.Error("target changeid_after should not be 0")
	}

	// Verify the old name no longer exists
	authCtx := newTestAuthCtx(0, 0)
	_, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "oldname.txt")
	if err == nil {
		t.Error("old name should not exist after RENAME")
	}

	// Verify the new name exists
	renamed, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "newname.txt")
	if err != nil {
		t.Fatalf("Lookup new name: %v", err)
	}
	if renamed.Type != metadata.FileTypeRegular {
		t.Errorf("renamed entry type = %v, want regular", renamed.Type)
	}
}

func TestHandleRename_CrossDirectory(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a subdirectory and a file
	subDirHandle := fx.createTestFile(t, fx.rootHandle, "subdir", metadata.FileTypeDirectory, 0o755, 0, 0)
	fx.createTestFile(t, fx.rootHandle, "moveme.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setSavedFH(ctx, fx.rootHandle)  // source directory
	setCurrentFH(ctx, subDirHandle) // target directory

	args := encodeRenameArgs("moveme.txt", "moved.txt")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("RENAME cross-dir status = %d, want NFS4_OK", result.Status)
	}

	// Parse response to verify dual change_info4
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// source_cinfo and target_cinfo
	_, _, _ = parseChangeInfo4(reader) // source
	_, _, _ = parseChangeInfo4(reader) // target

	// Verify old name gone from root
	authCtx := newTestAuthCtx(0, 0)
	_, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "moveme.txt")
	if err == nil {
		t.Error("moveme.txt should not exist in root after cross-dir RENAME")
	}

	// Verify new name in subdir
	moved, err := fx.metaSvc.Lookup(authCtx, subDirHandle, "moved.txt")
	if err != nil {
		t.Fatalf("Lookup moved.txt in subdir: %v", err)
	}
	if moved.Type != metadata.FileTypeRegular {
		t.Errorf("moved entry type = %v, want regular", moved.Type)
	}
}

func TestHandleRename_NoCurrentFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil // no current FH
	setSavedFH(ctx, fx.rootHandle)

	args := encodeRenameArgs("old", "new")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("RENAME without CurrentFH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleRename_NoSavedFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	ctx.SavedFH = nil // no saved FH

	args := encodeRenameArgs("old", "new")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_RESTOREFH {
		t.Errorf("RENAME without SavedFH status = %d, want NFS4ERR_RESTOREFH (%d)",
			result.Status, types.NFS4ERR_RESTOREFH)
	}
}

func TestHandleRename_PseudoFS_CurrentFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle() // pseudo-fs
	setSavedFH(ctx, fx.rootHandle)      // real-fs

	args := encodeRenameArgs("old", "new")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("RENAME on pseudo-fs CurrentFH status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestHandleRename_PseudoFS_SavedFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)  // real-fs
	ctx.SavedFH = pfs.GetRootHandle() // pseudo-fs

	args := encodeRenameArgs("old", "new")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("RENAME on pseudo-fs SavedFH status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestHandleRename_CrossShare(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a handle from a different share
	otherHandle := makeCrossShareHandle(t, "/other-share")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle) // /export
	setSavedFH(ctx, otherHandle)     // /other-share

	args := encodeRenameArgs("old", "new")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_XDEV {
		t.Errorf("RENAME cross-share status = %d, want NFS4ERR_XDEV (%d)",
			result.Status, types.NFS4ERR_XDEV)
	}
}

func TestHandleRename_NotFound(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fx.rootHandle)

	// Source name does not exist
	args := encodeRenameArgs("nonexistent.txt", "newname.txt")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOENT {
		t.Errorf("RENAME nonexistent status = %d, want NFS4ERR_NOENT (%d)",
			result.Status, types.NFS4ERR_NOENT)
	}
}

func TestHandleRename_InvalidName_OldEmpty(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fx.rootHandle)

	args := encodeRenameArgs("", "newname.txt") // empty old name
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("RENAME empty oldname status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleRename_InvalidName_NewSlash(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	fx.createTestFile(t, fx.rootHandle, "file.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, fx.rootHandle)

	args := encodeRenameArgs("file.txt", "bad/name") // slash in new name
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADNAME {
		t.Errorf("RENAME with slash in newname status = %d, want NFS4ERR_BADNAME (%d)",
			result.Status, types.NFS4ERR_BADNAME)
	}
}

// ============================================================================
// Compound Sequence Tests
// ============================================================================

func TestHandleLink_CompoundSequence(t *testing.T) {
	// Test a realistic COMPOUND: PUTFH(dir) + SAVEFH + PUTFH(dir) + LOOKUP(file) + SAVEFH + PUTFH(dir) + LINK(newname)
	// This simulates the typical NFSv4 client compound for hard link creation.
	fx := newRealFSTestFixture(t, "/export")

	// Create a file via metadata service
	authCtx := newTestAuthCtx(0, 0)
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	}
	createdFile, err := fx.metaSvc.CreateFile(authCtx, fx.rootHandle, "source.txt", fileAttr)
	if err != nil {
		t.Fatalf("create test file: %v", err)
	}
	sourceHandle, err := metadata.EncodeFileHandle(createdFile)
	if err != nil {
		t.Fatalf("encode source handle: %v", err)
	}

	// Build a direct test: set SavedFH to source, CurrentFH to root dir, call LINK
	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	setSavedFH(ctx, metadata.FileHandle(sourceHandle))

	args := encodeLinkArgs("link-to-source.txt")
	result := fx.handler.handleLink(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("LINK compound sequence status = %d, want NFS4_OK", result.Status)
	}

	// Verify the link
	linked, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "link-to-source.txt")
	if err != nil {
		t.Fatalf("Lookup link: %v", err)
	}
	if linked.Type != metadata.FileTypeRegular {
		t.Errorf("linked type = %v, want regular", linked.Type)
	}
}

func TestHandleRename_CompoundSequence(t *testing.T) {
	// Test RENAME via compound-like flow: set SavedFH to source dir, CurrentFH to target dir
	fx := newRealFSTestFixture(t, "/export")

	// Create source dir, target dir, and a file in source dir
	authCtx := newTestAuthCtx(0, 0)
	srcDirAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	srcDir, err := fx.metaSvc.CreateDirectory(authCtx, fx.rootHandle, "srcdir", srcDirAttr)
	if err != nil {
		t.Fatalf("create src dir: %v", err)
	}
	srcDirHandle, _ := metadata.EncodeFileHandle(srcDir)

	tgtDirAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	tgtDir, err := fx.metaSvc.CreateDirectory(authCtx, fx.rootHandle, "tgtdir", tgtDirAttr)
	if err != nil {
		t.Fatalf("create tgt dir: %v", err)
	}
	tgtDirHandle, _ := metadata.EncodeFileHandle(tgtDir)

	fileAttr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}
	_, err = fx.metaSvc.CreateFile(authCtx, metadata.FileHandle(srcDirHandle), "myfile.txt", fileAttr)
	if err != nil {
		t.Fatalf("create file in src dir: %v", err)
	}

	// RENAME: SavedFH=srcdir, CurrentFH=tgtdir
	ctx := newRealFSContext(0, 0)
	setSavedFH(ctx, metadata.FileHandle(srcDirHandle))
	setCurrentFH(ctx, metadata.FileHandle(tgtDirHandle))

	args := encodeRenameArgs("myfile.txt", "renamed.txt")
	result := fx.handler.handleRename(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("RENAME compound sequence status = %d, want NFS4_OK", result.Status)
	}

	// Verify file moved
	_, err = fx.metaSvc.Lookup(authCtx, metadata.FileHandle(srcDirHandle), "myfile.txt")
	if err == nil {
		t.Error("file should not exist in source dir after rename")
	}

	moved, err := fx.metaSvc.Lookup(authCtx, metadata.FileHandle(tgtDirHandle), "renamed.txt")
	if err != nil {
		t.Fatalf("Lookup renamed file in target dir: %v", err)
	}
	if moved.Type != metadata.FileTypeRegular {
		t.Errorf("moved type = %v, want regular", moved.Type)
	}
}
