package handlers

import (
	"context"
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
	file, err := metaSvc.CreateFile(authCtx, rootHandle, "f.dat", &metadata.FileAttr{
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
	putU64 := func(off int, v uint64) {
		for i := 0; i < 8; i++ {
			buf[off+i] = byte(v >> (8 * i))
		}
	}
	putU64(0, creationFT)
	putU64(8, atimeFT)
	putU64(16, mtimeFT)
	putU64(24, ctimeFT)
	// FileAttributes at offset 32, Reserved at offset 36
	for i := 0; i < 4; i++ {
		buf[32+i] = byte(fileAttrs >> (8 * i))
	}
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
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
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
	resp, err := h.setFileInfoFromStore(authCtx, openFile, types.FileBasicInformation, freezeBuf)
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
	resp, err = h.setFileInfoFromStore(authCtx, openFile, types.FileBasicInformation, thawBuf)
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
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
		CreationTime: &pinned,
		Atime:        &pinned,
		Mtime:        &pinned,
		Ctime:        &pinned,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	thawAll := makeBasicInfoBuffer(filetimeUnfreeze, filetimeUnfreeze, filetimeUnfreeze, filetimeUnfreeze, 0)
	resp, err := h.setFileInfoFromStore(authCtx, openFile, types.FileBasicInformation, thawAll)
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
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Size: &zero}); err != nil {
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
	preWrite2, _ := metaSvc.GetFile(authCtx.Context, fileHandle)
	time.Sleep(20 * time.Millisecond)

	one := uint64(1)
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Size: &one}); err != nil {
		t.Fatalf("SetEOF #2: %v", err)
	}
	postWrite2, _ := metaSvc.GetFile(authCtx.Context, fileHandle)
	if !postWrite2.Mtime.After(preWrite2.Mtime) {
		t.Errorf("SetEOF #2 did not bump Mtime past write time: w2=%v post=%v",
			preWrite2.Mtime, postWrite2.Mtime)
	}

	// h2 SetBasic with explicit future write_time. SetFileAttributes from
	// any handle pins file.Mtime to the explicit value.
	setTime := time.Now().UTC().Add(86400 * time.Second).Truncate(time.Microsecond)
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{Mtime: &setTime}); err != nil {
		t.Fatalf("SetBasic h2: %v", err)
	}

	// h1 close: GetFile must reflect the pinned set_time, not the previous
	// delayed-write value.
	closeFile, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile close: %v", err)
	}
	if !closeFile.Mtime.Equal(setTime) {
		t.Errorf("close Mtime should equal explicit set_time: got %v want %v",
			closeFile.Mtime, setTime)
	}

	// Sanity check that openFile is still in the handler (no stale-state
	// regression caused by my changes — applyFrozenTimestamps must be a
	// no-op when no field is frozen).
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
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
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

			resp, err := h.setFileInfoFromStore(authCtx, openFile, types.FileBasicInformation, tc.buf)
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
