// Handler-level coverage for SET_INFO BasicInformation explicit-timestamp
// preservation, mirroring smb2.setinfo (source4/torture/smb2/setinfo.c).
//
// The torture test sets all four timestamps to explicit values in one
// BasicInfo call (block 1), then issues a second BasicInfo call that mutates
// only FileAttributes while sending zero timestamps (block 2, "a zero time
// means don't change"), and asserts the explicit values from block 1 survive
// unbumped. Per MS-FSA 2.1.5.14.2 an explicit (non-zero, non-sentinel)
// timestamp set suppresses the automatic update of that field on subsequent
// operations. Without that suppression the attribute-change path auto-bumps
// LastWriteTime (set_info.go) and ChangeTime (metadata file_modify.go),
// clobbering the explicit values.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupBasicInfoTimestampTest builds a memory-backed runtime with a single
// share containing one regular file, and returns the handler, auth context,
// the file's metadata handle, and an OpenFile for it.
func setupBasicInfoTimestampTest(t *testing.T) (
	*Handler,
	*metadata.AuthContext,
	metadata.FileHandle,
	*OpenFile,
) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/ts"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ts-meta",
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
	file, err := metaSvc.CreateFile(authCtx, rootHandle, "ts.txt", &metadata.FileAttr{
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

	h := NewHandler()
	h.Registry = rt

	open := &OpenFile{
		FileID:         [16]byte{0x75, 0x73, 0x31},
		MetadataHandle: fileHandle,
		ParentHandle:   rootHandle,
		FileName:       "ts.txt",
		Path:           "ts.txt",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes),
	}
	h.StoreOpenFile(open)

	return h, authCtx, fileHandle, open
}

// encodeBasicInfo builds a 40-byte FILE_BASIC_INFORMATION buffer. Times are
// FILETIME values (0 = don't change); attrib is the FileAttributes field.
func encodeBasicInfo(creationFT, atimeFT, mtimeFT, ctimeFT uint64, attrib types.FileAttributes) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint64(buf[0:8], creationFT)
	binary.LittleEndian.PutUint64(buf[8:16], atimeFT)
	binary.LittleEndian.PutUint64(buf[16:24], mtimeFT)
	binary.LittleEndian.PutUint64(buf[24:32], ctimeFT)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(attrib))
	return buf
}

// TestSetInfo_BasicInfo_ExplicitTimestampsSurviveAttributeChange reproduces
// the smb2.setinfo block-1 → block-2 sequence: set all four timestamps to
// explicit values, then issue an attribute-only SET_INFO with zero
// timestamps, and assert all four explicit values survive in metadata.
func TestSetInfo_BasicInfo_ExplicitTimestampsSurviveAttributeChange(t *testing.T) {
	h, authCtx, fileHandle, open := setupBasicInfoTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	// Whole-second times so the FILETIME (100ns tick) round-trip is exact.
	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	wantCreate := base.Add(100 * time.Second)
	wantAccess := base.Add(200 * time.Second)
	wantWrite := base.Add(300 * time.Second)
	wantChange := base.Add(400 * time.Second)

	// Block 1: set all four timestamps + attrib=READONLY.
	buf1 := encodeBasicInfo(
		types.TimeToFiletime(wantCreate),
		types.TimeToFiletime(wantAccess),
		types.TimeToFiletime(wantWrite),
		types.TimeToFiletime(wantChange),
		types.FileAttributeReadonly,
	)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf1)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("block 1 setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	// After block 1 the explicit set must have frozen each field so a later
	// implicit update is suppressed.
	if !open.BtimeFrozen || !open.AtimeFrozen || !open.MtimeFrozen || !open.CtimeFrozen {
		t.Fatalf("explicit set did not freeze all fields: Btime=%v Atime=%v Mtime=%v Ctime=%v",
			open.BtimeFrozen, open.AtimeFrozen, open.MtimeFrozen, open.CtimeFrozen)
	}

	// Block 2: attribute-only change (attrib=NORMAL), all timestamps zero
	// ("don't change"). This is the operation that previously auto-bumped
	// LastWriteTime and ChangeTime.
	buf2 := encodeBasicInfo(0, 0, 0, 0, types.FileAttributeNormal)
	resp, err = h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf2)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("block 2 setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	file, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	checkTime := func(name string, got, want time.Time) {
		if !got.Equal(want) {
			t.Errorf("%s = %v after attribute-only SET_INFO; want %v (explicit value must survive)", name, got.UTC(), want)
		}
	}
	checkTime("CreationTime", file.CreationTime, wantCreate)
	checkTime("Atime", file.Atime, wantAccess)
	checkTime("Mtime", file.Mtime, wantWrite)
	checkTime("Ctime", file.Ctime, wantChange)
}

// TestSetInfo_BasicInfo_ExplicitLastWriteTimeUnbumpedByAttributeSet confirms the
// targeted regression: an explicit LastWriteTime set, followed by an
// attribute-change SET_INFO, leaves the explicit Mtime in metadata rather
// than the auto-bumped "now". This isolates the LastWriteTime field that the
// pre-fix code clobbered via the attribute-change Mtime auto-bump.
func TestSetInfo_BasicInfo_ExplicitLastWriteTimeUnbumpedByAttributeSet(t *testing.T) {
	h, authCtx, fileHandle, open := setupBasicInfoTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	wantWrite := time.Date(2031, 6, 7, 8, 9, 10, 0, time.UTC)

	// Set only LastWriteTime explicitly (other times zero, no attrib).
	buf := encodeBasicInfo(0, 0, types.TimeToFiletime(wantWrite), 0, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("explicit write_time SET_INFO: err=%v status=%v", err, resp)
	}
	if !open.MtimeFrozen {
		t.Fatalf("explicit LastWriteTime set did not freeze Mtime")
	}

	// Attribute-only change that would auto-bump Mtime to now if not frozen.
	bufAttr := encodeBasicInfo(0, 0, 0, 0, types.FileAttributeHidden)
	resp, err = h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, bufAttr)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("attribute-change SET_INFO: err=%v status=%v", err, resp)
	}

	file, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Mtime.Equal(wantWrite) {
		t.Errorf("Mtime = %v after attribute change; want explicit %v (must not auto-bump)", file.Mtime.UTC(), wantWrite)
	}
}

// TestSetInfo_BasicInfo_ExplicitSetThenSentinelUnfreeze confirms the explicit
// freeze can be lifted by a subsequent -2 (unfreeze) sentinel, restoring
// automatic-update behaviour for that field.
func TestSetInfo_BasicInfo_ExplicitSetThenSentinelUnfreeze(t *testing.T) {
	h, authCtx, _, open := setupBasicInfoTimestampTest(t)

	wantWrite := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)

	// Explicit LastWriteTime → freezes Mtime.
	buf := encodeBasicInfo(0, 0, types.TimeToFiletime(wantWrite), 0, 0)
	if resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, buf); err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("explicit write_time SET_INFO: err=%v status=%v", err, resp)
	}
	if !open.MtimeFrozen {
		t.Fatalf("explicit LastWriteTime set did not freeze Mtime")
	}

	// -2 unfreeze sentinel on LastWriteTime must lift the freeze.
	bufThaw := encodeBasicInfo(0, 0, filetimeUnfreeze, 0, 0)
	if resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, bufThaw); err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("unfreeze SET_INFO: err=%v status=%v", err, resp)
	}
	if open.MtimeFrozen {
		t.Errorf("MtimeFrozen still set after -2 unfreeze; freeze must be lifted")
	}
	if open.FrozenMtime != nil {
		t.Errorf("FrozenMtime not cleared after -2 unfreeze")
	}
}
