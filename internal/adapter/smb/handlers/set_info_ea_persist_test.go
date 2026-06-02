// Handler-level coverage for SET_INFO / QUERY_INFO FileFullEaInformation EA
// persistence and FILE_STANDARD_INFORMATION nlink-on-delete-pending, mirroring
// the smbtorture smb2.setinfo EA and DISPOSITION assertion sequence.
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

// setupEATest builds a memory-backed runtime with a single share containing a
// regular file `ea.txt`, returns the handler, root auth context, and an
// OpenFile on the file granting full EA + delete access.
func setupEATest(t *testing.T) (*Handler, *metadata.AuthContext, *OpenFile) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ea-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/ea"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ea-meta",
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
	file, err := metaSvc.CreateFile(authCtx, rootHandle, "ea.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	open := &OpenFile{
		FileID:         [16]byte{0xEA, 0x01},
		MetadataHandle: handle,
		ParentHandle:   rootHandle,
		FileName:       "ea.txt",
		Path:           "ea.txt",
		ShareName:      shareName,
		GrantedAccess: uint32(types.FileWriteEA) | uint32(types.FileReadEA) |
			uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes) |
			uint32(types.Delete),
		DesiredAccess: uint32(types.FileWriteEA) | uint32(types.FileReadEA),
	}
	h.StoreOpenFile(open)
	return h, authCtx, open
}

// queryEAs runs QUERY_INFO FileFullEaInformation and decodes the returned EAs.
func queryEAs(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile) map[string]string {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(authCtx.Context, open.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	info, err := h.buildFileInfoFromStore(authCtx, file, open, types.FileFullEaInformation)
	if err != nil {
		t.Fatalf("buildFileInfoFromStore(EA): %v", err)
	}
	entries, err := decodeFullEaEntries(info)
	if err != nil {
		t.Fatalf("decode EA response: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.name == "" {
			continue // empty-list sentinel entry
		}
		out[e.name] = string(e.value)
	}
	return out
}

// setEA issues a SET_INFO FileFullEaInformation with a single EA (zero-length
// value deletes per MS-FSCC §2.4.15).
func setEA(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile, name string, value []byte) types.Status {
	t.Helper()
	buf := encodeOneEAEntry(name, value)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileFullEaInformation, buf)
	if err != nil {
		t.Fatalf("setFileInfoFromStore(EA): %v", err)
	}
	return resp.GetStatus()
}

// TestSetInfo_EA_SetGetDelete mirrors the smbtorture setinfo EA sequence:
// set "NewEA"="testme" → it appears in GET; set "NewEA" to empty → it is gone;
// a pre-existing "EAONE" (seeded like the CREATE EA context) survives.
func TestSetInfo_EA_SetGetDelete(t *testing.T) {
	h, authCtx, open := setupEATest(t)

	// Seed a pre-existing EA the way the CREATE EA buffer context would.
	metaSvc := h.Registry.GetMetadataService()
	if err := metaSvc.SetFileAttributes(authCtx, open.MetadataHandle, &metadata.SetAttrs{
		EAMutations: []metadata.EAMutation{{Name: "EAONE", Value: []byte("one")}},
	}); err != nil {
		t.Fatalf("seed EAONE: %v", err)
	}

	if st := setEA(t, h, authCtx, open, "NewEA", []byte("testme")); st != types.StatusSuccess {
		t.Fatalf("set NewEA status = %v", st)
	}
	got := queryEAs(t, h, authCtx, open)
	if got["NewEA"] != "testme" {
		t.Fatalf("NewEA = %q after set, want testme (all: %v)", got["NewEA"], got)
	}
	if got["EAONE"] != "one" {
		t.Fatalf("pre-existing EAONE lost: %v", got)
	}

	// Empty value deletes NewEA but must not touch EAONE.
	if st := setEA(t, h, authCtx, open, "NewEA", []byte{}); st != types.StatusSuccess {
		t.Fatalf("delete NewEA status = %v", st)
	}
	got = queryEAs(t, h, authCtx, open)
	if _, ok := got["NewEA"]; ok {
		t.Fatalf("NewEA still present after empty-value set: %v", got)
	}
	if got["EAONE"] != "one" {
		t.Fatalf("EAONE disturbed by NewEA deletion: %v", got)
	}
}

// TestSetInfo_EA_ReservedACLNameRejected: SET on the reserved ACL xattr slot
// must return ACCESS_DENIED.
func TestSetInfo_EA_ReservedACLNameRejected(t *testing.T) {
	h, authCtx, open := setupEATest(t)
	if st := setEA(t, h, authCtx, open, reservedACLXattrName, []byte("x")); st != types.StatusAccessDenied {
		t.Fatalf("reserved EA set status = %v, want ACCESS_DENIED", st)
	}
}

// TestQueryInfo_AllInfo_NlinkTracksDeletePending mirrors smbtorture
// setinfo.c:229: FILE_ALL_INFORMATION reports NumberOfLinks = 0 when the open
// is delete-pending, 1 otherwise.
func TestQueryInfo_AllInfo_NlinkTracksDeletePending(t *testing.T) {
	h, authCtx, open := setupEATest(t)
	metaSvc := h.Registry.GetMetadataService()

	nlinkFromAllInfo := func() uint32 {
		file, err := metaSvc.GetFile(authCtx.Context, open.MetadataHandle)
		if err != nil {
			t.Fatalf("GetFile: %v", err)
		}
		info := h.buildFileAllInformationFromStore(authCtx, file, open)
		// FILE_STANDARD_INFORMATION starts at offset 40; NumberOfLinks is a
		// uint32 at offset +16 within it (after AllocationSize + EndOfFile),
		// i.e. absolute offset 56.
		return binary.LittleEndian.Uint32(info[56:60])
	}

	open.DeletePending = false
	if got := nlinkFromAllInfo(); got != 1 {
		t.Fatalf("nlink = %d with delete_pending=false, want 1", got)
	}

	open.DeletePending = true
	if got := nlinkFromAllInfo(); got != 0 {
		t.Fatalf("nlink = %d with delete_pending=true, want 0", got)
	}
}
