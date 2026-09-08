package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// =============================================================================
// Cross-session handle ownership
// =============================================================================
//
// Opens live in one process-wide table keyed by the FileId alone, so
// GetOpenFile hands any live handle to any caller that names it. Every
// handle-based command then adopts that handle's session, tree, share and
// permission through primeAuthContextFromOpenFile, so an unowned handle would
// run the request as its owner, against its share, with its tree permission.
// These tests pin the refusal, one command per site class.

// ownedHandleFixture builds a share holding a handle owned by the session
// setupReparseShare creates, then adds a second session with a tree of its
// own — the shape prepareDispatch guarantees, since it already refuses a tree
// belonging to another session. It returns a ctx for each session.
//
// The owner's handle gains FILE_WRITE_ATTRIBUTES so SET_INFO clears its own
// GrantedAccess gate, which sits ahead of the priming call.
func ownedHandleFixture(t *testing.T) (h *Handler, owner, other *SMBHandlerContext, fileID [16]byte) {
	t.Helper()

	h, owner, _, fileID = setupReparseShare(t)

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("fixture handle missing")
	}
	granted := openFile.GrantedAccess | uint32(types.FileWriteAttributes)
	openFile.DesiredAccess = granted
	openFile.GrantedAccess = granted

	uid := uint32(1002)
	mallory := h.CreateSession("127.0.0.1:2", false, "mallory", "")
	mallory.User = &models.User{ID: "mallory", Username: "mallory", UID: &uid}

	const malloryTree uint32 = 2
	h.StoreTree(&TreeConnection{
		TreeID:     malloryTree,
		SessionID:  mallory.SessionID,
		ShareName:  owner.ShareName,
		Permission: models.PermissionReadWrite,
	})

	other = &SMBHandlerContext{
		Context:   context.Background(),
		SessionID: mallory.SessionID,
		TreeID:    malloryTree,
		ShareName: owner.ShareName,
	}
	return h, owner, other, fileID
}

// storeSecondHandle creates another file on the share and registers an open for
// it on treeID, owned by the session in ownerCtx.
func storeSecondHandle(t *testing.T, h *Handler, ownerCtx *SMBHandlerContext, name string, treeID uint32) [16]byte {
	t.Helper()

	rt := h.Registry
	rootHandle, err := rt.GetRootHandle(ownerCtx.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	file, _, err := rt.GetMetadataService().CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile(%s): %v", name, err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	var fileID [16]byte
	copy(fileID[:], name)
	granted := uint32(types.FileReadData | types.FileWriteData | types.FileWriteAttributes)
	h.StoreOpenFile((&OpenFile{
		FileID:         fileID,
		TreeID:         treeID,
		SessionID:      ownerCtx.SessionID,
		ShareName:      ownerCtx.ShareName,
		DesiredAccess:  granted,
		GrantedAccess:  granted,
		MetadataHandle: fileHandle,
	}).WithName(OpenName{Path: name, FileName: name, ParentHandle: rootHandle}))
	return fileID
}

// buildIoctlRequestBody encodes an SMB2 IOCTL request body [MS-SMB2] 2.2.31.
// The offsets are reported relative to the SMB2 header so the production
// parser, which sees the body from offset 56, resolves to the bytes written
// here. maxOutput matters to COPYCHUNK, which rejects a response buffer too
// small for SRV_COPYCHUNK_RESPONSE before it resolves the source handle.
func buildIoctlRequestBody(ctlCode uint32, fileID [16]byte, input []byte, maxOutput uint32) []byte {
	const fixedSize = 56
	w := smbenc.NewWriter(fixedSize + len(input))
	w.WriteUint16(57)                     // StructureSize
	w.WriteUint16(0)                      // Reserved
	w.WriteUint32(ctlCode)                // CtlCode
	w.WriteBytes(fileID[:])               // FileId
	w.WriteUint32(uint32(64 + fixedSize)) // InputOffset
	w.WriteUint32(uint32(len(input)))     // InputCount
	w.WriteUint32(0)                      // MaxInputResponse
	w.WriteUint32(uint32(64 + fixedSize)) // OutputOffset
	w.WriteUint32(0)                      // OutputCount
	w.WriteUint32(maxOutput)              // MaxOutputResponse
	w.WriteUint32(0)                      // Flags
	w.WriteUint32(0)                      // Reserved2
	if len(input) > 0 {
		w.WriteBytes(input)
	}
	return w.Bytes()
}

// copyChunkInput encodes SRV_COPYCHUNK_COPY [MS-SMB2] 2.2.32.1 for one chunk.
func copyChunkInput(resumeKey [resumeKeyLen]byte) []byte {
	w := smbenc.NewWriter(32 + 24)
	w.WriteBytes(resumeKey[:])
	w.WriteUint32(1) // ChunkCount
	w.WriteUint32(0) // Reserved
	w.WriteUint64(0) // SourceOffset
	w.WriteUint64(0) // TargetOffset
	w.WriteUint32(4) // Length
	w.WriteUint32(0) // Reserved
	return w.Bytes()
}

// TestHandleOwnership_ForeignSessionRefused drives one command per site class
// that adopts a handle's identity, and asserts a second session naming the
// first's FileId is refused with STATUS_FILE_CLOSED — the same status these
// handlers return for a FileId that does not exist, so a handle belonging to
// somebody else stays indistinguishable from a closed one.
//
// The owner arm is the positive control: it asserts the guard does not fire on
// the session that opened the handle. It checks only that the status is not the
// refusal, since what each handler returns past that point is its own business,
// covered by its own tests.
func TestHandleOwnership_ForeignSessionRefused(t *testing.T) {
	ioctlOp := func(ctlCode uint32, input []byte) func(*Handler, *SMBHandlerContext, [16]byte) types.Status {
		return func(h *Handler, ctx *SMBHandlerContext, fileID [16]byte) types.Status {
			res, err := h.Ioctl(ctx, buildIoctlRequestBody(ctlCode, fileID, input, 4096))
			if err != nil {
				return types.StatusInternalError
			}
			return res.Status
		}
	}

	byteRange := func() []byte {
		w := smbenc.NewWriter(16)
		w.WriteUint64(0)   // FileOffset
		w.WriteUint64(512) // Length / BeyondFinalZero
		return w.Bytes()
	}()

	ops := []struct {
		name string
		call func(*Handler, *SMBHandlerContext, [16]byte) types.Status
	}{
		{"CLOSE", func(h *Handler, ctx *SMBHandlerContext, fileID [16]byte) types.Status {
			resp, err := h.Close(ctx, &CloseRequest{FileID: fileID})
			if err != nil {
				return types.StatusInternalError
			}
			return resp.Status
		}},
		{"FLUSH", func(h *Handler, ctx *SMBHandlerContext, fileID [16]byte) types.Status {
			resp, err := h.Flush(ctx, &FlushRequest{FileID: fileID})
			if err != nil {
				return types.StatusInternalError
			}
			return resp.Status
		}},
		{"SET_INFO", func(h *Handler, ctx *SMBHandlerContext, fileID [16]byte) types.Status {
			resp, err := h.SetInfo(ctx, &SetInfoRequest{
				FileID:        fileID,
				InfoType:      uint8(types.SMB2InfoTypeFile),
				FileInfoClass: uint8(types.FileBasicInformation),
				Buffer:        make([]byte, 40),
			})
			if err != nil {
				return types.StatusInternalError
			}
			return resp.Status
		}},
		{"IOCTL FSCTL_SET_SPARSE", ioctlOp(FsctlSetSparse, []byte{1})},
		{"IOCTL FSCTL_QUERY_ALLOCATED_RANGES", ioctlOp(FsctlQueryAllocatedRanges, byteRange)},
		{"IOCTL FSCTL_SET_ZERO_DATA", ioctlOp(FsctlSetZeroData, byteRange)},
		{"IOCTL FSCTL_SET_COMPRESSION", ioctlOp(FsctlSetCompression, []byte{0, 0})},
		{"IOCTL FSCTL_GET_REPARSE_POINT", ioctlOp(FsctlGetReparsePoint, nil)},
	}

	// A fixture per arm, since several of these commands consume the handle.
	for _, op := range ops {
		t.Run(op.name+"/foreign", func(t *testing.T) {
			h, _, other, fileID := ownedHandleFixture(t)
			if got := op.call(h, other, fileID); got != types.StatusFileClosed {
				t.Errorf("%s from a second session = %v, want %v — the handle belongs to another session",
					op.name, got, types.StatusFileClosed)
			}
		})

		t.Run(op.name+"/owner", func(t *testing.T) {
			h, owner, _, fileID := ownedHandleFixture(t)
			if got := op.call(h, owner, fileID); got == types.StatusFileClosed {
				t.Errorf("%s from the owning session = %v, want anything but the ownership refusal",
					op.name, got)
			}
		})
	}
}

// TestHandleOwnership_CopyChunkForeignHandles covers the one command that holds
// two handles at once: two FileIds that belong to a single other session agree
// with each other, so only anchoring both to the requester refuses them.
func TestHandleOwnership_CopyChunkForeignHandles(t *testing.T) {
	h, owner, other, srcFileID := ownedHandleFixture(t)
	dstFileID := storeSecondHandle(t, h, owner, "dst", owner.TreeID)

	resumeKey, err := h.resumeKeys.issue(srcFileID)
	if err != nil {
		t.Fatalf("issue resume key: %v", err)
	}
	body := buildIoctlRequestBody(FsctlSrvCopyChunk, dstFileID, copyChunkInput(resumeKey), 4096)

	// STATUS_OBJECT_NAME_NOT_FOUND is what this path already returns for a
	// source the requester may not use (MS-SMB2 3.3.5.15.6).
	res, err := h.Ioctl(other, body)
	if err != nil {
		t.Fatalf("Ioctl: %v", err)
	}
	if res.Status != types.StatusObjectNameNotFound {
		t.Errorf("COPYCHUNK with two handles from another session = %v, want %v",
			res.Status, types.StatusObjectNameNotFound)
	}

	res, err = h.Ioctl(owner, body)
	if err != nil {
		t.Fatalf("Ioctl (owner): %v", err)
	}
	if res.Status == types.StatusObjectNameNotFound {
		t.Errorf("COPYCHUNK from the owning session = %v, want anything but the ownership refusal",
			res.Status)
	}
}

// TestHandleOwnership_CopyChunkCrossTreeSourceAllowed pins the deliberate
// asymmetry in the COPYCHUNK gate: the source arrives as a resume key rather
// than a TreeID, and MS-SMB2 3.3.5.15.6 scopes it to the requester's session,
// not to one tree connect. A server-side copy reading from another tree the
// same session holds is legitimate, so the source is checked on SessionID
// alone — the full tree+session predicate would refuse it.
func TestHandleOwnership_CopyChunkCrossTreeSourceAllowed(t *testing.T) {
	h, owner, _, _ := ownedHandleFixture(t)

	// A second tree for the same session, with the source handle opened on it.
	const otherTree uint32 = 3
	h.StoreTree(&TreeConnection{
		TreeID:     otherTree,
		SessionID:  owner.SessionID,
		ShareName:  owner.ShareName,
		Permission: models.PermissionReadWrite,
	})
	srcFileID := storeSecondHandle(t, h, owner, "src", otherTree)
	dstFileID := storeSecondHandle(t, h, owner, "dst", owner.TreeID)

	resumeKey, err := h.resumeKeys.issue(srcFileID)
	if err != nil {
		t.Fatalf("issue resume key: %v", err)
	}

	res, err := h.Ioctl(owner, buildIoctlRequestBody(FsctlSrvCopyChunk, dstFileID, copyChunkInput(resumeKey), 4096))
	if err != nil {
		t.Fatalf("Ioctl: %v", err)
	}
	if res.Status == types.StatusObjectNameNotFound {
		t.Error("COPYCHUNK with a source on another tree of the same session was refused; " +
			"3.3.5.15.6 scopes the source to the session, not to the tree connect")
	}
}
