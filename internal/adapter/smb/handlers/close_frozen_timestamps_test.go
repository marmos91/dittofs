// Handler-level coverage for frozen timestamps surviving a rename and the
// CLOSE that follows it.
//
// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation") a timestamp frozen with the
// -1 sentinel must not be auto-updated by later operations on that handle.
// CLOSE is where that is hardest to hold: it flushes the deferred write state,
// which stamps Mtime/Ctime, and only then restores the frozen values. The
// rename in between stamps ChangeTime as well.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestClose_FrozenTimestampsSurviveRename drives the whole flow through the
// protocol handlers rather than seeding the freeze flags directly: the freeze
// and the restore are gated by the same ownership rule in the metadata layer,
// so a handle that could not have frozen a timestamp is not a case this can
// reach. The file is chowned to the session's user first for that reason.
func TestClose_FrozenTimestampsSurviveRename(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	metaSvc := h.Registry.GetMetadataService()

	// The fixture creates the file as root but authenticates the session as
	// uid 1000. Hand the file to that user so it can freeze its timestamps at
	// all — SET_INFO's explicit-timestamp write requires ownership.
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

	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	openFile.DesiredAccess = access
	openFile.GrantedAccess = access

	h.primeAuthContextFromOpenFile(smbCtx, openFile)
	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}

	// Freeze all four timestamps with the -1 sentinel.
	freeze := encodeBasicInfo(filetimeFreeze, filetimeFreeze, filetimeFreeze, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileBasicInformation, freeze)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v resp=%v", err, resp)
	}
	if !openFile.BtimeFrozen || !openFile.AtimeFrozen || !openFile.MtimeFrozen || !openFile.CtimeFrozen {
		t.Fatalf("sentinel did not freeze all four fields: btime=%v atime=%v mtime=%v ctime=%v",
			openFile.BtimeFrozen, openFile.AtimeFrozen, openFile.MtimeFrozen, openFile.CtimeFrozen)
	}

	// Rename, which stamps ChangeTime on the renamed inode.
	resp, err = h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileRenameInformation, encodeRenameInfo("link2"))
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("rename SET_INFO: err=%v resp=%v", err, resp)
	}

	// CLOSE must succeed, or the assertions below prove nothing about what
	// CLOSE does — it would only prove CLOSE bailed out early.
	cresp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cresp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Close status = %v, want STATUS_SUCCESS", cresp.GetStatus())
	}

	final, err := metaSvc.GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after close: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"CreationTime", final.CreationTime},
		{"LastAccessTime", final.Atime},
		{"LastWriteTime", final.Mtime},
		{"ChangeTime", final.Ctime},
	} {
		if !tc.got.Equal(past) {
			t.Errorf("%s = %v after rename + CLOSE; want the frozen %v", tc.name, tc.got.UTC(), past)
		}
	}
}
