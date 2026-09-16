// Handler-level coverage for a ChangeTime frozen with the -1 sentinel
// surviving the attribute-setting operations that are not timestamp sets:
// FSCTL_SET_SPARSE, FSCTL_SET_COMPRESSION, FSCTL_SET_ZERO_DATA, SET_INFO
// FileFullEaInformation and SET_INFO SecurityInformation.
//
// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation") a frozen timestamp must not be
// updated by later operations on the handle. Each of these changes something
// about the file without naming a ChangeTime, so the metadata layer would
// otherwise stamp ChangeTime = now for it.
//
// Each test asserts on the ChangeTime a client reads back straight after the
// operation, and names that operation in the failure, because the value the
// store holds between the operation and the next one is what every other reader
// sees.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// assertCtimeUnmoved reads the file back and reports the ChangeTime a client
// would observe now, failing if op moved it off the frozen instant.
func assertCtimeUnmoved(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after %s: %v", op, err)
	}
	if !file.Ctime.Equal(frozen) {
		t.Errorf("ChangeTime = %v after %s; want the frozen %v",
			file.Ctime.UTC(), op, frozen)
	}
}

// TestFrozenChangeTime_SurvivesSetSparse pins FSCTL_SET_SPARSE. It flips a mode
// bit, which is a metadata change the layer stamps ChangeTime for.
func TestFrozenChangeTime_SurvivesSetSparse(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	body := buildIoctlRequestBody(FsctlSetSparse, fileID, []byte{1}, 0)
	res, err := h.handleSetSparse(smbCtx, body)
	if err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetSparse: err=%v status=0x%08x", err, uint32(res.Status))
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "FSCTL_SET_SPARSE")
}

// TestFrozenChangeTime_SurvivesSetCompression pins FSCTL_SET_COMPRESSION, the
// same mode-bit flip one FSCTL over.
func TestFrozenChangeTime_SurvivesSetCompression(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	// COMPRESSION_FORMAT_LZNT1: sets the bit, so the mode really changes.
	body := buildIoctlRequestBody(FsctlSetCompression, fileID, []byte{0x02, 0x00}, 0)
	res, err := h.handleSetCompression(smbCtx, body)
	if err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetCompression: err=%v status=0x%08x", err, uint32(res.Status))
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "FSCTL_SET_COMPRESSION")
}

// TestFrozenChangeTime_SurvivesSetInfoFullEa pins SET_INFO
// FileFullEaInformation. An EA write changes the file's metadata without
// naming a timestamp.
func TestFrozenChangeTime_SurvivesSetInfoFullEa(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	buf := encodeOneEAEntry("NewEA", []byte("testme"))
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileFullEaInformation, buf)
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("SET_INFO EA: err=%v status=%v", err, resp.GetStatus())
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "SET_INFO FileFullEaInformation")
}

// TestFrozenChangeTime_SurvivesSetInfoSecurity pins SET_INFO
// SecurityInformation. Installing a DACL is a metadata change like any other.
func TestFrozenChangeTime_SurvivesSetInfoSecurity(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	// A DACL set is authorized against WRITE_DAC, which grantFullAccess does
	// not carry.
	openFile.DesiredAccess |= uint32(types.WriteDac)
	openFile.GrantedAccess |= uint32(types.WriteDac)
	h.primeAuthContextFromOpenFile(smbCtx, openFile)

	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	resp, err := h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, buildAutoInheritedOnlySD(t))
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("SET_INFO Security: err=%v status=%v", err, resp.GetStatus())
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "SET_INFO SecurityInformation")
}

// TestFrozenChangeTime_SurvivesSetZeroData pins FSCTL_SET_ZERO_DATA. Unlike the
// four above it writes data, so CommitWrite stamps Mtime and ChangeTime from
// inside the write path where no PreserveCtime reaches — the repair is the
// same restore WRITE does.
func TestFrozenChangeTime_SurvivesSetZeroData(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	// SET_ZERO_DATA clamps to the current size, so the file needs bytes before
	// the punch has anything to do.
	if resp, err := h.Write(smbCtx, &WriteRequest{
		FileID: fileID, Offset: 0, Data: make([]byte, 4096),
	}); err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	input := make([]byte, fileZeroDataBufSize)
	// FileOffset 0, BeyondFinalZero 4096, both little-endian uint64.
	input[8] = 0x00
	input[9] = 0x10
	body := buildIoctlRequestBody(FsctlSetZeroData, fileID, input, 0)
	res, err := h.handleSetZeroData(smbCtx, body)
	if err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetZeroData: err=%v status=0x%08x", err, uint32(res.Status))
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "FSCTL_SET_ZERO_DATA")
}

// TestFrozenChangeTime_AttrOpsDoNotRollBackAPeersAdvance guards the other
// direction: holding a ChangeTime must leave the stored value alone, not write
// the frozen one back over an advance another opener made after the freeze.
// NFSv4 derives its change attribute from ChangeTime (RFC 7530 §5.8.1.4), and a
// change attribute that goes backwards lets a client keep a cache it should
// have dropped.
func TestFrozenChangeTime_AttrOpsDoNotRollBackAPeersAdvance(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

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

	body := buildIoctlRequestBody(FsctlSetSparse, fileID, []byte{1}, 0)
	if res, err := h.handleSetSparse(smbCtx, body); err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetSparse: err=%v status=0x%08x", err, uint32(res.Status))
	}

	file, err := metaSvc.GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after FSCTL_SET_SPARSE: %v", err)
	}
	if file.Ctime.Before(advanced) {
		t.Errorf("FSCTL_SET_SPARSE moved ChangeTime back to %v; a peer had advanced it to %v",
			file.Ctime.UTC(), advanced.UTC())
	}
}
