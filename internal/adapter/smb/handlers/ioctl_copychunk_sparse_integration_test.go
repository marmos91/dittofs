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
	srcFile, err := metaSvc.CreateFile(authCtx, rootHandle, "src", &metadata.FileAttr{
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
	dstFile, err := metaSvc.CreateFile(authCtx, rootHandle, "dst", &metadata.FileAttr{
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

	sess := h.CreateSession("127.0.0.1:54321", false, "tester", "")
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
