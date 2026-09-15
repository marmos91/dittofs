// Handler-level coverage for a ChangeTime frozen with the -1 sentinel
// surviving the LastAccessTime bump that READ and WRITE make afterwards.
//
// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation") a frozen timestamp must not be
// updated by later operations on the handle. The access-time bump is an
// attribute write like any other, so the metadata layer stamps ChangeTime = now
// for it unless the caller names a ChangeTime of its own. These tests assert on
// the stored value right after the operation, not after CLOSE: a frozen
// ChangeTime that is wrong in the store until CLOSE repairs it is visible to
// every other reader in between, and any concurrent read-modify-write of the
// row makes it permanent.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// freezeCtimeOnSeededFile hands the fixture's file to the session's user, pins
// all four timestamps to a fixed past instant, and freezes ChangeTime with the
// -1 sentinel. Returns the frozen instant.
func freezeCtimeOnSeededFile(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) time.Time {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()

	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	sessUID, sessGID := uint32(1000), uint32(1000)
	past := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(rootCtx, openFile.MetadataHandle, &metadata.SetAttrs{
		UID: &sessUID, GID: &sessGID,
		CreationTime: &past, Atime: &past, Mtime: &past, Ctime: &past,
	}); err != nil {
		t.Fatalf("seed owner + timestamps: %v", err)
	}

	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	// Only ChangeTime carries the sentinel; the other three stay zero ("do not
	// change"), which is the case where the metadata layer's automatic stamp
	// would otherwise fire.
	freeze := encodeBasicInfo(0, 0, 0, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileBasicInformation, freeze)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v resp=%v", err, resp)
	}
	if !openFile.CtimeFrozen {
		t.Fatal("sentinel did not freeze ChangeTime")
	}
	return past
}

// grantFullAccess gives the handle the rights READ/WRITE and the
// explicit-timestamp SET_INFO path require.
func grantFullAccess(h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) {
	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	openFile.DesiredAccess = access
	openFile.GrantedAccess = access
	h.primeAuthContextFromOpenFile(smbCtx, openFile)
}

// assertFrozenCtimeSurvived reads the file back from the store and checks that
// op left the frozen ChangeTime alone while still landing its LastAccessTime
// bump.
func assertFrozenCtimeSurvived(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after %s: %v", op, err)
	}
	if !file.Ctime.Equal(frozen) {
		t.Errorf("ChangeTime = %v after %s; want the frozen %v", file.Ctime.UTC(), op, frozen)
	}
	if !file.Atime.After(frozen) {
		t.Errorf("LastAccessTime = %v after %s; the bump should still have landed", file.Atime.UTC(), op)
	}
}

func TestFrozenChangeTime_SurvivesWriteAtimeBump(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Write: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, openFile, frozen, "WRITE")
}

func TestFrozenChangeTime_SurvivesReadAtimeBump(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	// Give the file something to read back.
	if resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)}); err != nil ||
		resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}
	// That write already bumped LastAccessTime, and successive bumps on one
	// handle coalesce for smbAtimeUpdateWindow. Clear the mark so the READ
	// below performs the store write this test is about.
	openFile.SmbAtimeWrittenAt = time.Time{}

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	resp, err := h.Read(smbCtx, &ReadRequest{FileID: fileID, Offset: 0, Length: 4096})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Read: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, openFile, frozen, "READ")
}

func TestFrozenChangeTime_SurvivesQueryDirectoryAtimeBump(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	fileOpen, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}

	// A directory handle on the share root, alongside the fixture's file handle
	// so the enumeration has an entry to return.
	dirID := [16]byte{2}
	dirOpen := (&OpenFile{
		FileID: dirID, TreeID: fileOpen.TreeID, SessionID: fileOpen.SessionID,
		ShareName: fileOpen.ShareName, MetadataHandle: rootHandle, IsDirectory: true,
	}).WithName(OpenName{Path: "/", FileName: "/"})
	h.StoreOpenFile(dirOpen)
	grantFullAccess(h, smbCtx, dirOpen)

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, dirOpen)

	resp, err := h.QueryDirectory(smbCtx, &QueryDirectoryRequest{
		FileID: dirID, FileInfoClass: uint8(types.FileBothDirectoryInformation),
		FileName: "*", OutputBufferLength: 65536, Flags: uint8(types.SMB2RestartScans),
	})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("QueryDirectory: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, dirOpen, frozen, "QUERY_DIRECTORY")
}

// A freeze binds the handle that set it, not the file. When another opener
// legitimately advances ChangeTime after the freeze, the frozen handle's next
// access-time bump must leave that newer value alone — not restore the value it
// froze. NFSv4 encodes its change attribute from Ctime (RFC 7530 §5.8.1.4
// requires it to increase), so dragging the stored value backwards lets a
// client keep serving a cache it should have dropped.
func TestFrozenChangeTime_DoesNotRollBackAPeersAdvance(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	if resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)}); err != nil ||
		resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}
	openFile.SmbAtimeWrittenAt = time.Time{}

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	// Another opener advances ChangeTime after the freeze.
	metaSvc := h.Registry.GetMetadataService()
	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	advanced := frozen.Add(72 * time.Hour)
	if _, err := metaSvc.SetFileAttributes(rootCtx, openFile.MetadataHandle, &metadata.SetAttrs{
		Ctime: &advanced,
	}); err != nil {
		t.Fatalf("peer ChangeTime advance: %v", err)
	}

	resp, err := h.Read(smbCtx, &ReadRequest{FileID: fileID, Offset: 0, Length: 4096})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Read: err=%v status=%v", err, resp.GetStatus())
	}

	file, err := metaSvc.GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after READ: %v", err)
	}
	if file.Ctime.Before(advanced) {
		t.Errorf("READ moved ChangeTime backwards to %v; the peer had advanced it to %v",
			file.Ctime.UTC(), advanced.UTC())
	}
}
