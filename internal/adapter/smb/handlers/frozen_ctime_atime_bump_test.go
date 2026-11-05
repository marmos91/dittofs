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

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/pkg/metadata"
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

// grantFullAccess gives the handle the rights READ/WRITE, the
// explicit-timestamp SET_INFO path, and the EA path require.
func grantFullAccess(h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) {
	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileWriteEA | types.FileReadEA |
		types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	openFile.DesiredAccess = access
	openFile.GrantedAccess = access
	h.primeAuthContextFromOpenFile(smbCtx, openFile)
}

// assertCtimeUnmoved reads the file back and reports the ChangeTime a client
// would observe now, failing if op moved it off the frozen instant. Returns the
// file so callers can layer further assertions on the same read.
func assertCtimeUnmoved(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) *metadata.File {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after %s: %v", op, err)
	}
	if !file.Ctime.Equal(frozen) {
		t.Errorf("ChangeTime = %v after %s; want the frozen %v", file.Ctime.UTC(), op, frozen)
	}
	return file
}

// assertFrozenCtimeSurvived reads the file back from the store and checks that
// op left the frozen ChangeTime alone while still landing its LastAccessTime
// bump.
func assertFrozenCtimeSurvived(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) {
	t.Helper()
	file := assertCtimeUnmoved(t, h, openFile, frozen, op)
	if !file.Atime.After(frozen) {
		t.Errorf("LastAccessTime = %v after %s; the bump should still have landed", file.Atime.UTC(), op)
	}
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
