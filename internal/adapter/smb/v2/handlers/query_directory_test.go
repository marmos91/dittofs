// Refs #573. Handler-level integration coverage for SMB2 QUERY_DIRECTORY
// with the share's AccessBasedEnumeration toggle. Mirrors MS-SMB2 §3.3.5.18.1
// ¶6 ("If TreeConnect.Share.AccessBasedEnumeration is TRUE, the server MUST
// NOT include entries for which the user does not have FILE_READ_DATA /
// FILE_LIST_DIRECTORY access") and the smb2.acls.ACCESSBASED smbtorture
// case at source4/torture/smb2/acls.c:2308.
package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// abeChild describes a single per-entry fixture: a regular file with the
// given name, UID/GID, mode bits and (optional) DACL. The SMB SET_INFO
// Security wire path lands at metadata.SetFileAttributes with SetAttrs.ACL,
// so the test setup uses the same metadata API.
type abeChild struct {
	name string
	uid  uint32
	gid  uint32
	mode uint32
	acl  *acl.ACL
}

// setupABEQueryDirTest wires a Handler against an in-memory metadata store
// where the share has AccessBasedEnumeration explicitly toggled and the
// caller identity is built from callerUID / callerGID. children are
// created beneath the share root with the per-entry attrs given.
//
// The returned SMBHandlerContext is pre-populated with TreeID + User so
// QueryDirectory's ABE branch (gated on h.GetTree(TreeID).AccessBasedEnumeration
// + BuildAuthContext(ctx.User)) finds both the share toggle and the
// caller's UID/GID.
func setupABEQueryDirTest(t *testing.T, abe bool, callerUID, callerGID uint32, children []abeChild) (*Handler, *OpenFile, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "/abe"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:                   shareName,
		MetadataStore:          "test-meta",
		AccessBasedEnumeration: abe,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	// Files are created as root so per-file ACL / mode-bit decisions made
	// downstream reflect the fixture exactly and aren't shadowed by the
	// creator's owner-implicit grants.
	rootUID := uint32(0)
	rootGID := uint32(0)
	rootAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &rootUID,
			GID: &rootGID,
		},
	}
	metaSvc := rt.GetMetadataService()
	for _, c := range children {
		if _, err := metaSvc.CreateFile(rootAuth, rootHandle, c.name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: c.mode,
			UID:  c.uid,
			GID:  c.gid,
		}); err != nil {
			t.Fatalf("CreateFile(%q): %v", c.name, err)
		}
		if c.acl != nil {
			// SetFileAttributes with SetAttrs.ACL is the metadata-layer
			// entrypoint that SET_INFO Security calls after parsing the
			// wire SD; mirrors what a Windows client triggers via
			// smb2_setinfo_file. Re-resolve the child handle here
			// because CreateFile returns *File; we need FileHandle.
			childFile, lookupErr := metaSvc.Lookup(rootAuth, rootHandle, c.name)
			if lookupErr != nil {
				t.Fatalf("Lookup(%q): %v", c.name, lookupErr)
			}
			childHandle, encodeErr := metadata.EncodeFileHandle(childFile)
			if encodeErr != nil {
				t.Fatalf("EncodeFileHandle(%q): %v", c.name, encodeErr)
			}
			if err := metaSvc.SetFileAttributes(rootAuth, childHandle, &metadata.SetAttrs{ACL: c.acl}); err != nil {
				t.Fatalf("SetFileAttributes ACL(%q): %v", c.name, err)
			}
		}
	}

	h := NewHandler()
	h.Registry = rt

	// Register the tree so QueryDirectory's ABE gate
	// (treeHasAccessBasedEnumeration) finds the share toggle. TreeID = 1
	// is arbitrary as long as it matches smbCtx.TreeID below.
	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:                 treeID,
		ShareName:              shareName,
		AccessBasedEnumeration: abe,
	})

	open := &OpenFile{
		FileID:         h.GenerateFileID(),
		TreeID:         treeID,
		Path:           shareName,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(open)

	// SMB session carries a User struct (callerUID / callerGID);
	// QueryDirectory goes through BuildAuthContext → BuildAuthContextFromUser
	// which mirrors the production session-setup → request-dispatch handoff.
	uidPtr := callerUID
	gidPtr := callerGID
	smbCtx := &SMBHandlerContext{
		Context: context.Background(),
		TreeID:  treeID,
		User: &models.User{
			Username: fmt.Sprintf("uid-%d", callerUID),
			UID:      &uidPtr,
			Groups:   []models.Group{{GID: &gidPtr}},
		},
	}

	return h, open, smbCtx
}

// callABEQuery invokes QueryDirectory with sensible defaults for the ABE
// handler-level tests. Mirrors the smbtorture acls.ACCESSBASED enumeration
// which uses SMB2_FIND_DIRECTORY_INFO with continue_flags=REOPEN and a
// 4 KiB output buffer.
func callABEQuery(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, openID [16]byte) *QueryDirectoryResponse {
	t.Helper()
	req := &QueryDirectoryRequest{
		FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
		Flags:              uint8(types.SMB2RestartScans),
		FileID:             openID,
		FileName:           "*",
		OutputBufferLength: 65536,
	}
	resp, err := h.QueryDirectory(smbCtx, req)
	if err != nil {
		t.Fatalf("QueryDirectory error: %v", err)
	}
	return resp
}

// countABEEntries walks the NextEntryOffset chain in a directory info buffer
// and returns the entry count. Encoder-class-agnostic: only the first
// 4 bytes of each entry (NextEntryOffset) are read.
func countABEEntries(buf []byte) int {
	if len(buf) < 4 {
		return 0
	}
	n := 0
	off := 0
	for {
		n++
		next := binary.LittleEndian.Uint32(buf[off : off+4])
		if next == 0 {
			return n
		}
		off += int(next)
		if off+4 > len(buf) {
			return n
		}
	}
}

// ownerOnlyReadACL grants ACE4_READ_DATA only to the file owner. Under ABE,
// such an entry is hidden from any caller that isn't the owner.
func ownerOnlyReadACL() *acl.ACL {
	return &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: acl.ACE4_READ_DATA,
				Who:        acl.SpecialOwner,
			},
		},
	}
}

// everyoneReadACL grants ACE4_READ_DATA to EVERYONE@. Under ABE, the entry
// remains visible to any caller.
func everyoneReadACL() *acl.ACL {
	return &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: acl.ACE4_READ_DATA,
				Who:        acl.SpecialEveryone,
			},
		},
	}
}

// TestQueryDirectory_ABE_OmitsNonReadableEntries — refs #573. With ABE on
// and the only child entry's DACL denying the caller READ_DATA, the
// QUERY_DIRECTORY response must hide the entry. Mirrors smb2.acls.ACCESSBASED
// at source4/torture/smb2/acls.c:2308.
func TestQueryDirectory_ABE_OmitsNonReadableEntries(t *testing.T) {
	const callerUID = uint32(2000)
	const callerGID = uint32(2000)
	h, open, smbCtx := setupABEQueryDirTest(t, true, callerUID, callerGID, []abeChild{
		{
			name: "secret.txt",
			uid:  1000, // not the caller — DACL denies non-owners
			gid:  1000,
			mode: 0o600,
			acl:  ownerOnlyReadACL(),
		},
	})

	resp := callABEQuery(t, h, smbCtx, open.FileID)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%x, want StatusSuccess", uint32(resp.Status))
	}
	// Only "." and ".." should remain; secret.txt is filtered out.
	if got := countABEEntries(resp.Data); got != 2 {
		t.Fatalf("entries = %d, want 2 (. and .. only — secret.txt must be hidden)", got)
	}
}

// TestQueryDirectory_ABE_IncludesReadableEntries — refs #573. With ABE on
// and the child entry's DACL granting EVERYONE@ READ_DATA, the entry must
// remain visible in the listing.
func TestQueryDirectory_ABE_IncludesReadableEntries(t *testing.T) {
	const callerUID = uint32(2000)
	const callerGID = uint32(2000)
	h, open, smbCtx := setupABEQueryDirTest(t, true, callerUID, callerGID, []abeChild{
		{
			name: "public.txt",
			uid:  1000,
			gid:  1000,
			mode: 0o644,
			acl:  everyoneReadACL(),
		},
	})

	resp := callABEQuery(t, h, smbCtx, open.FileID)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%x, want StatusSuccess", uint32(resp.Status))
	}
	// ".", "..", public.txt — 3 entries.
	if got := countABEEntries(resp.Data); got != 3 {
		t.Fatalf("entries = %d, want 3 (. + .. + public.txt)", got)
	}
}

// TestQueryDirectory_ABEDisabled_IncludesAllEntries — refs #573. With ABE
// off, the handler must NOT apply the per-entry filter even when the
// child's DACL would deny the caller. Same fixture as
// TestQueryDirectory_ABE_OmitsNonReadableEntries but with the share toggle
// disabled.
func TestQueryDirectory_ABEDisabled_IncludesAllEntries(t *testing.T) {
	const callerUID = uint32(2000)
	const callerGID = uint32(2000)
	h, open, smbCtx := setupABEQueryDirTest(t, false, callerUID, callerGID, []abeChild{
		{
			name: "secret.txt",
			uid:  1000,
			gid:  1000,
			mode: 0o600,
			acl:  ownerOnlyReadACL(),
		},
	})

	resp := callABEQuery(t, h, smbCtx, open.FileID)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%x, want StatusSuccess", uint32(resp.Status))
	}
	// ".", "..", secret.txt — 3 entries (no ABE filter).
	if got := countABEEntries(resp.Data); got != 3 {
		t.Fatalf("entries = %d, want 3 (. + .. + secret.txt — ABE off)", got)
	}
}
