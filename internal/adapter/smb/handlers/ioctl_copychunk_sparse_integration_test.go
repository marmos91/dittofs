// Integration-level reproducer for smbtorture smb2.ioctl.copy_chunk_sparse_dest
// (source4/torture/smb2/ioctl.c::test_ioctl_copy_chunk_sparse_dest).
//
// The smbtorture sequence:
//  1. Source file FNAME filled with 4096 bytes of a non-zero pattern.
//  2. Destination file FNAME2 created 0-byte.
//  3. FSCTL_SRV_COPYCHUNK copies source [0,4096) -> dest [4096,8192).
//  4. READ dest [0,4096): must return STATUS_OK with 4096 ZERO bytes (the
//     sparse hole between old EOF (0) and the chunk target offset (4096)).
//  5. check_pattern on dest [4096,8192): must equal the source pattern.
//
// This drives the REAL executeCopyChunks write path and the REAL Read handler
// against a memory block store (the backend every smb-conformance profile uses
// — see test/smb-conformance/bootstrap.sh `store block local add --type memory`).
package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestCopyChunk_SparseDest_LeadingGapReadsZeros(t *testing.T) {
	ctx := context.Background()

	// Control-plane store (in-memory SQLite) so AddShare can resolve a
	// real memory block store config.
	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}

	rt := runtime.New(cps)

	// Register a memory metadata store.
	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "ccmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ccmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// Create a memory local block store config.
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "ccbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/cc"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "ccmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		},
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

	// --- Source file: create + write 4096 bytes of a non-zero pattern. ---
	srcFile, _, err := metaSvc.CreateFile(authCtx, rootHandle, "src", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile src: %v", err)
	}
	srcHandle, err := metadata.EncodeFileHandle(srcFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle src: %v", err)
	}

	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte((i % 251) + 1) // 1..251, never zero
	}

	srcBS, err := rt.GetBlockStoreForHandle(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle src: %v", err)
	}
	srcWriteOp, err := metaSvc.PrepareWrite(authCtx, srcHandle, 4096)
	if err != nil {
		t.Fatalf("PrepareWrite src: %v", err)
	}
	if _, err := srcBS.WriteAt(ctx, string(srcWriteOp.PayloadID), nil, pattern, 0); err != nil {
		t.Fatalf("src WriteAt: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, srcWriteOp); err != nil {
		t.Fatalf("CommitWrite src: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, srcHandle); err != nil {
		t.Fatalf("Flush src: %v", err)
	}

	// --- Destination file: create 0-byte. ---
	dstFile, _, err := metaSvc.CreateFile(authCtx, rootHandle, "dst", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile dst: %v", err)
	}
	dstHandle, err := metadata.EncodeFileHandle(dstFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle dst: %v", err)
	}

	// --- Handler wiring: session, tree, open files. ---
	h := NewHandler()
	h.Registry = rt

	// Authenticated session whose identity (UID/GID 0) matches the file
	// owner created above. A null/anonymous session would map to the
	// unprivileged nobody (65534) and be denied write on the 0o644 files
	// (audit #1132 — null sessions no longer get root).
	sessUID, sessGID := uint32(0), uint32(0)
	sess := h.CreateSession("127.0.0.1:54321", false, "tester", "")
	sess.User = &models.User{
		Username: "tester",
		UID:      &sessUID,
		Groups:   []models.Group{{GID: &sessGID}},
	}
	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{TreeID: treeID, SessionID: sess.SessionID, ShareName: shareName})

	srcOpen := &OpenFile{
		FileID:         [16]byte{1},
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		Path:           "src",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileReadData | types.FileWriteData),
		GrantedAccess:  uint32(types.FileReadData | types.FileWriteData),
		MetadataHandle: srcHandle,
		PayloadID:      srcFile.PayloadID,
	}
	dstOpen := &OpenFile{
		FileID:         [16]byte{2},
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		Path:           "dst",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileReadData | types.FileWriteData),
		GrantedAccess:  uint32(types.FileReadData | types.FileWriteData),
		MetadataHandle: dstHandle,
		PayloadID:      dstFile.PayloadID,
		ParentHandle:   rootHandle,
		FileName:       "dst",
	}
	h.StoreOpenFile(srcOpen)
	h.StoreOpenFile(dstOpen)

	smbCtx := &SMBHandlerContext{
		Context:   ctx,
		SessionID: sess.SessionID,
		TreeID:    treeID,
		ShareName: shareName,
	}

	// --- Drive the real copychunk executor: source [0,4096) -> dest [4096,8192). ---
	chunks := []copyChunk{{SourceOffset: 0, TargetOffset: 4096, Length: 4096}}
	res, err := h.executeCopyChunks(smbCtx, FsctlSrvCopyChunk, dstOpen.FileID, srcOpen, dstOpen, chunks)
	if err != nil {
		t.Fatalf("executeCopyChunks: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("executeCopyChunks status = 0x%08x, want SUCCESS", uint32(res.Status))
	}

	// --- Read dest [0,4096): must be STATUS_OK with 4096 zero bytes. ---
	readReq := &ReadRequest{FileID: dstOpen.FileID, Offset: 0, Length: 4096}
	readResp, err := h.Read(smbCtx, readReq)
	if err != nil {
		t.Fatalf("Read [0,4096): %v", err)
	}
	if readResp.Status != types.StatusSuccess {
		t.Fatalf("Read [0,4096) status = 0x%08x, want SUCCESS (sparse hole)", uint32(readResp.Status))
	}
	if len(readResp.Data) != 4096 {
		t.Fatalf("Read [0,4096) returned %d bytes, want 4096", len(readResp.Data))
	}
	for i, b := range readResp.Data {
		if b != 0 {
			t.Fatalf("sparse hole byte %d = 0x%02x, want 0", i, b)
		}
	}

	// --- Read dest [4096,8192): must equal the source pattern. ---
	readReq2 := &ReadRequest{FileID: dstOpen.FileID, Offset: 4096, Length: 4096}
	readResp2, err := h.Read(smbCtx, readReq2)
	if err != nil {
		t.Fatalf("Read [4096,8192): %v", err)
	}
	if readResp2.Status != types.StatusSuccess {
		t.Fatalf("Read [4096,8192) status = 0x%08x, want SUCCESS", uint32(readResp2.Status))
	}
	if !bytes.Equal(readResp2.Data, pattern) {
		t.Fatalf("copied region [4096,8192) does not match source pattern")
	}

	// --- Defensive: a single read spanning hole + data [0,8192) must return
	// zeros for [0,4096) and the pattern for [4096,8192). ---
	readReq3 := &ReadRequest{FileID: dstOpen.FileID, Offset: 0, Length: 8192}
	readResp3, err := h.Read(smbCtx, readReq3)
	if err != nil {
		t.Fatalf("Read [0,8192): %v", err)
	}
	if readResp3.Status != types.StatusSuccess || len(readResp3.Data) != 8192 {
		t.Fatalf("Read [0,8192) status=0x%08x len=%d, want SUCCESS/8192",
			uint32(readResp3.Status), len(readResp3.Data))
	}
	for i := 0; i < 4096; i++ {
		if readResp3.Data[i] != 0 {
			t.Fatalf("spanning read: hole byte %d = 0x%02x, want 0", i, readResp3.Data[i])
		}
	}
	if !bytes.Equal(readResp3.Data[4096:], pattern) {
		t.Fatalf("spanning read: copied region mismatch")
	}
}

// TestCopyChunk_SparseDest_SurvivesPriorPayloadReuse reproduces the
// full-battery pollution that broke smb2.ioctl.copy_chunk_sparse_dest at
// ioctl.c:1772 ("sparse did not pass class") even though the standalone run
// passed. An earlier copychunk test (copy_chunk_src_exceed_multi) writes a
// NON-ZERO pattern into the dest path's leading region, then the dest is
// unlinked and recreated at the SAME path.
//
// Since #1166 PR-3 a recreated file gets a FRESH UUID-based PayloadID, so the
// new dest reads its sparse hole as zeros by construction — it never aliases
// the prior file's append log. This test still drives the REAL delete-on-close
// path (Handler.Close with DeletePending), which purges the deleted file's own
// payload to reclaim its append-log/CAS state; it asserts both that the recreate
// gets a fresh PayloadID and that the sparse hole reads zeros.
func TestCopyChunk_SparseDest_SurvivesPriorPayloadReuse(t *testing.T) {
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := runtime.New(cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "ccmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("ccmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "ccbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/cc"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "ccmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
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
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()

	// --- Source file: 4096 bytes of a non-zero pattern. ---
	srcFile, _, err := metaSvc.CreateFile(authCtx, rootHandle, "src", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile src: %v", err)
	}
	srcHandle, err := metadata.EncodeFileHandle(srcFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle src: %v", err)
	}
	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte((i % 251) + 1)
	}
	srcBS, err := rt.GetBlockStoreForHandle(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle src: %v", err)
	}
	srcWriteOp, err := metaSvc.PrepareWrite(authCtx, srcHandle, 4096)
	if err != nil {
		t.Fatalf("PrepareWrite src: %v", err)
	}
	if _, err := srcBS.WriteAt(ctx, string(srcWriteOp.PayloadID), nil, pattern, 0); err != nil {
		t.Fatalf("src WriteAt: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, srcWriteOp); err != nil {
		t.Fatalf("CommitWrite src: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, srcHandle); err != nil {
		t.Fatalf("Flush src: %v", err)
	}

	h := NewHandler()
	h.Registry = rt
	// Authenticated session whose UID/GID 0 matches the file owner; a null
	// session would map to nobody (65534) and be denied write (audit #1132).
	sessUID, sessGID := uint32(0), uint32(0)
	sess := h.CreateSession("127.0.0.1:54321", false, "tester", "")
	sess.User = &models.User{
		Username: "tester",
		UID:      &sessUID,
		Groups:   []models.Group{{GID: &sessGID}},
	}
	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{TreeID: treeID, SessionID: sess.SessionID, ShareName: shareName})

	srcOpen := &OpenFile{
		FileID:         [16]byte{1},
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		Path:           "src",
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileReadData | types.FileWriteData),
		GrantedAccess:  uint32(types.FileReadData | types.FileWriteData),
		MetadataHandle: srcHandle,
		PayloadID:      srcFile.PayloadID,
	}
	h.StoreOpenFile(srcOpen)

	smbCtx := &SMBHandlerContext{
		Context:   ctx,
		SessionID: sess.SessionID,
		TreeID:    treeID,
		ShareName: shareName,
	}

	// openDst creates "dst" at the share root and returns a wired OpenFile
	// for it. Since #1166 PR-3 each create gets a fresh UUID-based PayloadID,
	// so successive calls return DIFFERENT PayloadIDs even at the same path —
	// the recreate the smbtorture battery exercises no longer aliases the
	// prior file's content.
	openDst := func(fileID [16]byte) (*OpenFile, metadata.FileHandle) {
		t.Helper()
		dstFile, _, err := metaSvc.CreateFile(authCtx, rootHandle, "dst", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644,
		})
		if err != nil {
			t.Fatalf("CreateFile dst: %v", err)
		}
		dstHandle, err := metadata.EncodeFileHandle(dstFile)
		if err != nil {
			t.Fatalf("EncodeFileHandle dst: %v", err)
		}
		of := &OpenFile{
			FileID:         fileID,
			TreeID:         treeID,
			SessionID:      sess.SessionID,
			Path:           "dst",
			ShareName:      shareName,
			DesiredAccess:  uint32(types.FileReadData | types.FileWriteData | types.Delete),
			GrantedAccess:  uint32(types.FileReadData | types.FileWriteData | types.Delete),
			MetadataHandle: dstHandle,
			PayloadID:      dstFile.PayloadID,
			ParentHandle:   rootHandle,
			FileName:       "dst",
		}
		h.StoreOpenFile(of)
		return of, dstHandle
	}

	// --- Round 1: mimic copy_chunk_src_exceed_multi — copy the non-zero
	// pattern into dest [0,4096), then delete-on-close the dest. ---
	dst1, _ := openDst([16]byte{2})
	chunks1 := []copyChunk{{SourceOffset: 0, TargetOffset: 0, Length: 4096}}
	res1, err := h.executeCopyChunks(smbCtx, FsctlSrvCopyChunk, dst1.FileID, srcOpen, dst1, chunks1)
	if err != nil || res1.Status != types.StatusSuccess {
		t.Fatalf("round1 executeCopyChunks: err=%v status=0x%08x", err, uint32(res1.Status))
	}
	// Sanity: dest [0,4096) now holds the non-zero pattern.
	pre := &ReadRequest{FileID: dst1.FileID, Offset: 0, Length: 4096}
	preResp, err := h.Read(smbCtx, pre)
	if err != nil || preResp.Status != types.StatusSuccess {
		t.Fatalf("round1 pre-read: err=%v status=0x%08x", err, uint32(preResp.Status))
	}
	if !bytes.Equal(preResp.Data, pattern) {
		t.Fatalf("round1: dest [0,4096) should hold the non-zero pattern before delete")
	}

	// Delete-on-close the dest (real CLOSE path). This MUST purge the
	// block-store payload so the reused PayloadID starts clean.
	dst1.DeletePending = true
	if _, err := h.Close(smbCtx, &CloseRequest{FileID: dst1.FileID}); err != nil {
		t.Fatalf("round1 Close (delete-on-close): %v", err)
	}

	// --- Round 2: recreate dest at the same path, then the sparse_dest copy
	// into [4096,8192). The leading [0,4096) hole MUST read zeros — not the
	// stale round-1 pattern. ---
	dst2, _ := openDst([16]byte{3})
	if dst2.PayloadID == dst1.PayloadID {
		t.Fatalf("test invariant: recreated dest should get a fresh UUID-based PayloadID, not reuse %q",
			dst1.PayloadID)
	}
	chunks2 := []copyChunk{{SourceOffset: 0, TargetOffset: 4096, Length: 4096}}
	res2, err := h.executeCopyChunks(smbCtx, FsctlSrvCopyChunk, dst2.FileID, srcOpen, dst2, chunks2)
	if err != nil || res2.Status != types.StatusSuccess {
		t.Fatalf("round2 executeCopyChunks: err=%v status=0x%08x", err, uint32(res2.Status))
	}

	// The sparse hole [0,4096) must read 4096 zero bytes.
	holeReq := &ReadRequest{FileID: dst2.FileID, Offset: 0, Length: 4096}
	holeResp, err := h.Read(smbCtx, holeReq)
	if err != nil {
		t.Fatalf("round2 hole read: %v", err)
	}
	if holeResp.Status != types.StatusSuccess || len(holeResp.Data) != 4096 {
		t.Fatalf("round2 hole read status=0x%08x len=%d, want SUCCESS/4096",
			uint32(holeResp.Status), len(holeResp.Data))
	}
	for i, b := range holeResp.Data {
		if b != 0 {
			t.Fatalf("round2 sparse hole byte %d = 0x%02x, want 0 (stale append-log not purged on delete)", i, b)
		}
	}

	// And the copied region [4096,8192) must equal the source pattern.
	dataReq := &ReadRequest{FileID: dst2.FileID, Offset: 4096, Length: 4096}
	dataResp, err := h.Read(smbCtx, dataReq)
	if err != nil || dataResp.Status != types.StatusSuccess {
		t.Fatalf("round2 data read: err=%v status=0x%08x", err, uint32(dataResp.Status))
	}
	if !bytes.Equal(dataResp.Data, pattern) {
		t.Fatalf("round2 copied region [4096,8192) does not match source pattern")
	}
}

// TestPurgeBlockStorePayload_PreservesHardLinkedContent guards the
// delete-on-close block-store purge against destroying content that a surviving
// hard link still references. MetadataService.RemoveFile returns an empty
// PayloadID precisely when content must survive (nlink>1 or recycle-to-trash);
// the purge must honour that signal rather than the open handle's PayloadID.
func TestPurgeBlockStorePayload_PreservesHardLinkedContent(t *testing.T) {
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := runtime.New(cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "hlmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("hlmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "hlbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/hl"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "hlmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
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
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()

	// Create "a", write a non-zero pattern, then hard-link it as "b".
	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "a", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile a: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle a: %v", err)
	}
	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte((i % 251) + 1)
	}
	bs, err := rt.GetBlockStoreForHandle(ctx, handle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	wop, err := metaSvc.PrepareWrite(authCtx, handle, 4096)
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := bs.WriteAt(ctx, string(wop.PayloadID), nil, pattern, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, wop); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := metaSvc.CreateHardLink(authCtx, rootHandle, "b", handle); err != nil {
		t.Fatalf("CreateHardLink b: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	// Delete the "a" link: RemoveFile decrements nlink (2 -> 1) and returns an
	// empty PayloadID. The purge must therefore NOT touch the block store.
	removed, _, err := metaSvc.RemoveFile(authCtx, rootHandle, "a")
	if err != nil {
		t.Fatalf("RemoveFile a: %v", err)
	}
	if removed.PayloadID != "" {
		t.Fatalf("RemoveFile of a hard-linked file should return empty PayloadID, got %q", removed.PayloadID)
	}
	var removedPayloadID metadata.PayloadID
	if removed != nil {
		removedPayloadID = removed.PayloadID
	}
	h.purgeBlockStorePayload(ctx, handle, removedPayloadID, "a", "TEST")

	// The surviving "b" link must still read the full pattern.
	dst := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, string(file.PayloadID), nil, dst, 0)
	if err != nil {
		t.Fatalf("ReadAt surviving link: %v", err)
	}
	if n != 4096 || !bytes.Equal(dst, pattern) {
		t.Fatalf("surviving hard link content corrupted by purge (n=%d)", n)
	}
}
