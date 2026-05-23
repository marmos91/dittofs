package handlers

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFileID_StableAcrossViews pins the contract exercised by smbtorture
// smb2.fileid.fileid / fileid-dir (refs #478): the on-disk file identity
// reported by SMB2 MUST be stable across the three views a client uses to
// observe it on a single file:
//
//  1. QFid create context response (DiskFileId, first 8 bytes)
//  2. FileInternalInformation [MS-FSCC §2.4.20]
//  3. FileAllInformation embedded InternalInformation field [MS-FSCC §2.4.2]
//  4. FileIdBothDirectoryInformation FileId field via QUERY_DIRECTORY
//
// All four MUST equal LittleEndian(uuid[0:8]) of the persistent metadata
// store's File.ID, and MUST persist across re-open of the same path.
func TestFileID_StableAcrossViews(t *testing.T) {
	h, authCtx, rootHandle := setupFileIDTest(t)
	metaSvc := h.Registry.GetMetadataService()

	file, err := metaSvc.CreateFile(authCtx, rootHandle, "foo", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	expected := binary.LittleEndian.Uint64(file.ID[:8])

	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	openFile := &OpenFile{
		FileID:         [16]byte{1},
		MetadataHandle: fileHandle,
		ParentHandle:   rootHandle,
		FileName:       "foo",
		Path:           "foo",
		ShareName:      "/fid",
		GrantedAccess:  0x001F01FF,
	}

	t.Run("QFid", func(t *testing.T) {
		qfid := h.baseFileUUID(authCtx, rootHandle, "foo", file.ID)
		if got := binary.LittleEndian.Uint64(qfid[:8]); got != expected {
			t.Errorf("QFid DiskFileId first 8 bytes = 0x%016x, want 0x%016x", got, expected)
		}
	})

	t.Run("FileInternalInformation", func(t *testing.T) {
		info, err := h.buildFileInfoFromStore(authCtx, file, openFile, types.FileInternalInformation)
		if err != nil {
			t.Fatalf("buildFileInfoFromStore: %v", err)
		}
		if len(info) != 8 {
			t.Fatalf("len = %d, want 8", len(info))
		}
		if got := binary.LittleEndian.Uint64(info); got != expected {
			t.Errorf("FileInternalInformation = 0x%016x, want 0x%016x", got, expected)
		}
	})

	t.Run("FileAllInformation", func(t *testing.T) {
		info, err := h.buildFileInfoFromStore(authCtx, file, openFile, types.FileAllInformation)
		if err != nil {
			t.Fatalf("buildFileInfoFromStore: %v", err)
		}
		// InternalInformation lives at offset 64 (Basic 40 + Standard 24).
		if len(info) < 72 {
			t.Fatalf("len = %d, want >= 72", len(info))
		}
		if got := binary.LittleEndian.Uint64(info[64:72]); got != expected {
			t.Errorf("FileAllInformation.InternalInformation @ offset 64 = 0x%016x, want 0x%016x",
				got, expected)
		}
	})

	t.Run("QueryDirectoryIdBoth", func(t *testing.T) {
		got := readFirstNamedEntryFileID(t, h, authCtx, rootHandle, "foo")
		if got != expected {
			t.Errorf("FileIdBothDirectoryInformation.FileId for %q = 0x%016x, want 0x%016x",
				"foo", got, expected)
		}
	})

	t.Run("ReopenSamePath", func(t *testing.T) {
		// Reopening the same path MUST resolve to the same File.ID and so
		// surface the same FileId in QFid + InternalInformation.
		reopened, err := metaSvc.Lookup(authCtx, rootHandle, "foo")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if reopened.ID != file.ID {
			t.Fatalf("re-open changed File.ID: got %v want %v", reopened.ID, file.ID)
		}
	})
}

// TestFileID_StreamMatchesBase pins the smbtorture invariant `Create stream,
// check the stream's File-ID, should be the same as the base file
// (sic!, tested against Windows)`. ADS opens MUST report the base file's
// FileId in both QFid and FileInternalInformation (refs #478).
func TestFileID_StreamMatchesBase(t *testing.T) {
	h, authCtx, rootHandle := setupFileIDTest(t)
	metaSvc := h.Registry.GetMetadataService()

	base, err := metaSvc.CreateFile(authCtx, rootHandle, "foo", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile base: %v", err)
	}
	expected := binary.LittleEndian.Uint64(base.ID[:8])

	// ADS streams are stored as siblings in the parent directory under names
	// containing a colon (see internal/adapter/smb/v2/handlers/close.go).
	stream, err := metaSvc.CreateFile(authCtx, rootHandle, "foo:bar", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile stream: %v", err)
	}
	if stream.ID == base.ID {
		t.Fatalf("ADS stream and base share File.ID — they must be distinct rows")
	}

	streamHandle, err := metadata.EncodeFileHandle(stream)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	openStream := &OpenFile{
		FileID:         [16]byte{2},
		MetadataHandle: streamHandle,
		ParentHandle:   rootHandle,
		FileName:       "foo:bar",
		Path:           "foo:bar",
		ShareName:      "/fid",
		GrantedAccess:  0x001F01FF,
	}

	qfid := h.baseFileUUID(authCtx, rootHandle, "foo:bar", stream.ID)
	if got := binary.LittleEndian.Uint64(qfid[:8]); got != expected {
		t.Errorf("ADS QFid first 8 bytes = 0x%016x, want base 0x%016x", got, expected)
	}

	info, err := h.buildFileInfoFromStore(authCtx, stream, openStream, types.FileInternalInformation)
	if err != nil {
		t.Fatalf("buildFileInfoFromStore: %v", err)
	}
	if got := binary.LittleEndian.Uint64(info); got != expected {
		t.Errorf("ADS FileInternalInformation = 0x%016x, want base 0x%016x", got, expected)
	}

	allInfo, err := h.buildFileInfoFromStore(authCtx, stream, openStream, types.FileAllInformation)
	if err != nil {
		t.Fatalf("buildFileInfoFromStore: %v", err)
	}
	if got := binary.LittleEndian.Uint64(allInfo[64:72]); got != expected {
		t.Errorf("ADS FileAllInformation.InternalInformation = 0x%016x, want base 0x%016x", got, expected)
	}
}

// TestFileID_UniqueAcrossSiblings pins smbtorture smb2.fileid.unique /
// unique-dir: rapidly created peers MUST report distinct FileIds.
func TestFileID_UniqueAcrossSiblings(t *testing.T) {
	h, authCtx, rootHandle := setupFileIDTest(t)
	metaSvc := h.Registry.GetMetadataService()

	const n = 100
	for _, kind := range []metadata.FileType{metadata.FileTypeRegular, metadata.FileTypeDirectory} {
		seen := make(map[uint64]string, n)
		for i := 0; i < n; i++ {
			name := "u." + strconv.Itoa(int(kind)) + "." + strconv.Itoa(i)
			var (
				f   *metadata.File
				err error
			)
			if kind == metadata.FileTypeDirectory {
				f, err = metaSvc.CreateDirectory(authCtx, rootHandle, name, &metadata.FileAttr{
					Type: metadata.FileTypeDirectory,
					Mode: 0o755,
				})
			} else {
				f, err = metaSvc.CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{
					Type: metadata.FileTypeRegular,
					Mode: 0o644,
				})
			}
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
			fh, err := metadata.EncodeFileHandle(f)
			if err != nil {
				t.Fatalf("EncodeFileHandle: %v", err)
			}
			info, err := h.buildFileInfoFromStore(
				authCtx, f,
				&OpenFile{
					MetadataHandle: fh, ParentHandle: rootHandle,
					FileName: name, Path: name, ShareName: "/fid",
					GrantedAccess: 0x001F01FF,
				},
				types.FileInternalInformation,
			)
			if err != nil {
				t.Fatalf("buildFileInfoFromStore: %v", err)
			}
			id := binary.LittleEndian.Uint64(info)
			if prev, dup := seen[id]; dup {
				t.Fatalf("duplicate FileId 0x%016x: %s vs %s", id, prev, name)
			}
			seen[id] = name
		}
	}
}

// readFirstNamedEntryFileID drives QueryDirectory with FileIdBothDirectoryInformation
// and an exact-name pattern, returning the FileId field of the matched entry.
// The wire format for FILE_ID_BOTH_DIR_INFORMATION places FileId at offset 96
// within each entry [MS-FSCC §2.4.18].
func readFirstNamedEntryFileID(
	t *testing.T,
	h *Handler,
	authCtx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	pattern string,
) uint64 {
	t.Helper()

	dirOpen := &OpenFile{
		FileID:         [16]byte{0xDD},
		MetadataHandle: dirHandle,
		ParentHandle:   dirHandle,
		FileName:       "",
		Path:           "",
		ShareName:      "/fid",
		IsDirectory:    true,
		GrantedAccess:  0x001F01FF,
	}
	h.StoreOpenFile(dirOpen)

	req := &QueryDirectoryRequest{
		FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
		Flags:              uint8(types.SMB2RestartScans),
		FileID:             dirOpen.FileID,
		FileName:           pattern,
		OutputBufferLength: 1 << 16,
	}
	resp, err := h.QueryDirectory(&SMBHandlerContext{Context: authCtx.Context}, req)
	if err != nil {
		t.Fatalf("QueryDirectory: %v", err)
	}
	if resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("QueryDirectory status = 0x%08x", resp.GetStatus())
	}
	if len(resp.Data) < 104 {
		t.Fatalf("QueryDirectory data too short: %d", len(resp.Data))
	}
	return binary.LittleEndian.Uint64(resp.Data[96:104])
}

// setupFileIDTest creates a memory-backed runtime + handler and returns the
// share's root directory handle so callers can populate fixtures via
// MetadataService.
func setupFileIDTest(t *testing.T) (*Handler, *metadata.AuthContext, metadata.FileHandle) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("fid-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          "/fid",
		MetadataStore: "fid-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle("/fid")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	h := NewHandler()
	h.Registry = rt
	return h, authCtx, rootHandle
}
