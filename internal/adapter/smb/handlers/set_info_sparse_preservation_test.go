// Handler-level coverage for SET_INFO BasicInformation preservation of the
// FSCTL-managed modeDOSSparse bit on a non-stream (base) handle.
//
// Per MS-FSCC 2.4.7, FILE_ATTRIBUTE_SPARSE_FILE is not settable via
// FileBasicInformation — it is controlled exclusively via FSCTL_SET_SPARSE.
// A SET_INFO that updates DOS attributes (HIDDEN, READONLY, ...) must
// therefore preserve any existing modeDOSSparse bit rather than clearing it.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupSparsePreservationTest builds a memory-backed runtime with a single
// share containing one regular file at POSIX mode 0o644 and seeds the
// provided FSCTL-managed bits (e.g. modeDOSSparse) into its mode, mirroring
// what handleSetSparse / handleSetCompression do via SetFileAttributes.
//
// Returns the handler, auth context, the file's metadata handle, and an
// OpenFile (non-stream) on that file with FILE_WRITE_ATTRIBUTES access.
func setupSparsePreservationTest(t *testing.T, seedFSCTLBits uint32) (
	*Handler,
	*metadata.AuthContext,
	metadata.FileHandle,
	*OpenFile,
) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("sparse-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/sparse"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "sparse-meta",
		Enabled:       true,
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}

	metaSvc := rt.GetMetadataService()

	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "sparse.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// Seed the FSCTL-managed bits onto the file's mode, simulating a prior
	// FSCTL_SET_SPARSE / FSCTL_SET_COMPRESSION (see ioctl_sparse.go:107 and
	// ioctl_fsctl.go:111).
	seedMode := file.Mode | seedFSCTLBits
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Mode: &seedMode}); err != nil {
		t.Fatalf("SetFileAttributes(seed): %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	open := &OpenFile{
		FileID:         [16]byte{0x59, 0x9A, 0x12, 0x33},
		MetadataHandle: fileHandle,
		ParentHandle:   rootHandle,
		FileName:       "sparse.txt",
		Path:           "sparse.txt",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileWriteAttributes),
	}
	h.StoreOpenFile(open)

	return h, authCtx, fileHandle, open
}

// TestSetInfo_FileBasicInfo_PreservesSparseAfterAttrsChange: a SET_INFO
// FileBasicInformation that flips HIDDEN on a file already marked sparse via
// FSCTL_SET_SPARSE must NOT clear the modeDOSSparse bit. Before the fix the
// preserved-bit mask only carried modeDOSCompressed, so the sparse bit was
// dropped from the recomputed mode.
func TestSetInfo_FileBasicInfo_PreservesSparseAfterAttrsChange(t *testing.T) {
	h, authCtx, fileHandle, open := setupSparsePreservationTest(t, modeDOSSparse)
	metaSvc := h.Registry.GetMetadataService()

	// Pre-condition: the file must be sparse going in.
	pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(pre): %v", err)
	}
	if pre.Mode&modeDOSSparse == 0 {
		t.Fatalf("setup broken: modeDOSSparse not set before SET_INFO")
	}

	// SET_INFO FileBasicInformation, FileAttributes = HIDDEN (0x02), all
	// four FILETIME fields zero.
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(types.FileAttributeHidden))

	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(post): %v", err)
	}

	// The FSCTL-managed sparse bit must survive the attribute change.
	if got.Mode&modeDOSSparse == 0 {
		t.Errorf("modeDOSSparse cleared by FileBasicInformation SET_INFO; FSCTL-managed bit must survive")
	}
	// The intended attribute change must still land.
	if !got.Hidden {
		t.Errorf("got.Hidden = false after SET_INFO HIDDEN; expected true")
	}
	// POSIX permission bits must be stable.
	if gotPOSIX := got.Mode & 0o7777; gotPOSIX != 0o644 {
		t.Errorf("POSIX mode = 0o%o after SET_INFO; expected 0o644", gotPOSIX)
	}
}

// TestSetInfo_FileBasicInfo_PreservesSparseBitAlongsideCompressed: a regression
// guard proving the combined preserve mask (modeDOSCompressed | modeDOSSparse)
// keeps both FSCTL-managed bits across a FileBasicInformation SET_INFO.
func TestSetInfo_FileBasicInfo_PreservesSparseBitAlongsideCompressed(t *testing.T) {
	h, authCtx, fileHandle, open := setupSparsePreservationTest(t, modeDOSSparse|modeDOSCompressed)
	metaSvc := h.Registry.GetMetadataService()

	pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(pre): %v", err)
	}
	if pre.Mode&modeDOSSparse == 0 || pre.Mode&modeDOSCompressed == 0 {
		t.Fatalf("setup broken: expected both sparse and compressed bits set before SET_INFO")
	}

	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(types.FileAttributeHidden))

	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(post): %v", err)
	}

	if got.Mode&modeDOSSparse == 0 {
		t.Errorf("modeDOSSparse cleared by FileBasicInformation SET_INFO; FSCTL-managed bit must survive")
	}
	if got.Mode&modeDOSCompressed == 0 {
		t.Errorf("modeDOSCompressed cleared by FileBasicInformation SET_INFO; FSCTL-managed bit must survive")
	}
	if !got.Hidden {
		t.Errorf("got.Hidden = false after SET_INFO HIDDEN; expected true")
	}
}
