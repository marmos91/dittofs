package handlers

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
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
//     (per MS-FSA 2.1.5.15.3).
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
	w4FileWriteData uint32 = 0x00000002
	w4FileWriteEA   uint32 = 0x00000010
	w4FileReadData  uint32 = 0x00000001
	w4FileWriteAttr uint32 = 0x00000100
	w4DeleteAccess  uint32 = 0x00010000
)

// setupSetInfoGateTest stands up a Handler + runtime with an in-memory store,
// creates a file under the share root, and registers an OpenFile for it with
// the given GrantedAccess. Returns the handler, the file ID, and the open.
func setupSetInfoGateTest(t *testing.T, grantedAccess uint32) (*Handler, [16]byte, *OpenFile, uint64, uint32) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/wave4-rw-test"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
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

	uid := uint32(0)
	gid := uint32(0)
	auth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
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

	// Write resolves a per-share block store; publish one via the locked
	// testing setter so the WRITE path can prepare/commit payloads.
	metaStore := memStore
	tmpDir := t.TempDir()
	localStore, err := fs.NewWithOptions(tmpDir, 0, metaStore, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("create local store: %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })
	syncer := engine.NewRemoteSync(localStore, nil, metaStore, engine.DefaultConfig())
	blockSvc, err := engine.New(engine.BlockStoreConfig{
		Local:      localStore,
		RemoteSync: syncer,
	})
	if err != nil {
		t.Fatalf("create block store: %v", err)
	}
	if err := blockSvc.Start(context.Background()); err != nil {
		t.Fatalf("start block store: %v", err)
	}
	t.Cleanup(func() { _ = blockSvc.Close() })
	if err := rt.SetBlockStoreForTesting(shareName, blockSvc); err != nil {
		t.Fatalf("SetBlockStoreForTesting: %v", err)
	}

	// Write/SetInfo validate session and tree: mint a real session and store a
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

// w4Context builds a handler context bound to the test's session and tree.
func w4Context(sessionID uint64, treeID uint32) *SMBHandlerContext {
	return &SMBHandlerContext{Context: context.Background(), SessionID: sessionID, TreeID: treeID}
}

// w4SetInfo drives a SET_INFO file-class request and returns the status.
func w4SetInfo(t *testing.T, ctx *SMBHandlerContext, h *Handler, fileID [16]byte, class types.FileInfoClass, buffer []byte) types.Status {
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
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileWriteData|w4FileWriteAttr)
	// Per MS-FSA 2.1.5.15.1: InputBufferSize < 8 → STATUS_INFO_LENGTH_MISMATCH.
	// A nil buffer must not fall through to Success.
	if got := w4SetInfo(t, w4Context(sessionID, treeID), h, fileID, types.FileAllocationInformation, nil); got != types.StatusInfoLengthMismatch {
		t.Fatalf("nil allocation buffer: status = 0x%08x, want STATUS_INFO_LENGTH_MISMATCH (0x%08x)",
			uint32(got), uint32(types.StatusInfoLengthMismatch))
	}
	// A 4-byte buffer is also too short.
	if got := w4SetInfo(t, w4Context(sessionID, treeID), h, fileID, types.FileAllocationInformation, []byte{1, 2, 3, 4}); got != types.StatusInfoLengthMismatch {
		t.Fatalf("4-byte allocation buffer: status = 0x%08x, want STATUS_INFO_LENGTH_MISMATCH",
			uint32(got))
	}
}

func TestSetInfo_EAWithoutFileWriteEA_AccessDenied(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileReadData|w4FileWriteData|w4FileWriteAttr)
	// Handle holds FILE_WRITE_DATA but not FILE_WRITE_EA: EA set must be denied.
	buf := w4EncodeEAEntry("user.test", []byte("v"))
	if got := w4SetInfo(t, w4Context(sessionID, treeID), h, fileID, types.FileFullEaInformation, buf); got != types.StatusAccessDenied {
		t.Fatalf("EA set without FILE_WRITE_EA: status = 0x%08x, want STATUS_ACCESS_DENIED (0x%08x)",
			uint32(got), uint32(types.StatusAccessDenied))
	}
	// A handle with FILE_WRITE_EA but WITHOUT FILE_WRITE_ATTRIBUTES must
	// still pass: the EA class is exempt from the step-1b attributes gate
	// (its own FILE_WRITE_EA check is what governs).
	h2, fileID2, _, sessionID2, treeID2 := setupSetInfoGateTest(t, w4FileReadData|w4FileWriteEA)
	if got := w4SetInfo(t, w4Context(sessionID2, treeID2), h2, fileID2, types.FileFullEaInformation, buf); got != types.StatusSuccess {
		t.Fatalf("EA set with FILE_WRITE_EA only (no FILE_WRITE_ATTRIBUTES): status = 0x%08x, want STATUS_SUCCESS (0x%08x)",
			uint32(got), uint32(types.StatusSuccess))
	}
}

func TestSetInfo_EOFWithoutFileWriteData_AccessDenied(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileReadData|w4FileWriteEA|w4FileWriteAttr)
	// Handle holds no FILE_WRITE_DATA: EOF set must be denied.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, 0)
	if got := w4SetInfo(t, w4Context(sessionID, treeID), h, fileID, types.FileEndOfFileInformation, buf); got != types.StatusAccessDenied {
		t.Fatalf("EOF set without FILE_WRITE_DATA: status = 0x%08x, want STATUS_ACCESS_DENIED (0x%08x)",
			uint32(got), uint32(types.StatusAccessDenied))
	}
}

func TestSetInfo_RenameNonZeroRootDirectory_InvalidParameter(t *testing.T) {
	// Rename requires DELETE access on the source (MS-FSA 2.1.5.15.12), granted
	// here so the RootDirectory check is what fires.
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileWriteData|w4FileWriteEA|w4FileWriteAttr|w4DeleteAccess)
	// RootDirectory non-zero with a handle-relative name: must be rejected,
	// not silently renamed within the same directory.
	buf := w4EncodeRenameInfo(false, 0xDEADBEEF, "\\moved.txt")
	if got := w4SetInfo(t, w4Context(sessionID, treeID), h, fileID, types.FileRenameInformation, buf); got != types.StatusInvalidParameter {
		t.Fatalf("rename with non-zero RootDirectory: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
			uint32(got), uint32(types.StatusInvalidParameter))
	}
}

func TestWrite_RdmaChannel_InvalidParameter(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileWriteData|w4FileWriteAttr)
	resp, err := h.Write(w4Context(sessionID, treeID), &WriteRequest{
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
	copy(w[48:], "0123456789abcdef")            // payload at 48, not at 56
	binary.LittleEndian.PutUint64(w[8:], 0)     // Offset
	copy(w[48:], "0123456789abcdef")            // payload at 48, DataOffset says 64
	if _, err := DecodeWriteRequest(body); err == nil {
		t.Fatal("DecodeWriteRequest with payload not at DataOffset: want error, got nil")
	}
}

func TestSetInfo_PositionInfo_ConcurrentSetQuery_RaceFree(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, w4FileWriteData|w4FileWriteAttr)

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
				resp, err := h.SetInfo(w4Context(sessionID, treeID), &SetInfoRequest{
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
		req := &QueryInfoRequest{
			InfoType:      types.SMB2InfoTypeFile,
			FileInfoClass: uint8(types.FilePositionInformation),
			FileID:        fileID,
		}
		for n := 0; n < 50; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := h.QueryInfo(w4Context(sessionID, treeID), req); err != nil {
				t.Errorf("QueryInfo(Position): %v", err)
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
}

func TestWrite_AdvancesPositionInfo(t *testing.T) {
	h, fileID, open, sessionID, treeID := setupSetInfoGateTest(t, w4FileWriteData|w4FileWriteAttr)
	resp, err := h.Write(w4Context(sessionID, treeID), &WriteRequest{
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

// w4EncodeEAEntry builds a minimal FILE_FULL_EA_INFORMATION buffer with one
// entry (name + value) per [MS-FSCC] 2.4.16: NextEntryOffset (4B LE), Flags
// (1B), NameLength (1B), ValueLength (2B LE), Name, NUL, Value.
func w4EncodeEAEntry(name string, value []byte) []byte {
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

// w4EncodeRenameInfo builds a FILE_RENAME_INFORMATION buffer per
// [MS-FSCC] 2.4.42: ReplaceIfExists (1B), Reserved (7B), RootDirectory (8B),
// FileNameLength (4B), FileName (UTF-16LE).
func w4EncodeRenameInfo(replaceIfExists bool, rootDirectory uint64, fileName string) []byte {
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
