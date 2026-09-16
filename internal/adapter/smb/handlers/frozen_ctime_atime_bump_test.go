// Handler-level coverage for a ChangeTime frozen with the -1 sentinel
// surviving the LastAccessTime bump that READ and WRITE make afterwards.
//
// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation") a frozen timestamp must not be
// updated by later operations on the handle. The access-time bump is an
// attribute write like any other, so the metadata layer stamps ChangeTime = now
// for it unless the caller names a ChangeTime of its own. These tests assert on
// the stored value right after the operation, not after CLOSE: a frozen
// ChangeTime that is wrong in the store until CLOSE repairs it is visible to
// every other reader in between, and any concurrent read-modify-write of the
// row makes it permanent.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// freezeCtimeOnSeededFile hands the fixture's file to the session's user, pins
// all four timestamps to a fixed past instant, and freezes ChangeTime with the
// -1 sentinel. Returns the frozen instant.
func freezeCtimeOnSeededFile(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) time.Time {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()

	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	sessUID, sessGID := uint32(1000), uint32(1000)
	past := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(rootCtx, openFile.MetadataHandle, &metadata.SetAttrs{
		UID: &sessUID, GID: &sessGID,
		CreationTime: &past, Atime: &past, Mtime: &past, Ctime: &past,
	}); err != nil {
		t.Fatalf("seed owner + timestamps: %v", err)
	}

	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	// Only ChangeTime carries the sentinel; the other three stay zero ("do not
	// change"), which is the case where the metadata layer's automatic stamp
	// would otherwise fire.
	freeze := encodeBasicInfo(0, 0, 0, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileBasicInformation, freeze)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v resp=%v", err, resp)
	}
	if !openFile.CtimeFrozen {
		t.Fatal("sentinel did not freeze ChangeTime")
	}
	return past
}

// grantFullAccess gives the handle the rights READ/WRITE, the
// explicit-timestamp SET_INFO path, and the EA path require.
func grantFullAccess(h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) {
	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileWriteEA | types.FileReadEA |
		types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	openFile.DesiredAccess = access
	openFile.GrantedAccess = access
	h.primeAuthContextFromOpenFile(smbCtx, openFile)
}

// assertCtimeUnmoved reads the file back and reports the ChangeTime a client
// would observe now, failing if op moved it off the frozen instant. Returns the
// file so callers can layer further assertions on the same read.
func assertCtimeUnmoved(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) *metadata.File {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after %s: %v", op, err)
	}
	if !file.Ctime.Equal(frozen) {
		t.Errorf("ChangeTime = %v after %s; want the frozen %v", file.Ctime.UTC(), op, frozen)
	}
	return file
}

// assertFrozenCtimeSurvived reads the file back from the store and checks that
// op left the frozen ChangeTime alone while still landing its LastAccessTime
// bump.
func assertFrozenCtimeSurvived(t *testing.T, h *Handler, openFile *OpenFile, frozen time.Time, op string) {
	t.Helper()
	file := assertCtimeUnmoved(t, h, openFile, frozen, op)
	if !file.Atime.After(frozen) {
		t.Errorf("LastAccessTime = %v after %s; the bump should still have landed", file.Atime.UTC(), op)
	}
}

func TestFrozenChangeTime_SurvivesWriteAtimeBump(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)
	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Write: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, openFile, frozen, "WRITE")
}

func TestFrozenChangeTime_SurvivesReadAtimeBump(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	// Give the file something to read back.
	if resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)}); err != nil ||
		resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}
	// That write already bumped LastAccessTime, and successive bumps on one
	// handle coalesce for smbAtimeUpdateWindow. Clear the mark so the READ
	// below performs the store write this test is about.
	openFile.SmbAtimeWrittenAt = time.Time{}

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	resp, err := h.Read(smbCtx, &ReadRequest{FileID: fileID, Offset: 0, Length: 4096})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Read: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, openFile, frozen, "READ")
}

func TestFrozenChangeTime_SurvivesQueryDirectoryAtimeBump(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	fileOpen, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}

	// A directory handle on the share root, alongside the fixture's file handle
	// so the enumeration has an entry to return.
	dirID := [16]byte{2}
	dirOpen := (&OpenFile{
		FileID: dirID, TreeID: fileOpen.TreeID, SessionID: fileOpen.SessionID,
		ShareName: fileOpen.ShareName, MetadataHandle: rootHandle, IsDirectory: true,
	}).WithName(OpenName{Path: "/", FileName: "/"})
	h.StoreOpenFile(dirOpen)
	grantFullAccess(h, smbCtx, dirOpen)

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, dirOpen)

	resp, err := h.QueryDirectory(smbCtx, &QueryDirectoryRequest{
		FileID: dirID, FileInfoClass: uint8(types.FileBothDirectoryInformation),
		FileName: "*", OutputBufferLength: 65536, Flags: uint8(types.SMB2RestartScans),
	})
	if err != nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("QueryDirectory: err=%v status=%v", err, resp.GetStatus())
	}

	assertFrozenCtimeSurvived(t, h, dirOpen, frozen, "QUERY_DIRECTORY")
}

// A freeze binds the handle that set it, not the file. When another opener
// legitimately advances ChangeTime after the freeze, the frozen handle's next
// RESTORING operation must leave that newer value alone — not write the value
// it froze back over it. NFSv4 encodes its change attribute from Ctime
// (RFC 7530 §5.8.1.4 requires it to increase), so dragging the stored value
// backwards lets a client keep serving a cache it should have dropped.
//
// FSCTL_SET_ZERO_DATA drives this deliberately: it writes through CommitWrite,
// which stamps ChangeTime where no PreserveCtime reaches, so the handler repairs
// it with restoreFrozenTimestamps afterwards. That is the mechanism that can
// walk a peer's advance backwards. A hold-only operation cannot violate this
// property at all — it writes no value — so driving the assertion through one
// (READ, FSCTL_SET_SPARSE) proves nothing about the restore.
func TestFrozenChangeTime_DoesNotRollBackAPeersAdvance(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	// SET_ZERO_DATA clamps to the current size, so the file needs bytes before
	// the punch has anything to do.
	if resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)}); err != nil ||
		resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	// Another opener advances ChangeTime after the freeze.
	metaSvc := h.Registry.GetMetadataService()
	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	advanced := frozen.Add(72 * time.Hour)
	if _, err := metaSvc.SetFileAttributes(rootCtx, openFile.MetadataHandle, &metadata.SetAttrs{
		Ctime: &advanced,
	}); err != nil {
		t.Fatalf("peer ChangeTime advance: %v", err)
	}

	input := make([]byte, fileZeroDataBufSize)
	// FileOffset 0, BeyondFinalZero 4096, both little-endian uint64.
	input[8] = 0x00
	input[9] = 0x10
	body := buildIoctlRequestBody(FsctlSetZeroData, fileID, input, 0)
	res, err := h.handleSetZeroData(smbCtx, body)
	if err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("handleSetZeroData: err=%v status=0x%08x", err, uint32(res.Status))
	}

	file, err := metaSvc.GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after FSCTL_SET_ZERO_DATA: %v", err)
	}
	if file.Ctime.Before(advanced) {
		t.Errorf("FSCTL_SET_ZERO_DATA moved ChangeTime backwards to %v; the peer had advanced it to %v",
			file.Ctime.UTC(), advanced.UTC())
	}
}

// freezeCtimeOnHandle seeds the file behind openFile with a fixed past instant
// and freezes its ChangeTime with the -1 sentinel, without changing ownership —
// for fixtures whose files are already owned by the session's user.
func freezeCtimeOnHandle(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openFile *OpenFile) time.Time {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()

	uid, gid := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	past := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(rootCtx, openFile.MetadataHandle, &metadata.SetAttrs{
		CreationTime: &past, Atime: &past, Mtime: &past, Ctime: &past,
	}); err != nil {
		t.Fatalf("seed timestamps: %v", err)
	}

	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	openFile.DesiredAccess = access
	openFile.GrantedAccess = access
	h.primeAuthContextFromOpenFile(smbCtx, openFile)
	authCtx, err := BuildAuthContext(smbCtx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	freeze := encodeBasicInfo(0, 0, 0, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(smbCtx, authCtx, openFile, types.FileBasicInformation, freeze)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v resp=%v", err, resp)
	}
	if !openFile.CtimeFrozen {
		t.Fatal("sentinel did not freeze ChangeTime")
	}
	return past
}

// COPYCHUNK bumps LastAccessTime on both the source and the destination handle.
// Neither may move a ChangeTime frozen on its own handle. The destination's
// bump runs after restoreFrozenTimestamps, so without the hold it undoes that
// restore; the source has no restore at all, so without the hold it is lost
// outright.
func TestFrozenChangeTime_SurvivesCopyChunkAtimeBumps(t *testing.T) {
	h, smbCtx, srcOpen, dstOpen := setupCopyChunkFrozenFixture(t)

	srcFrozen := freezeCtimeOnHandle(t, h, smbCtx, srcOpen)
	dstFrozen := freezeCtimeOnHandle(t, h, smbCtx, dstOpen)

	chunks := []copyChunk{{SourceOffset: 0, TargetOffset: 0, Length: 4096}}
	res, err := h.executeCopyChunks(smbCtx, FsctlSrvCopyChunk, dstOpen.FileID, srcOpen, dstOpen, chunks)
	if err != nil {
		t.Fatalf("executeCopyChunks: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("executeCopyChunks status = 0x%08x, want SUCCESS", uint32(res.Status))
	}

	metaSvc := h.Registry.GetMetadataService()
	for _, tc := range []struct {
		name   string
		open   *OpenFile
		frozen time.Time
	}{
		{"source", srcOpen, srcFrozen},
		{"destination", dstOpen, dstFrozen},
	} {
		file, err := metaSvc.GetFile(context.Background(), tc.open.MetadataHandle)
		if err != nil {
			t.Fatalf("GetFile %s: %v", tc.name, err)
		}
		if !file.Ctime.Equal(tc.frozen) {
			t.Errorf("%s ChangeTime = %v after COPYCHUNK; want the frozen %v",
				tc.name, file.Ctime.UTC(), tc.frozen)
		}
	}
}

// CLOSE flushes the access time a coalesced READ left held on the handle. That
// flush is an attribute write like any other, so it must not move a frozen
// ChangeTime.
//
// Note on what this pins: restoreFrozenTimestamps runs a few lines after the
// flush inside the same CLOSE and writes the same frozen value, so the END state
// is correct with or without the hold on this path — removing the hold does not
// make this test fail. It covers the coalesced-flush-through-CLOSE path, which
// nothing else exercises, and would catch a regression in the flush or in the
// restore. The hold itself is there to keep the forbidden value out of the store
// in the window between the two writes, which no end-state assertion can see.
func TestFrozenChangeTime_SurvivesCloseAtimeFlush(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("open file missing")
	}
	grantFullAccess(h, smbCtx, openFile)

	if resp, err := h.Write(smbCtx, &WriteRequest{FileID: fileID, Offset: 0, Data: make([]byte, 4096)}); err != nil ||
		resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("seed Write: err=%v resp=%v", err, resp)
	}
	// Two READs inside the coalescing window: the first writes the access time
	// through, the second is suppressed and leaves its value held on the handle
	// for CLOSE to flush.
	openFile.SmbAtimeWrittenAt = time.Time{}
	for i := range 2 {
		if resp, err := h.Read(smbCtx, &ReadRequest{FileID: fileID, Offset: 0, Length: 4096}); err != nil ||
			resp.GetStatus() != types.StatusSuccess {
			t.Fatalf("Read %d: err=%v status=%v", i, err, resp.GetStatus())
		}
		// The suppressed READ only holds its sample when that sample is strictly
		// after the one already written, and two READs this close together land
		// in the same tick on a coarse clock — so the handle would come out of
		// the loop with nothing held, for a reason that has nothing to do with
		// coalescing. Backdate inside the window: still suppressed, now ordered.
		if i == 0 {
			openFile.mu.Lock()
			openFile.SmbAtimeWrittenAt = openFile.SmbAtimeWrittenAt.Add(-time.Second)
			openFile.mu.Unlock()
		}
	}
	if openFile.SmbPendingAtime.IsZero() {
		t.Fatal("no access time held on the handle; CLOSE would have nothing to flush")
	}

	frozen := freezeCtimeOnSeededFile(t, h, smbCtx, openFile)

	cresp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID})
	if err != nil || cresp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Close: err=%v status=%v", err, cresp.GetStatus())
	}

	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(context.Background(), openFile.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after CLOSE: %v", err)
	}
	if !file.Ctime.Equal(frozen) {
		t.Errorf("ChangeTime = %v after CLOSE; want the frozen %v", file.Ctime.UTC(), frozen)
	}
	if !file.Atime.After(frozen) {
		t.Errorf("LastAccessTime = %v after CLOSE; the held bump should have flushed", file.Atime.UTC())
	}
}

// setupCopyChunkFrozenFixture wires a share with a 4096-byte source file and an
// empty destination, plus an open handle on each, ready for executeCopyChunks.
func setupCopyChunkFrozenFixture(t *testing.T) (*Handler, *SMBHandlerContext, *OpenFile, *OpenFile) {
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
	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "ccfmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("ccfmeta", metamemory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	bsID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "ccfbs", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	const shareName = "/ccf"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name: shareName, MetadataStore: "ccfmeta", Enabled: true, BlockStoreID: bsID,
		RootAttr: &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()

	newFile := func(name string) (*metadata.File, metadata.FileHandle) {
		f, _, err := metaSvc.CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644,
		})
		if err != nil {
			t.Fatalf("CreateFile %s: %v", name, err)
		}
		fh, err := metadata.EncodeFileHandle(f)
		if err != nil {
			t.Fatalf("EncodeFileHandle %s: %v", name, err)
		}
		return f, fh
	}
	srcFile, srcHandle := newFile("src")
	dstFile, dstHandle := newFile("dst")

	// Give the source 4096 bytes so the copy has something to move.
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	srcBS, err := rt.GetBlockStoreForHandle(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	writeOp, err := metaSvc.PrepareWrite(authCtx, srcHandle, 4096)
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := srcBS.WriteAt(ctx, string(writeOp.PayloadID), nil, payload, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, srcHandle, true); err != nil {
		t.Fatalf("Flush src: %v", err)
	}

	h := NewHandler()
	h.Registry = rt
	sessUID, sessGID := uint32(0), uint32(0)
	sess := h.CreateSession("127.0.0.1:54321", false, "tester", "")
	sess.User = &models.User{Username: "tester", UID: &sessUID, Groups: []models.Group{{GID: &sessGID}}}

	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID: treeID, SessionID: sess.SessionID, ShareName: shareName,
		Permission: models.PermissionReadWrite,
	})

	access := uint32(types.FileReadData | types.FileWriteData |
		types.FileReadAttributes | types.FileWriteAttributes)
	mk := func(id byte, name string, mh metadata.FileHandle, pid metadata.PayloadID) *OpenFile {
		o := (&OpenFile{
			FileID: [16]byte{id}, TreeID: treeID, SessionID: sess.SessionID, ShareName: shareName,
			DesiredAccess: access, GrantedAccess: access, MetadataHandle: mh, PayloadID: pid,
		}).WithName(OpenName{Path: name, FileName: name, ParentHandle: rootHandle})
		h.StoreOpenFile(o)
		return o
	}
	srcOpen := mk(1, "src", srcHandle, srcFile.PayloadID)
	dstOpen := mk(2, "dst", dstHandle, dstFile.PayloadID)

	return h, &SMBHandlerContext{
		Context: ctx, SessionID: sess.SessionID, TreeID: treeID, ShareName: shareName,
	}, srcOpen, dstOpen
}
