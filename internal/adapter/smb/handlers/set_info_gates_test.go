package handlers

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Handler-level coverage for the SET_INFO / READ / WRITE access-gate and
// position-tracking fixes. Each test drives the gate at its call site and
// asserts the on-wire status:
//
//   - FileAllocationInformation with a short buffer → STATUS_INFO_LENGTH_MISMATCH
//     (per MS-FSA 2.1.5.15.1 InputBufferSize rule), not a silent Success.
//   - FileFullEaInformation without FILE_WRITE_EA → STATUS_ACCESS_DENIED
//     (per MS-FSA 2.1.5.15.6).
//   - FileEndOfFileInformation without FILE_WRITE_DATA → STATUS_ACCESS_DENIED
//     (per MS-FSA 2.1.5.15.5).
//   - FileRenameInformation with a non-zero RootDirectory →
//     STATUS_INVALID_PARAMETER (no silent same-dir fallback).
//   - WRITE with a nonzero RDMA Channel → STATUS_INVALID_PARAMETER
//     (per MS-SMB2 3.3.5.13, no RDMA support).
//   - DecodeWriteRequest fails when the payload is not where DataOffset says.
//
// The SET_INFO tests share a helper that registers a real open file on a
// Handler backed by an in-memory metadata store, so the gates and the
// position/mode writes run against the same OpenFile the QUERY_INFO reader
// observes.

const (
	gateFileWriteData uint32 = 0x00000002
	gateFileWriteEA   uint32 = 0x00000010
	gateFileReadData  uint32 = 0x00000001
	gateFileWriteAttr uint32 = 0x00000100
	gateDeleteAccess  uint32 = 0x00010000
	gateFileReadAttr  uint32 = 0x00000080
)

// setupSetInfoGateTest stands up a Handler + runtime with an in-memory
// metadata store and a memory block store, creates a file under the share root,
// and registers an OpenFile for it with the given GrantedAccess. Returns the
// handler, the file ID, the open, and the session/tree the gates validate
// against.
func setupSetInfoGateTest(t *testing.T, grantedAccess uint32) (*Handler, [16]byte, *OpenFile, uint64, uint32) {
	t.Helper()
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := newTestRuntime(t, cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "gatemeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("gatemeta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// WRITE resolves the share's block store to prepare/commit payloads.
	blockStoreID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "gatebs", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/setinfo-gate-test"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "gatemeta",
		Enabled:       true,
		BlockStoreID:  blockStoreID,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	auth := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}

	metaSvc := rt.GetMetadataService()
	file, _, err := metaSvc.CreateFile(auth, rootHandle, "gate-target.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o666,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	encHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	// WRITE/SET_INFO validate session and tree: mint a real session and store a
	// tree connection so the ownership gates answer "belongs".
	sess := h.CreateSession("127.0.0.1:1", false, "gate-user", "")
	sessionID := sess.SessionID
	const treeID = uint32(1)
	h.StoreTree(&TreeConnection{
		TreeID:     treeID,
		SessionID:  sessionID,
		ShareName:  shareName,
		Permission: models.PermissionReadWrite,
	})

	fileID := h.GenerateFileID()
	open := (&OpenFile{
		FileID:         fileID,
		TreeID:         treeID,
		SessionID:      sessionID,
		ShareName:      shareName,
		DesiredAccess:  grantedAccess,
		GrantedAccess:  grantedAccess,
		MetadataHandle: encHandle,
	}).WithName(OpenName{
		Path:         "/gate-target.txt",
		FileName:     "gate-target.txt",
		ParentHandle: rootHandle,
	})
	h.StoreOpenFile(open)
	return h, fileID, open, sessionID, treeID
}

// gateContext builds a handler context bound to the test's session and tree.
func gateContext(sessionID uint64, treeID uint32) *SMBHandlerContext {
	return &SMBHandlerContext{Context: context.Background(), SessionID: sessionID, TreeID: treeID}
}

// gateSetInfo drives a SET_INFO file-class request and returns the status.
func gateSetInfo(t *testing.T, ctx *SMBHandlerContext, h *Handler, fileID [16]byte, class types.FileInfoClass, buffer []byte) types.Status {
	t.Helper()
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(class),
		Buffer:        buffer,
		FileID:        fileID,
	})
	if err != nil {
		t.Fatalf("SetInfo(%v): %v", class, err)
	}
	return resp.GetStatus()
}

func TestSetInfo_ShortAllocationBuffer_InfoLengthMismatch(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)
	// Per MS-FSA 2.1.5.15.1: InputBufferSize < 8 → STATUS_INFO_LENGTH_MISMATCH.
	// A nil buffer must not fall through to Success.
	if got := gateSetInfo(t, gateContext(sessionID, treeID), h, fileID, types.FileAllocationInformation, nil); got != types.StatusInfoLengthMismatch {
		t.Fatalf("nil allocation buffer: status = 0x%08x, want STATUS_INFO_LENGTH_MISMATCH (0x%08x)",
			uint32(got), uint32(types.StatusInfoLengthMismatch))
	}
	// A 4-byte buffer is also too short.
	if got := gateSetInfo(t, gateContext(sessionID, treeID), h, fileID, types.FileAllocationInformation, []byte{1, 2, 3, 4}); got != types.StatusInfoLengthMismatch {
		t.Fatalf("4-byte allocation buffer: status = 0x%08x, want STATUS_INFO_LENGTH_MISMATCH",
			uint32(got))
	}
}

func TestSetInfo_EAWithoutFileWriteEA_AccessDenied(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileReadData|gateFileWriteData|gateFileWriteAttr)
	// Handle holds FILE_WRITE_DATA but not FILE_WRITE_EA: EA set must be denied.
	buf := gateEncodeEAEntry("user.test", []byte("v"))
	if got := gateSetInfo(t, gateContext(sessionID, treeID), h, fileID, types.FileFullEaInformation, buf); got != types.StatusAccessDenied {
		t.Fatalf("EA set without FILE_WRITE_EA: status = 0x%08x, want STATUS_ACCESS_DENIED (0x%08x)",
			uint32(got), uint32(types.StatusAccessDenied))
	}
	// A handle with FILE_WRITE_EA but WITHOUT FILE_WRITE_ATTRIBUTES must
	// still pass: the EA class is exempt from the step-1b attributes gate
	// (its own FILE_WRITE_EA check is what governs).
	h2, fileID2, _, sessionID2, treeID2 := setupSetInfoGateTest(t, gateFileReadData|gateFileWriteEA)
	if got := gateSetInfo(t, gateContext(sessionID2, treeID2), h2, fileID2, types.FileFullEaInformation, buf); got != types.StatusSuccess {
		t.Fatalf("EA set with FILE_WRITE_EA only (no FILE_WRITE_ATTRIBUTES): status = 0x%08x, want STATUS_SUCCESS (0x%08x)",
			uint32(got), uint32(types.StatusSuccess))
	}
}

func TestSetInfo_EOFWithoutFileWriteData_AccessDenied(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileReadData|gateFileWriteEA|gateFileWriteAttr)
	// Handle holds no FILE_WRITE_DATA: EOF set must be denied.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, 0)
	if got := gateSetInfo(t, gateContext(sessionID, treeID), h, fileID, types.FileEndOfFileInformation, buf); got != types.StatusAccessDenied {
		t.Fatalf("EOF set without FILE_WRITE_DATA: status = 0x%08x, want STATUS_ACCESS_DENIED (0x%08x)",
			uint32(got), uint32(types.StatusAccessDenied))
	}
}

func TestSetInfo_RenameNonZeroRootDirectory_InvalidParameter(t *testing.T) {
	// Rename requires DELETE access on the source (MS-FSA 2.1.5.15.12), granted
	// here so the RootDirectory check is what fires.
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteEA|gateFileWriteAttr|gateDeleteAccess)
	// RootDirectory non-zero with a handle-relative name: must be rejected,
	// not silently renamed within the same directory.
	buf := gateEncodeRenameInfo(false, 0xDEADBEEF, "\\moved.txt")
	if got := gateSetInfo(t, gateContext(sessionID, treeID), h, fileID, types.FileRenameInformation, buf); got != types.StatusInvalidParameter {
		t.Fatalf("rename with non-zero RootDirectory: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
			uint32(got), uint32(types.StatusInvalidParameter))
	}
}

func TestWrite_RdmaChannel_InvalidParameter(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)
	resp, err := h.Write(gateContext(sessionID, treeID), &WriteRequest{
		FileID:     fileID,
		Offset:     0,
		Length:     4,
		DataOffset: 64 + 48,
		Channel:    1, // SMB2_CHANNEL_RDMA_V1 — unsupported on this transport
		Data:       []byte("data"),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := resp.GetStatus(); got != types.StatusInvalidParameter {
		t.Fatalf("WRITE with RDMA channel: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
			uint32(got), uint32(types.StatusInvalidParameter))
	}
}

func TestDecodeWriteRequest_DataOffsetMismatch_Fails(t *testing.T) {
	// Fixed structure is 48 bytes; payload must sit where DataOffset says.
	// DataOffset claims byte 56 but the body ends at 64, so the claimed region
	// cannot hold the payload — decode must fail instead of speculatively
	// falling back to offset 48.
	body := make([]byte, 48+16)
	w := body
	binary.LittleEndian.PutUint16(w[0:], 49)    // StructureSize
	binary.LittleEndian.PutUint16(w[2:], 64+56) // DataOffset claims body byte 56
	binary.LittleEndian.PutUint32(w[4:], 16)    // Length: 16 bytes needed
	binary.LittleEndian.PutUint64(w[8:], 0)     // Offset
	copy(w[48:], "0123456789abcdef")            // payload at 48, not where DataOffset points
	if _, err := DecodeWriteRequest(body); err == nil {
		t.Fatal("DecodeWriteRequest with payload not at DataOffset: want error, got nil")
	}
}

func TestSetInfo_PositionInfo_ConcurrentSetQuery_RaceFree(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)

	// Drive SET_INFO FilePositionInformation and QUERY_INFO
	// FilePositionInformation concurrently — both touch OpenFile.PositionInfo,
	// which must be read/written under openFile.mu. Run with -race.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 8)
			for n := 0; n < 50; n++ {
				select {
				case <-stop:
					return
				default:
				}
				binary.LittleEndian.PutUint64(buf, uint64(i*1000+n))
				resp, err := h.SetInfo(gateContext(sessionID, treeID), &SetInfoRequest{
					InfoType:      types.SMB2InfoTypeFile,
					FileInfoClass: uint8(types.FilePositionInformation),
					Buffer:        buf,
					FileID:        fileID,
				})
				if err != nil || resp.GetStatus() != types.StatusSuccess {
					t.Errorf("SetInfo(Position): err=%v status=0x%08x", err, uint32(resp.GetStatus()))
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The buffer length matters: QueryInfo refuses a request shorter than the
		// info class with STATUS_INFO_LENGTH_MISMATCH before it reads anything, so
		// a zero-length request never reaches the field this test is racing.
		// FILE_POSITION_INFORMATION is 8 bytes ([MS-FSCC] 2.4.40).
		req := &QueryInfoRequest{
			InfoType:           types.SMB2InfoTypeFile,
			FileInfoClass:      uint8(types.FilePositionInformation),
			FileID:             fileID,
			OutputBufferLength: 8,
		}
		for n := 0; n < 50; n++ {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := h.QueryInfo(gateContext(sessionID, treeID), req)
			if err != nil {
				t.Errorf("QueryInfo(Position): %v", err)
				return
			}
			if resp.GetStatus() != types.StatusSuccess {
				t.Errorf("QueryInfo(Position): status=0x%08x, want STATUS_SUCCESS: "+
					"the query was refused before it read the field this test races",
					uint32(resp.GetStatus()))
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
}

func TestWrite_AdvancesPositionInfo(t *testing.T) {
	h, fileID, open, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)
	resp, err := h.Write(gateContext(sessionID, treeID), &WriteRequest{
		FileID:     fileID,
		Offset:     100,
		Length:     4,
		DataOffset: 64 + 48,
		Data:       []byte("data"),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := resp.GetStatus(); got != types.StatusSuccess {
		t.Fatalf("Write status = 0x%08x, want STATUS_SUCCESS", uint32(got))
	}
	// Per MS-FSA 2.1.5.4: CurrentByteOffset advances to Offset + BytesWritten.
	open.mu.RLock()
	got := open.PositionInfo
	open.mu.RUnlock()
	if got != 104 {
		t.Fatalf("PositionInfo after WRITE = %d, want 104", got)
	}
}

// gateEncodeEAEntry builds a minimal FILE_FULL_EA_INFORMATION buffer with one
// entry (name + value) per [MS-FSCC] 2.4.16: NextEntryOffset (4B LE), Flags
// (1B), NameLength (1B), ValueLength (2B LE), Name, NUL, Value.
func gateEncodeEAEntry(name string, value []byte) []byte {
	total := 8 + len(name) + 1 + len(value)
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[0:], 0)                  // NextEntryOffset: single entry
	buf[4] = 0                                                 // Flags
	buf[5] = byte(len(name))                                   // NameLength (excludes NUL)
	binary.LittleEndian.PutUint16(buf[6:], uint16(len(value))) // ValueLength
	copy(buf[8:], name)
	buf[8+len(name)] = 0
	copy(buf[8+len(name)+1:], value)
	return buf
}

// gateEncodeRenameInfo builds a FILE_RENAME_INFORMATION buffer per
// [MS-FSCC] 2.4.42: ReplaceIfExists (1B), Reserved (7B), RootDirectory (8B),
// FileNameLength (4B), FileName (UTF-16LE).
func gateEncodeRenameInfo(replaceIfExists bool, rootDirectory uint64, fileName string) []byte {
	nameUTF16 := make([]byte, 0, len(fileName)*2)
	for _, r := range fileName {
		nameUTF16 = binary.LittleEndian.AppendUint16(nameUTF16, uint16(r))
	}
	buf := make([]byte, 8+8+4+len(nameUTF16))
	if replaceIfExists {
		buf[0] = 1
	}
	binary.LittleEndian.PutUint64(buf[8:], rootDirectory)
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(nameUTF16)))
	copy(buf[20:], nameUTF16)
	return buf
}

// TestRead_RdmaChannel_InvalidParameter pins the READ half of the RDMA-channel
// gate. WRITE has its own coverage above; without a READ case the branch could
// regress to silently ignoring the requested channel while the write test stays
// green. Per MS-SMB2 3.3.5.12 a channel this transport cannot honor must fail
// rather than be served inline.
func TestRead_RdmaChannel_InvalidParameter(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileReadData|gateFileWriteData)
	resp, err := h.Read(gateContext(sessionID, treeID), &ReadRequest{
		FileID:  fileID,
		Offset:  0,
		Length:  4,
		Channel: 1, // SMB2_CHANNEL_RDMA_V1 — unsupported on this transport
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := resp.GetStatus(); got != types.StatusInvalidParameter {
		t.Fatalf("READ with RDMA channel: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
			uint32(got), uint32(types.StatusInvalidParameter))
	}
}

// TestSetInfo_AllocAndMode_ConcurrentSetQuery_RaceFree pins that the
// SET_INFO writers and the QUERY_INFO readers of RequestedAllocSize and
// CreateOptions agree on openFile.mu. Locking only the writer leaves the field
// racy: the mutex guards the field, not the value the reader already loaded.
// Run with -race.
func TestSetInfo_AllocAndMode_ConcurrentSetQuery_RaceFree(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t,
		gateFileReadData|gateFileWriteData|gateFileWriteAttr|gateFileReadAttr)

	var wg sync.WaitGroup
	// Writers: allocation (8-byte buffer) and mode (4-byte buffer).
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alloc := make([]byte, 8)
			mode := make([]byte, 4)
			for n := 0; n < 50; n++ {
				binary.LittleEndian.PutUint64(alloc, uint64((i+1)*4096+n))
				if _, err := h.SetInfo(gateContext(sessionID, treeID), &SetInfoRequest{
					InfoType: types.SMB2InfoTypeFile, FileID: fileID,
					FileInfoClass: uint8(types.FileAllocationInformation), Buffer: alloc,
				}); err != nil {
					t.Errorf("SetInfo(Allocation): %v", err)
					return
				}
				// Alternate a valid FILE_MODE_INFORMATION bit on and off.
				var m uint32
				if n%2 == 0 {
					m = uint32(types.FileWriteThrough)
				}
				binary.LittleEndian.PutUint32(mode, m)
				if _, err := h.SetInfo(gateContext(sessionID, treeID), &SetInfoRequest{
					InfoType: types.SMB2InfoTypeFile, FileID: fileID,
					FileInfoClass: uint8(types.FileModeInformation), Buffer: mode,
				}); err != nil {
					t.Errorf("SetInfo(Mode): %v", err)
					return
				}
			}
		}(i)
	}
	// Readers: the QUERY_INFO classes that report those two fields.
	for _, class := range []types.FileInfoClass{
		types.FileStandardInformation,
		types.FileNetworkOpenInformation,
		types.FileModeInformation,
		types.FileAllInformation,
	} {
		wg.Add(1)
		go func(c types.FileInfoClass) {
			defer wg.Done()
			req := &QueryInfoRequest{
				InfoType: types.SMB2InfoTypeFile, FileID: fileID,
				FileInfoClass: uint8(c), OutputBufferLength: 4096,
			}
			for n := 0; n < 50; n++ {
				resp, err := h.QueryInfo(gateContext(sessionID, treeID), req)
				if err != nil {
					t.Errorf("QueryInfo(%v): %v", c, err)
					return
				}
				// A failed query never reaches the fields under test, which
				// would make this a race test that exercises nothing.
				if got := resp.GetStatus(); got != types.StatusSuccess {
					t.Errorf("QueryInfo(%v): status = 0x%08x, want STATUS_SUCCESS", c, uint32(got))
					return
				}
			}
		}(class)
	}
	wg.Wait()
}

// TestWrite_ZeroLength_AdvancesPositionInfo covers the success path that returns
// before the position update the sibling test exercises.
//
// A zero-length WRITE is a valid no-op for the data, but it is still a
// successful WRITE, so MS-FSA 2.1.5.4 sets CurrentByteOffset to
// ByteOffset + BytesWritten — the offset itself. Returning early left
// PositionInfo reporting the position of the previous write, so a client
// pipelining a zero-length WRITE and then querying FilePositionInformation read
// a stale offset.
//
// The first write is what makes the assertion mean something: without it the
// handle's PositionInfo starts at 0 and any offset would look like a change.
func TestWrite_ZeroLength_AdvancesPositionInfo(t *testing.T) {
	h, fileID, open, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)

	resp, err := h.Write(gateContext(sessionID, treeID), &WriteRequest{
		FileID: fileID, Offset: 100, Length: 4, DataOffset: 64 + 48, Data: []byte("data"),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := resp.GetStatus(); got != types.StatusSuccess {
		t.Fatalf("seed Write status = 0x%08x, want STATUS_SUCCESS", uint32(got))
	}

	resp, err = h.Write(gateContext(sessionID, treeID), &WriteRequest{
		FileID: fileID, Offset: 4096, Length: 0, DataOffset: 64 + 48, Data: nil,
	})
	if err != nil {
		t.Fatalf("zero-length Write: %v", err)
	}
	if got := resp.GetStatus(); got != types.StatusSuccess {
		t.Fatalf("zero-length Write status = 0x%08x, want STATUS_SUCCESS", uint32(got))
	}
	if resp.Count != 0 {
		t.Fatalf("zero-length Write Count = %d, want 0", resp.Count)
	}

	open.mu.RLock()
	got := open.PositionInfo
	open.mu.RUnlock()
	if got != 4096 {
		t.Fatalf("PositionInfo after zero-length WRITE = %d, want 4096: a successful WRITE must advance CurrentByteOffset to ByteOffset + BytesWritten", got)
	}
}

// TestDecodeWriteRequest_HugeLengthIsRefusedNotSliced pins the bound on the
// WRITE payload length. The check compares in uint64: on a 32-bit build
// int(req.Length) of a wire value like 0x80000000 is negative, dataStart plus
// that lands below len(body), the check passes and the slice panics — from a
// request anyone can send. The module does not currently build for a 32-bit
// target (a dependency refuses to), so this pins the refusal rather than
// reproducing the wrap; on any arch the request must be rejected, not sliced.
func TestDecodeWriteRequest_HugeLengthIsRefusedNotSliced(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dataOffset uint16
		length     uint32
	}{
		// A length that wraps int on a 32-bit build.
		{"huge length", 64 + 48, 0x80000000},
		{"max length", 64 + 48, 0xFFFFFFFF},
		// A DataOffset past the end of the message. len(body)-dataStart
		// underflows in uint64, and then even a tiny length fits "available".
		{"offset past the end", 64 + 4096, 1},
		{"offset past the end, zero-ish length", 64 + 200, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := make([]byte, 48+16)
			binary.LittleEndian.PutUint16(body[0:], 49) // StructureSize
			binary.LittleEndian.PutUint16(body[2:], tc.dataOffset)
			binary.LittleEndian.PutUint32(body[4:], tc.length)
			binary.LittleEndian.PutUint64(body[8:], 0)

			req, err := DecodeWriteRequest(body)
			if err == nil {
				t.Errorf("decode succeeded with DataOffset=%d and Length=%#x on a %d-byte body; "+
					"want a refusal", tc.dataOffset, tc.length, len(body))
			}
			if req != nil && len(req.Data) > len(body) {
				t.Error("decoded Data is longer than the message")
			}
		})
	}
}
