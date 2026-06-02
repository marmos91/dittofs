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

// setupTimestampTest creates a memory-backed runtime + handler + a single
// regular file with known timestamps. Mirrors the smbtorture
// `smb2.timestamps.*` setup so the unit tests exercise the SET_INFO and
// CLOSE paths against a real metadata service.
func setupTimestampTest(t *testing.T) (*Handler, *metadata.AuthContext, metadata.FileHandle, *OpenFile) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/ts"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ts-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
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
	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "f.dat", &metadata.FileAttr{
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

	openFile := &OpenFile{
		FileID:         [16]byte{1, 2, 3, 4},
		MetadataHandle: fileHandle,
		ParentHandle:   rootHandle,
		FileName:       "f.dat",
		Path:           "f.dat",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileWriteData) | uint32(types.FileReadData) | uint32(types.Delete),
	}
	h.StoreOpenFile(openFile)

	return h, authCtx, fileHandle, openFile
}

// makeBasicInfoBuffer constructs a 40-byte FILE_BASIC_INFORMATION buffer
// with raw FILETIME slots — used for sentinel values (FREEZE/THAW) that
// would be stripped by the time.Time round-trip in EncodeFileBasicInfo.
func makeBasicInfoBuffer(creationFT, atimeFT, mtimeFT, ctimeFT uint64, fileAttrs uint32) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint64(buf[0:8], creationFT)
	binary.LittleEndian.PutUint64(buf[8:16], atimeFT)
	binary.LittleEndian.PutUint64(buf[16:24], mtimeFT)
	binary.LittleEndian.PutUint64(buf[24:32], ctimeFT)
	binary.LittleEndian.PutUint32(buf[32:36], fileAttrs)
	return buf
}

// TestSetFileInfo_FreezeThaw exercises smbtorture `smb2.timestamps.freeze-thaw`:
// after explicit timestamps are set, both FREEZE (-1) and THAW (-2) sentinels
// MUST NOT change any timestamp value (per Samba
// `lib/util/time.c::nt_time_to_full_timespec` which maps both to omit).
func TestSetFileInfo_FreezeThaw(t *testing.T) {
	h, authCtx, fileHandle, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	// Step 1: explicit set of CreationTime + Mtime to nttime.
	pinned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
		CreationTime: &pinned,
		Mtime:        &pinned,
	}); err != nil {
		t.Fatalf("seed SetFileAttributes: %v", err)
	}

	// Step 2: verify they match.
	file0, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (after seed): %v", err)
	}
	if !file0.CreationTime.Equal(pinned) {
		t.Fatalf("seed CreationTime: got %v want %v", file0.CreationTime, pinned)
	}
	if !file0.Mtime.Equal(pinned) {
		t.Fatalf("seed Mtime: got %v want %v", file0.Mtime, pinned)
	}

	// Step 3: SET_INFO with NTTIME_FREEZE (-1) for create_time + write_time.
	// Atime/Ctime are 0 (omit). FileAttributes 0.
	freezeBuf := makeBasicInfoBuffer(filetimeFreeze, 0, filetimeFreeze, 0, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, openFile, types.FileBasicInformation, freezeBuf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(FREEZE): err=%v status=%v", err, resp)
	}

	// Step 4: timestamps unchanged.
	file1, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (after FREEZE): %v", err)
	}
	if !file1.CreationTime.Equal(pinned) {
		t.Errorf("after FREEZE: CreationTime got %v want %v", file1.CreationTime, pinned)
	}
	if !file1.Mtime.Equal(pinned) {
		t.Errorf("after FREEZE: Mtime got %v want %v", file1.Mtime, pinned)
	}

	// Step 5: SET_INFO with NTTIME_THAW (-2) for create_time + write_time.
	thawBuf := makeBasicInfoBuffer(filetimeUnfreeze, 0, filetimeUnfreeze, 0, 0)
	resp, err = h.setFileInfoFromStore(nil, authCtx, openFile, types.FileBasicInformation, thawBuf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(THAW): err=%v status=%v", err, resp)
	}

	// Step 6: timestamps STILL unchanged. This is the regression caught
	// by issue #434 — previously THAW set the value to time.Now().
	file2, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (after THAW): %v", err)
	}
	if !file2.CreationTime.Equal(pinned) {
		t.Errorf("after THAW: CreationTime got %v want %v", file2.CreationTime, pinned)
	}
	if !file2.Mtime.Equal(pinned) {
		t.Errorf("after THAW: Mtime got %v want %v", file2.Mtime, pinned)
	}
}

// TestSetFileInfo_FreezeThaw_AllFields exercises THAW/FREEZE for every
// timestamp field including atime + ctime.
func TestSetFileInfo_FreezeThaw_AllFields(t *testing.T) {
	h, authCtx, fileHandle, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	pinned := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
		CreationTime: &pinned,
		Atime:        &pinned,
		Mtime:        &pinned,
		Ctime:        &pinned,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	thawAll := makeBasicInfoBuffer(filetimeUnfreeze, filetimeUnfreeze, filetimeUnfreeze, filetimeUnfreeze, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, openFile, types.FileBasicInformation, thawAll)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
	}

	file, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.CreationTime.Equal(pinned) {
		t.Errorf("CreationTime: got %v want %v", file.CreationTime, pinned)
	}
	if !file.Atime.Equal(pinned) {
		t.Errorf("Atime: got %v want %v", file.Atime, pinned)
	}
	if !file.Mtime.Equal(pinned) {
		t.Errorf("Mtime: got %v want %v", file.Mtime, pinned)
	}
	if !file.Ctime.Equal(pinned) {
		t.Errorf("Ctime: got %v want %v", file.Ctime, pinned)
	}
}

// TestDelayedWrite_TwoWritesProduceDistinctTimestamps mirrors smbtorture
// `delayed-2write`: two writes separated in time MUST produce distinct
// LastWriteTime values, and a subsequent CLOSE MUST NOT bump it past the
// second write's timestamp. This test drives the metadata service's
// Prepare/Commit/Flush sequence directly so the deferred-commit pending
// state is exercised end-to-end.
func TestDelayedWrite_TwoWritesProduceDistinctTimestamps(t *testing.T) {
	h, authCtx, fileHandle, _ := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	// First write.
	op1, err := metaSvc.PrepareWrite(authCtx, fileHandle, 1)
	if err != nil {
		t.Fatalf("PrepareWrite #1: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, op1); err != nil {
		t.Fatalf("CommitWrite #1: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("FlushPendingWriteForFile #1: %v", err)
	}

	file1, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after write1: %v", err)
	}
	writeTime1 := file1.Mtime

	// Wait long enough that time.Now() advances past the previous mtime.
	// Real smbtorture sleeps 3s; for the unit test we only need to clear
	// the FILETIME 100ns granularity.
	time.Sleep(20 * time.Millisecond)

	// Second write.
	op2, err := metaSvc.PrepareWrite(authCtx, fileHandle, 1)
	if err != nil {
		t.Fatalf("PrepareWrite #2: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, op2); err != nil {
		t.Fatalf("CommitWrite #2: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("FlushPendingWriteForFile #2: %v", err)
	}

	file2, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after write2: %v", err)
	}
	writeTime2 := file2.Mtime

	if !writeTime2.After(writeTime1) {
		t.Errorf("second write did not advance LastWriteTime: w1=%v w2=%v",
			writeTime1, writeTime2)
	}

	// Simulate CLOSE: another GetFile after a delay. Mtime MUST NOT advance.
	time.Sleep(10 * time.Millisecond)
	file3, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after close-delay: %v", err)
	}
	if !file3.Mtime.Equal(writeTime2) {
		t.Errorf("CLOSE-time read changed Mtime: w2=%v close=%v",
			writeTime2, file3.Mtime)
	}
}

// TestDelayedWrite_VsFlush mirrors smbtorture `delayed-write-vs-flush`:
// after a WRITE updates LastWriteTime, an explicit metadata flush MUST
// NOT change it.
func TestDelayedWrite_VsFlush(t *testing.T) {
	h, authCtx, fileHandle, _ := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	op, err := metaSvc.PrepareWrite(authCtx, fileHandle, 1)
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, op); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	file1, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after write: %v", err)
	}
	writeTime := file1.Mtime

	// FLUSH equivalent — re-flush an empty pending state.
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	file2, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after flush: %v", err)
	}
	if !file2.Mtime.Equal(writeTime) {
		t.Errorf("flush bumped Mtime: pre=%v post=%v", writeTime, file2.Mtime)
	}
}

// TestDelayedWrite_VsSetEOF mirrors smbtorture `delayed-write-vs-seteof`:
// SET_INFO EndOfFile (even with the same size) MUST update LastWriteTime;
// a subsequent SET_INFO BasicInfo on a second handle that pins write_time
// to a chosen value MUST be reflected in the first handle's CLOSE
// response (the explicit set wins over delayed-write recovery).
func TestDelayedWrite_VsSetEOF(t *testing.T) {
	h, authCtx, fileHandle, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	// SetEOF size=0 — same as current. Expect Mtime to bump.
	zero := uint64(0)
	pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile pre-seteof: %v", err)
	}
	preMtime := pre.Mtime
	time.Sleep(20 * time.Millisecond)
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Size: &zero}); err != nil {
		t.Fatalf("SetEOF #1: %v", err)
	}
	post, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile post-seteof: %v", err)
	}
	if !post.Mtime.After(preMtime) {
		t.Errorf("SetEOF (same size) did not bump Mtime: pre=%v post=%v", preMtime, post.Mtime)
	}

	// Simulate a WRITE then another SetEOF.
	op, err := metaSvc.PrepareWrite(authCtx, fileHandle, 1)
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, op); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	preWrite2, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile preWrite2: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	one := uint64(1)
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Size: &one}); err != nil {
		t.Fatalf("SetEOF #2: %v", err)
	}
	postWrite2, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile postWrite2: %v", err)
	}
	if !postWrite2.Mtime.After(preWrite2.Mtime) {
		t.Errorf("SetEOF #2 did not bump Mtime past write time: w2=%v post=%v",
			preWrite2.Mtime, postWrite2.Mtime)
	}

	// SetBasic with explicit future write_time. SetFileAttributes pins
	// file.Mtime to the explicit value regardless of which handle issues it.
	setTime := time.Now().UTC().Add(86400 * time.Second).Truncate(time.Microsecond)
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Mtime: &setTime}); err != nil {
		t.Fatalf("SetBasic h2: %v", err)
	}

	// On subsequent GetFile, the pinned set_time must surface — not the
	// previous delayed-write value.
	closeFile, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile close: %v", err)
	}
	if !closeFile.Mtime.Equal(setTime) {
		t.Errorf("close Mtime should equal explicit set_time: got %v want %v",
			closeFile.Mtime, setTime)
	}

	// applyFrozenTimestamps MUST be a no-op when nothing is frozen.
	applyFrozenTimestamps(openFile, closeFile)
	if !closeFile.Mtime.Equal(setTime) {
		t.Errorf("applyFrozenTimestamps mutated unfrozen Mtime: got %v want %v",
			closeFile.Mtime, setTime)
	}
}

// TestSetFileInfo_DelayedWriteVsSetbasic exercises the smbtorture
// `delayed-write-vs-setbasic` flow: after a WRITE has updated mtime, a
// subsequent SET_INFO BasicInfo with all-zero timestamps (or only one
// non-write field set) MUST NOT bump mtime.
func TestSetFileInfo_DelayedWriteVsSetbasic(t *testing.T) {
	h, authCtx, fileHandle, openFile := setupTimestampTest(t)
	metaSvc := h.Registry.GetMetadataService()

	// Simulate a WRITE: drive mtime to a known recent value.
	writeTime := time.Now().Add(-time.Hour).UTC()
	if _, err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
		Mtime: &writeTime,
	}); err != nil {
		t.Fatalf("seed Mtime: %v", err)
	}

	cases := []struct {
		name string
		buf  []byte
	}{
		{
			name: "all-zero",
			buf:  makeBasicInfoBuffer(0, 0, 0, 0, 0),
		},
		{
			name: "set create_time only",
			buf: makeBasicInfoBuffer(
				types.TimeToFiletime(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				0, 0, 0, 0,
			),
		},
		{
			name: "set access_time only",
			buf: makeBasicInfoBuffer(
				0,
				types.TimeToFiletime(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)),
				0, 0, 0,
			),
		},
		{
			name: "set change_time only",
			buf: makeBasicInfoBuffer(
				0, 0, 0,
				types.TimeToFiletime(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
				0,
			),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pre, err := metaSvc.GetFile(authCtx.Context, fileHandle)
			if err != nil {
				t.Fatalf("GetFile pre: %v", err)
			}
			preMtime := pre.Mtime

			resp, err := h.setFileInfoFromStore(nil, authCtx, openFile, types.FileBasicInformation, tc.buf)
			if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
				t.Fatalf("setFileInfoFromStore: err=%v status=%v", err, resp)
			}

			post, err := metaSvc.GetFile(authCtx.Context, fileHandle)
			if err != nil {
				t.Fatalf("GetFile post: %v", err)
			}
			if !post.Mtime.Equal(preMtime) {
				t.Errorf("write_time changed across SetBasic: pre=%v post=%v", preMtime, post.Mtime)
			}
		})
	}
}

// TestDirFreezeTimestamps_ChildCreate_AllFrozen verifies that freezing ALL
// directory timestamps via SET_INFO(-1) survives a child file creation.
func TestDirFreezeTimestamps_ChildCreate_AllFrozen(t *testing.T) {
	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/ts"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ts-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
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

	// Step 1: Create a subdirectory.
	dir, _, err := metaSvc.CreateDirectory(authCtx, rootHandle, "testdir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	dirHandle, err := metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle(dir): %v", err)
	}

	// Pin timestamps to a well-known value so assertions are deterministic.
	pinned := time.Date(2026, 1, 2, 13, 40, 49, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(authCtx, dirHandle, &metadata.SetAttrs{
		CreationTime: &pinned,
		Mtime:        &pinned,
		Atime:        &pinned,
		Ctime:        &pinned,
	}); err != nil {
		t.Fatalf("pin timestamps: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	// Step 2: "Open" the directory (create an OpenFile).
	dirOpenFile := &OpenFile{
		FileID:         [16]byte{0xAA},
		MetadataHandle: dirHandle,
		ParentHandle:   rootHandle,
		FileName:       "testdir",
		Path:           "testdir",
		ShareName:      shareName,
		IsDirectory:    true,
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes),
	}
	h.StoreOpenFile(dirOpenFile)

	// Step 3: Freeze all four timestamps via SET_INFO(-1).
	freezeAll := makeBasicInfoBuffer(filetimeFreeze, filetimeFreeze, filetimeFreeze, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, dirOpenFile, types.FileBasicInformation, freezeAll)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v status=%v", err, resp)
	}

	// Verify frozen state was recorded.
	if !dirOpenFile.BtimeFrozen || dirOpenFile.FrozenBtime == nil {
		t.Fatalf("BtimeFrozen not set after freeze")
	}
	if !dirOpenFile.MtimeFrozen || dirOpenFile.FrozenMtime == nil {
		t.Fatalf("MtimeFrozen not set after freeze")
	}
	if !dirOpenFile.AtimeFrozen || dirOpenFile.FrozenAtime == nil {
		t.Fatalf("AtimeFrozen not set after freeze")
	}
	if !dirOpenFile.CtimeFrozen || dirOpenFile.FrozenCtime == nil {
		t.Fatalf("CtimeFrozen not set after freeze")
	}

	// Step 4: Create a child file -- this updates the directory's
	// Mtime/Ctime/Atime in the metadata store.
	if _, _, createErr := metaSvc.CreateFile(authCtx, dirHandle, "child.dat", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	}); createErr != nil {
		t.Fatalf("CreateFile(child): %v", createErr)
	}

	// Step 4a: restoreParentDirFrozenTimestamps -- mirrors the CREATE handler.
	h.restoreParentDirFrozenTimestamps(authCtx, dirHandle)

	// Step 5: Read the directory from the store and apply frozen overrides
	// (same as QUERY_INFO handler).
	dirFile, err := metaSvc.GetFile(authCtx.Context, dirHandle)
	if err != nil {
		t.Fatalf("GetFile(dir) after child create: %v", err)
	}
	applyFrozenTimestamps(dirOpenFile, dirFile)

	// Verify all four timestamps are still the pinned value.
	if !dirFile.CreationTime.Equal(pinned) {
		t.Errorf("CreationTime changed: got %v want %v", dirFile.CreationTime, pinned)
	}
	if !dirFile.Mtime.Equal(pinned) {
		t.Errorf("Mtime changed: got %v want %v", dirFile.Mtime, pinned)
	}
	if !dirFile.Atime.Equal(pinned) {
		t.Errorf("Atime changed: got %v want %v", dirFile.Atime, pinned)
	}
	if !dirFile.Ctime.Equal(pinned) {
		t.Errorf("Ctime changed: got %v want %v", dirFile.Ctime, pinned)
	}

	// Also verify the raw store values (before applyFrozenTimestamps) for
	// fields that are frozen.
	rawDir, err := metaSvc.GetFile(authCtx.Context, dirHandle)
	if err != nil {
		t.Fatalf("GetFile(dir) raw: %v", err)
	}
	// CreationTime is never modified by createEntry, so it should be
	// the pinned value in the store regardless of freeze.
	if !rawDir.CreationTime.Equal(pinned) {
		t.Errorf("store CreationTime changed: got %v want %v", rawDir.CreationTime, pinned)
	}
	// Mtime, Ctime, Atime are updated by createEntry but restored by
	// restoreParentDirFrozenTimestamps. Check restoration.
	if !rawDir.Mtime.Equal(pinned) {
		t.Errorf("store Mtime not restored: got %v want %v", rawDir.Mtime, pinned)
	}
	if !rawDir.Atime.Equal(pinned) {
		t.Errorf("store Atime not restored: got %v want %v", rawDir.Atime, pinned)
	}
	// Ctime may be auto-updated by SetFileAttributes to time.Now() if the
	// restore call only sets some fields. Check it was properly restored.
	if !rawDir.Ctime.Equal(pinned) {
		t.Errorf("store Ctime not restored: got %v want %v", rawDir.Ctime, pinned)
	}
}

// TestDirFreezeTimestamps_ChildCreate_WalkPath verifies that the parentHandle
// from walkPath matches the directory's MetadataHandle so that
// restoreParentDirFrozenTimestamps can find and restore frozen timestamps.
func TestDirFreezeTimestamps_ChildCreate_WalkPath(t *testing.T) {
	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/ts"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ts-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:                context.Background(),
		Identity:               &metadata.Identity{UID: &uid, GID: &gid},
		BypassTraverseChecking: true,
	}
	metaSvc := rt.GetMetadataService()

	// Create subdirectory.
	dir, _, err := metaSvc.CreateDirectory(authCtx, rootHandle, "testdir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	dirHandle, err := metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// Pin and freeze all timestamps.
	pinned := time.Date(2026, 1, 2, 13, 40, 49, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(authCtx, dirHandle, &metadata.SetAttrs{
		CreationTime: &pinned, Mtime: &pinned, Atime: &pinned, Ctime: &pinned,
	}); err != nil {
		t.Fatalf("pin: %v", err)
	}

	h := NewHandler()
	h.Registry = rt
	dirOpenFile := &OpenFile{
		FileID:         [16]byte{0xCC},
		MetadataHandle: dirHandle,
		ParentHandle:   rootHandle,
		FileName:       "testdir",
		Path:           "testdir",
		ShareName:      shareName,
		IsDirectory:    true,
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes),
	}
	h.StoreOpenFile(dirOpenFile)

	freezeAll := makeBasicInfoBuffer(filetimeFreeze, filetimeFreeze, filetimeFreeze, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, dirOpenFile, types.FileBasicInformation, freezeAll)
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze: err=%v status=%v", err, resp)
	}

	// Resolve parentHandle via walkPath (as the CREATE handler would for
	// "testdir/child.dat").
	walkedHandle, walkErr := h.walkPath(authCtx, rootHandle, "testdir")
	if walkErr != nil {
		t.Fatalf("walkPath: %v", walkErr)
	}

	// Verify walkedHandle matches the directory's MetadataHandle.
	if string(walkedHandle) != string(dirHandle) {
		t.Fatalf("walkPath handle mismatch: walked=%q dir=%q", walkedHandle, dirHandle)
	}

	// Create child and restore (using walked handle as the CREATE handler would).
	if _, _, createErr := metaSvc.CreateFile(authCtx, walkedHandle, "child.dat", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	}); createErr != nil {
		t.Fatalf("CreateFile: %v", createErr)
	}
	h.restoreParentDirFrozenTimestamps(authCtx, walkedHandle)

	dirFile, err := metaSvc.GetFile(authCtx.Context, dirHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	applyFrozenTimestamps(dirOpenFile, dirFile)

	if !dirFile.CreationTime.Equal(pinned) {
		t.Errorf("CreationTime: got %v want %v", dirFile.CreationTime, pinned)
	}
	if !dirFile.Mtime.Equal(pinned) {
		t.Errorf("Mtime: got %v want %v", dirFile.Mtime, pinned)
	}
	if !dirFile.Atime.Equal(pinned) {
		t.Errorf("Atime: got %v want %v", dirFile.Atime, pinned)
	}
	if !dirFile.Ctime.Equal(pinned) {
		t.Errorf("Ctime: got %v want %v", dirFile.Ctime, pinned)
	}
}

// TestDirFreezeTimestamps_ChildCreate_SingleField verifies that freezing a
// SINGLE directory timestamp via SET_INFO(-1) survives a child file creation.
// This mirrors the WPTS tests which freeze only one timestamp at a time.
// The key concern is that restoreParentDirFrozenTimestamps calls
// SetFileAttributes with only the frozen field, which may trigger a Ctime
// auto-update side effect for the non-frozen fields.
func TestDirFreezeTimestamps_ChildCreate_SingleField(t *testing.T) {
	cases := []struct {
		name      string
		freezeBuf []byte
		checkFn   func(t *testing.T, file *metadata.File, pinned time.Time)
	}{
		{
			name:      "CreationTime",
			freezeBuf: makeBasicInfoBuffer(filetimeFreeze, 0, 0, 0, 0),
			checkFn: func(t *testing.T, file *metadata.File, pinned time.Time) {
				if !file.CreationTime.Equal(pinned) {
					t.Errorf("CreationTime changed: got %v want %v", file.CreationTime, pinned)
				}
			},
		},
		{
			name:      "LastWriteTime",
			freezeBuf: makeBasicInfoBuffer(0, 0, filetimeFreeze, 0, 0),
			checkFn: func(t *testing.T, file *metadata.File, pinned time.Time) {
				if !file.Mtime.Equal(pinned) {
					t.Errorf("Mtime changed: got %v want %v", file.Mtime, pinned)
				}
			},
		},
		{
			name:      "LastAccessTime",
			freezeBuf: makeBasicInfoBuffer(0, filetimeFreeze, 0, 0, 0),
			checkFn: func(t *testing.T, file *metadata.File, pinned time.Time) {
				if !file.Atime.Equal(pinned) {
					t.Errorf("Atime changed: got %v want %v", file.Atime, pinned)
				}
			},
		},
		{
			name:      "ChangeTime",
			freezeBuf: makeBasicInfoBuffer(0, 0, 0, filetimeFreeze, 0),
			checkFn: func(t *testing.T, file *metadata.File, pinned time.Time) {
				if !file.Ctime.Equal(pinned) {
					t.Errorf("Ctime changed: got %v want %v", file.Ctime, pinned)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.New(nil)
			memStore := memory.NewMemoryMetadataStoreWithDefaults()
			if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
				t.Fatalf("RegisterMetadataStore: %v", err)
			}
			shareName := "/ts"
			if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
				Name:          shareName,
				MetadataStore: "ts-meta",
				RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
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

			// Create subdirectory and pin all timestamps.
			dir, _, err := metaSvc.CreateDirectory(authCtx, rootHandle, "testdir", &metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
				Mode: 0o755,
			})
			if err != nil {
				t.Fatalf("CreateDirectory: %v", err)
			}
			dirHandle, err := metadata.EncodeFileHandle(dir)
			if err != nil {
				t.Fatalf("EncodeFileHandle: %v", err)
			}
			pinned := time.Date(2026, 1, 2, 13, 40, 49, 0, time.UTC)
			if _, err := metaSvc.SetFileAttributes(authCtx, dirHandle, &metadata.SetAttrs{
				CreationTime: &pinned,
				Mtime:        &pinned,
				Atime:        &pinned,
				Ctime:        &pinned,
			}); err != nil {
				t.Fatalf("pin timestamps: %v", err)
			}

			h := NewHandler()
			h.Registry = rt
			dirOpenFile := &OpenFile{
				FileID:         [16]byte{0xBB},
				MetadataHandle: dirHandle,
				ParentHandle:   rootHandle,
				FileName:       "testdir",
				Path:           "testdir",
				ShareName:      shareName,
				IsDirectory:    true,
				DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes),
			}
			h.StoreOpenFile(dirOpenFile)

			// Freeze only the target timestamp.
			resp, sErr := h.setFileInfoFromStore(nil, authCtx, dirOpenFile, types.FileBasicInformation, tc.freezeBuf)
			if sErr != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
				t.Fatalf("freeze SET_INFO: err=%v status=%v", sErr, resp)
			}

			// Create child file (updates dir Mtime/Ctime/Atime).
			if _, _, createErr := metaSvc.CreateFile(authCtx, dirHandle, "child.dat", &metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0o644,
			}); createErr != nil {
				t.Fatalf("CreateFile(child): %v", createErr)
			}

			// Restore frozen timestamps (mirrors CREATE handler).
			h.restoreParentDirFrozenTimestamps(authCtx, dirHandle)

			// Read file and apply frozen overrides (mirrors QUERY_INFO).
			dirFile, err := metaSvc.GetFile(authCtx.Context, dirHandle)
			if err != nil {
				t.Fatalf("GetFile: %v", err)
			}
			applyFrozenTimestamps(dirOpenFile, dirFile)

			// The frozen timestamp must still be the pinned value.
			tc.checkFn(t, dirFile, pinned)
		})
	}
}

// TestUpdateBaseObjectTimestampsForADSWrite_PreservesBaseCtimeWhenFrozen
// reproduces the WPTS scenario in
// `FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime`:
//
//  1. The client opens an ADS handle on a base object (directory or file).
//  2. The client freezes ChangeTime on the ADS handle via SET_INFO -1.
//  3. The client writes data to the ADS.
//
// Per MS-FSA 2.1.5.14.2 the freeze sentinel applies to the underlying object,
// so the base's ChangeTime must not be bumped by the ADS write. The metadata
// layer auto-bumps Ctime when SetFileAttributes is called with any modified
// attribute and attrs.Ctime == nil; updateBaseObjectTimestampsForADSWrite
// must therefore explicitly pin the base's Ctime when the ADS handle has it
// frozen but Mtime is not.
func TestUpdateBaseObjectTimestampsForADSWrite_PreservesBaseCtimeWhenFrozen(t *testing.T) {
	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ts-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/ts"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "ts-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:                context.Background(),
		Identity:               &metadata.Identity{UID: &uid, GID: &gid},
		BypassTraverseChecking: true,
	}
	metaSvc := rt.GetMetadataService()

	// Create the base directory (mirrors WPTS test: directory hosts the ADS).
	baseDir, _, err := metaSvc.CreateDirectory(authCtx, rootHandle, "basedir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	baseHandle, err := metadata.EncodeFileHandle(baseDir)
	if err != nil {
		t.Fatalf("EncodeFileHandle base: %v", err)
	}

	// Create the ADS as a sibling entry under root (matches CREATE handler's
	// stream-entry layout: stream `basedir:streamname` is a root child).
	streamFile, _, err := metaSvc.CreateFile(authCtx, rootHandle, "basedir:streamname", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile stream: %v", err)
	}
	streamHandle, err := metadata.EncodeFileHandle(streamFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle stream: %v", err)
	}

	// Pin the base directory's Ctime to a known value.
	pinned := time.Date(2026, 1, 2, 13, 40, 49, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(authCtx, baseHandle, &metadata.SetAttrs{
		Ctime: &pinned, Mtime: &pinned, CreationTime: &pinned, Atime: &pinned,
	}); err != nil {
		t.Fatalf("pin base: %v", err)
	}

	h := NewHandler()
	h.Registry = rt
	adsOpen := &OpenFile{
		FileID:         [16]byte{0xAD, 0x5C},
		MetadataHandle: streamHandle,
		ParentHandle:   rootHandle,
		FileName:       "basedir:streamname",
		Path:           "basedir:streamname",
		ShareName:      shareName,
		IsDirectory:    false,
		DesiredAccess:  uint32(types.FileWriteData) | uint32(types.FileWriteAttributes),
		CtimeFrozen:    true, // SET_INFO -1 on ChangeTime
		FrozenCtime:    &pinned,
	}
	h.StoreOpenFile(adsOpen)

	// Trigger the WRITE-path base-timestamp update.
	h.updateBaseObjectTimestampsForADSWrite(authCtx, metaSvc, adsOpen, "basedir")

	// Verify the base directory's Ctime is unchanged but Mtime was advanced
	// (because only Ctime is frozen on the ADS handle).
	post, err := metaSvc.GetFile(authCtx.Context, baseHandle)
	if err != nil {
		t.Fatalf("GetFile post: %v", err)
	}
	if !post.Ctime.Equal(pinned) {
		t.Errorf("base Ctime was bumped: got %v want %v (frozen)", post.Ctime, pinned)
	}
	if post.Mtime.Equal(pinned) {
		t.Errorf("base Mtime was not advanced: got %v want != %v", post.Mtime, pinned)
	}
}
