// Handler-level coverage for a ChangeTime frozen with the -1 sentinel surviving
// the attribute-setting operations that are not timestamp sets: FSCTL_SET_SPARSE,
// FSCTL_SET_COMPRESSION, FSCTL_SET_ZERO_DATA, SET_INFO FileFullEaInformation,
// SET_INFO SecurityInformation and SET_INFO FileLinkInformation.
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
)

// openFrozenCtimeHandle returns the fixture's file handle with full access and
// its ChangeTime frozen, plus the frozen instant.
func openFrozenCtimeHandle(t *testing.T) (*Handler, *SMBHandlerContext, *OpenFile, [16]byte, time.Time) {
	t.Helper()
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	return h, smbCtx, openFile, fileID, freezeCtimeOnSeededFile(t, h, smbCtx, openFile)
}

// TestFrozenChangeTime_SurvivesSetSparse pins FSCTL_SET_SPARSE. It flips a mode
// bit, which is a metadata change the layer stamps ChangeTime for.
func TestFrozenChangeTime_SurvivesSetSparse(t *testing.T) {
	h, smbCtx, openFile, fileID, frozen := openFrozenCtimeHandle(t)

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
	h, smbCtx, openFile, fileID, frozen := openFrozenCtimeHandle(t)

	// COMPRESSION_FORMAT_LZNT1: sets the bit, so the mode really changes.
	body := buildIoctlRequestBody(FsctlSetCompression, fileID, []byte{0x02, 0x00}, 0)
	res, err := h.handleSetCompression(smbCtx, body)
	if err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetCompression: err=%v status=0x%08x", err, uint32(res.Status))
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "FSCTL_SET_COMPRESSION")
}

// TestFrozenChangeTime_SurvivesSetInfoFullEa pins SET_INFO
// FileFullEaInformation. An EA write changes the file's metadata without naming
// a timestamp.
func TestFrozenChangeTime_SurvivesSetInfoFullEa(t *testing.T) {
	h, smbCtx, openFile, _, frozen := openFrozenCtimeHandle(t)

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
	h, smbCtx, openFile, _, frozen := openFrozenCtimeHandle(t)

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

// TestFrozenChangeTime_SurvivesSetInfoLink pins SET_INFO FileLinkInformation.
// Adding a name to an inode raises its link count, which CreateHardLink stamps
// a ChangeTime for from inside its own transaction — so unlike the four above,
// this one is beyond the reach of PreserveCtime and is repaired by the restore.
func TestFrozenChangeTime_SurvivesSetInfoLink(t *testing.T) {
	h, smbCtx, openFile, _, frozen := openFrozenCtimeHandle(t)

	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	buf := encodeFileLinkInfoWire(t, false, [8]byte{}, "link2")
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileLinkInformation, buf)
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("SET_INFO Link: err=%v status=%v", err, resp.GetStatus())
	}

	assertCtimeUnmoved(t, h, openFile, frozen, "SET_INFO FileLinkInformation")
}

// TestFrozenChangeTime_SurvivesSetZeroData pins FSCTL_SET_ZERO_DATA. It writes
// data, so CommitWrite stamps Mtime and ChangeTime from inside the write path
// where no PreserveCtime reaches — the repair is the same restore WRITE does.
// The seed write has to precede the freeze because SET_ZERO_DATA clamps to the
// current size, so the file needs bytes before the punch has anything to do.
func TestFrozenChangeTime_SurvivesSetZeroData(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
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

// TestZeroFillRange_ReportsWhetherAnythingCommitted pins the signal the
// frozen-timestamp repair in handleSetZeroData gates on.
//
// restoreFrozenTimestamps writes every frozen timestamp back explicitly, which
// drags a value another opener advanced after the freeze backwards. That is the
// accepted price of undoing a stamp the operation itself made, but a fill that
// committed no chunk made no stamp, and performing the write there would move a
// peer's timestamps backwards on an operation that changed no file state. The
// handler therefore restores only when this reports true.
func TestZeroFillRange_ReportsWhetherAnythingCommitted(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}

	if committed, err := h.zeroFillRange(authCtx, openFile, 0, 4096); err != nil || !committed {
		t.Errorf("a fill that wrote [0,4096) reported committed=%v err=%v; want true, nil", committed, err)
	}

	// Cancelled before the first chunk: the loop's context check fires ahead of
	// any PrepareWrite, so nothing is stamped and nothing is to repair.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	deadCtx := *authCtx
	deadCtx.Context = cancelled
	committed, err := h.zeroFillRange(&deadCtx, openFile, 0, 4096)
	if committed {
		t.Error("a fill cancelled before its first chunk reported committed=true; " +
			"the handler would restore frozen timestamps over a peer's advance for an operation that wrote nothing")
	}
	if err == nil {
		t.Error("a cancelled fill returned a nil error")
	}
}
