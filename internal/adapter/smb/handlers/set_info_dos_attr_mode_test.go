// Handler-level coverage for the POSIX permission bits of a file whose DOS
// attributes are updated over SMB.
//
// A FileAttributes update is a DOS attribute change and nothing else: MS-FSCC
// 2.6 describes HIDDEN / SYSTEM / ARCHIVE / READONLY, none of which say
// anything about who may read or write the file. DittoFS stores those bits in
// the high word of the same mode field that carries the POSIX permission
// triple, so an update that rewrites the whole mode silently republishes the
// file at whatever permissions the SMB layer would have synthesized for a new
// one — a 0o600 file becomes 0o644, and an NFS client sees the widened mode on
// its next GETATTR.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupDOSAttrModeTest builds a memory-backed runtime holding a single object
// of the requested type at the requested POSIX mode, and returns the handler,
// a root auth context, the object's metadata handle and an OpenFile on it with
// FILE_WRITE_ATTRIBUTES access.
func setupDOSAttrModeTest(t *testing.T, fileType metadata.FileType, mode uint32) (
	*Handler,
	*metadata.AuthContext,
	metadata.FileHandle,
	*OpenFile,
) {
	t.Helper()

	rt, bsID := newTestShareRuntime(t)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("dosmode-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/dosmode"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "dosmode-meta",
		BlockStoreID:      bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		Enabled:           true,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
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
	// CreateFile hardcodes FileTypeRegular and ignores attr.Type, so a
	// directory has to be created through CreateDirectory to be one.
	attr := &metadata.FileAttr{Type: fileType, Mode: mode}
	var file *metadata.File
	if fileType == metadata.FileTypeDirectory {
		file, _, err = metaSvc.CreateDirectory(authCtx, rootHandle, "target", attr)
	} else {
		file, _, err = metaSvc.CreateFile(authCtx, rootHandle, "target", attr)
	}
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if file.Type != fileType {
		t.Fatalf("test subject is a %v, not the %v the case names", file.Type, fileType)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	open := (&OpenFile{
		FileID:         [16]byte{0x27, 0x17},
		MetadataHandle: fileHandle,
		ShareName:      shareName,
		IsDirectory:    fileType == metadata.FileTypeDirectory,
		DesiredAccess:  uint32(types.FileWriteAttributes),
	}).WithName(OpenName{Path: "target", FileName: "target", ParentHandle: rootHandle})
	h.StoreOpenFile(open)

	return h, authCtx, fileHandle, open
}

// basicInfoBuffer builds a 40-byte FILE_BASIC_INFORMATION carrying only the
// given FileAttributes; all four FILETIME fields are zero ("do not change").
func basicInfoBuffer(attrs types.FileAttributes) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(attrs))
	return buf
}

// TestSetInfo_FileBasicInfo_PreservesPOSIXMode is the reported case and its
// neighbours: a SET_INFO FileBasicInformation carrying FileAttributes must
// leave the POSIX permission triple exactly as it found it, for files and
// directories alike and whatever the mode happens to be.
func TestSetInfo_FileBasicInfo_PreservesPOSIXMode(t *testing.T) {
	cases := []struct {
		name     string
		fileType metadata.FileType
		mode     uint32
		attrs    types.FileAttributes
	}{
		// The reported case: a private file loses group/other protection
		// because the client set HIDDEN.
		{"private file, HIDDEN", metadata.FileTypeRegular, 0o600, types.FileAttributeHidden},
		// The other direction: a file wider than the synthesized default must
		// not be narrowed either.
		{"world-writable file, ARCHIVE", metadata.FileTypeRegular, 0o777, types.FileAttributeArchive},
		{"private file, READONLY", metadata.FileTypeRegular, 0o600, types.FileAttributeReadonly},
		{"default-mode file, SYSTEM", metadata.FileTypeRegular, 0o644, types.FileAttributeSystem},
		// Directories synthesize 0o755, so a private directory loses the most.
		{"private directory, HIDDEN", metadata.FileTypeDirectory, 0o700, types.FileAttributeHidden},
		{"default-mode directory, HIDDEN", metadata.FileTypeDirectory, 0o755, types.FileAttributeHidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, authCtx, fileHandle, open := setupDOSAttrModeTest(t, tc.fileType, tc.mode)
			metaSvc := h.Registry.GetMetadataService()

			pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
			if err != nil {
				t.Fatalf("GetFile(pre): %v", err)
			}
			wantPOSIX := pre.Mode & 0o7777

			resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, basicInfoBuffer(tc.attrs))
			if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
				t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
			}

			got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
			if err != nil {
				t.Fatalf("GetFile(post): %v", err)
			}

			if gotPOSIX := got.Mode & 0o7777; gotPOSIX != wantPOSIX {
				t.Errorf("POSIX mode = 0o%o after SET_INFO FileBasicInformation; want 0o%o unchanged",
					gotPOSIX, wantPOSIX)
			}

			// The DOS attribute the client asked for must still round-trip.
			if tc.attrs&types.FileAttributeHidden != 0 && !got.Hidden {
				t.Errorf("Hidden = false after SET_INFO HIDDEN; want true")
			}
			gotAttrs := fileAttrToSMBAttributesInternal(&got.FileAttr, got.Hidden)
			if gotAttrs&tc.attrs != tc.attrs {
				t.Errorf("FileAttributes = 0x%x after SET_INFO 0x%x; requested bits did not round-trip",
					gotAttrs, tc.attrs)
			}
		})
	}
}

// TestCreateOverwrite_PreservesPOSIXMode covers the same defect on the CREATE
// path: FILE_OVERWRITE / FILE_SUPERSEDE truncates an existing file and applies
// the requested attributes, and must not republish the file at the mode the
// SMB layer would have synthesized for a new one.
func TestCreateOverwrite_PreservesPOSIXMode(t *testing.T) {
	cases := []struct {
		name string
		mode uint32
	}{
		{"private file", 0o600},
		{"world-writable file", 0o777},
		{"default-mode file", 0o644},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, authCtx, fileHandle, _ := setupDOSAttrModeTest(t, metadata.FileTypeRegular, tc.mode)
			metaSvc := h.Registry.GetMetadataService()

			pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
			if err != nil {
				t.Fatalf("GetFile(pre): %v", err)
			}
			wantPOSIX := pre.Mode & 0o7777

			updated, _, err := h.overwriteFile(authCtx, pre, &CreateRequest{
				FileAttributes: types.FileAttributeHidden,
			})
			if err != nil {
				t.Fatalf("overwriteFile: %v", err)
			}

			if gotPOSIX := updated.Mode & 0o7777; gotPOSIX != wantPOSIX {
				t.Errorf("POSIX mode = 0o%o after CREATE overwrite; want 0o%o unchanged",
					gotPOSIX, wantPOSIX)
			}
			if !updated.Hidden {
				t.Errorf("Hidden = false after CREATE overwrite with HIDDEN; want true")
			}
			// MS-FSA 2.1.5.1.2: an overwrite forces ARCHIVE regardless of what
			// the client sent.
			if updated.Mode&modeDOSArchive == 0 {
				t.Errorf("modeDOSArchive not set after CREATE overwrite; want forced ARCHIVE")
			}
		})
	}
}

// TestSetInfo_FileBasicInfo_PreservesFSCTLBitsWithoutReReading is the flip side
// of the mask form: the FSCTL-managed bits are preserved because a
// FileAttributes update never names them, not because the handler reads them
// back. Seeding both and flipping HIDDEN must leave both standing.
func TestSetInfo_FileBasicInfo_PreservesFSCTLBitsWithoutReReading(t *testing.T) {
	h, authCtx, fileHandle, open := setupDOSAttrModeTest(t, metadata.FileTypeRegular, 0o600)
	metaSvc := h.Registry.GetMetadataService()

	seed := modeDOSSparse | modeDOSCompressed
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{ModeOrMask: &seed}); err != nil {
		t.Fatalf("SetFileAttributes(seed): %v", err)
	}

	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation,
		basicInfoBuffer(types.FileAttributeHidden))
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(post): %v", err)
	}
	if got.Mode&modeDOSSparse == 0 || got.Mode&modeDOSCompressed == 0 {
		t.Errorf("FSCTL-managed bits cleared by FileBasicInformation SET_INFO: mode=0x%x", got.Mode)
	}
	if gotPOSIX := got.Mode & 0o7777; gotPOSIX != 0o600 {
		t.Errorf("POSIX mode = 0o%o after SET_INFO; want 0o600 unchanged", gotPOSIX)
	}
}

// TestSetInfo_FileBasicInfo_ReadonlyRoundTripOnNonDefaultMode pins the READONLY
// round-trip on a file whose POSIX mode is not the synthesized default, in both
// directions, and records the one-way edge of the POSIX-derived fallback.
//
// Setting and clearing READONLY moves modeDOSReadonly and nothing else. A file
// that was read-only only by virtue of its POSIX bits reports READONLY until
// the first SET_INFO gives it an explicit DOS attribute state; from then on the
// reported attribute follows modeDOSReadonly alone, and the POSIX bits are the
// client's to change through a chmod rather than an attribute write.
func TestSetInfo_FileBasicInfo_ReadonlyRoundTripOnNonDefaultMode(t *testing.T) {
	h, authCtx, fileHandle, open := setupDOSAttrModeTest(t, metadata.FileTypeRegular, 0o600)
	metaSvc := h.Registry.GetMetadataService()

	setInfo := func(attrs types.FileAttributes) *metadata.File {
		t.Helper()
		resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, basicInfoBuffer(attrs))
		if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
			t.Fatalf("setFileInfoFromStore(0x%x): err=%v status=%v", attrs, err, resp)
		}
		got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		return got
	}

	// Set READONLY: the attribute lands, the mode does not move.
	got := setInfo(types.FileAttributeReadonly)
	if fileAttrToSMBAttributesInternal(&got.FileAttr, got.Hidden)&types.FileAttributeReadonly == 0 {
		t.Errorf("READONLY did not round-trip after being set")
	}
	if gotPOSIX := got.Mode & 0o7777; gotPOSIX != 0o600 {
		t.Errorf("POSIX mode = 0o%o after setting READONLY; want 0o600 unchanged", gotPOSIX)
	}

	// Clear it again with a different attribute: READONLY goes away, the mode
	// still does not move.
	got = setInfo(types.FileAttributeArchive)
	if fileAttrToSMBAttributesInternal(&got.FileAttr, got.Hidden)&types.FileAttributeReadonly != 0 {
		t.Errorf("READONLY survived a SET_INFO that did not request it")
	}
	if gotPOSIX := got.Mode & 0o7777; gotPOSIX != 0o600 {
		t.Errorf("POSIX mode = 0o%o after clearing READONLY; want 0o600 unchanged", gotPOSIX)
	}
}

// TestSetInfo_FileBasicInfo_PosixReadonlyFallbackIsOneWay pins the edge the
// decision comment on the fallback in fileAttrToSMBAttributesInternal describes:
// a file made read-only out-of-band reports READONLY, and a SET_INFO clearing
// it makes the report follow the DOS bit from then on WITHOUT restoring
// owner-write. Pinned so the tradeoff is visible rather than discovered.
func TestSetInfo_FileBasicInfo_PosixReadonlyFallbackIsOneWay(t *testing.T) {
	h, authCtx, fileHandle, open := setupDOSAttrModeTest(t, metadata.FileTypeRegular, 0o444)
	metaSvc := h.Registry.GetMetadataService()

	pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(pre): %v", err)
	}
	if fileAttrToSMBAttributesInternal(&pre.FileAttr, pre.Hidden)&types.FileAttributeReadonly == 0 {
		t.Fatalf("setup broken: a 0o444 file with no explicit DOS state must report READONLY")
	}

	// FILE_ATTRIBUTE_NORMAL is what a client sends to clear the read-only box.
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation,
		basicInfoBuffer(types.FileAttributeNormal))
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	got, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile(post): %v", err)
	}
	if fileAttrToSMBAttributesInternal(&got.FileAttr, got.Hidden)&types.FileAttributeReadonly != 0 {
		t.Errorf("READONLY still reported after the client cleared it")
	}
	// The documented tradeoff: the POSIX bits are untouched, so the file is
	// still not writable through its mode. An attribute write must not widen
	// permissions, which is the whole point of the mask form.
	if gotPOSIX := got.Mode & 0o7777; gotPOSIX != 0o444 {
		t.Errorf("POSIX mode = 0o%o after clearing READONLY; want 0o444 unchanged", gotPOSIX)
	}
}
