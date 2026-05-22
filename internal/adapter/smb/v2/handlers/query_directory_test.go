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

// readOnlyACL grants ACE4_READ_DATA to a single principal. Under ABE,
// only callers matching that principal see the entry in the listing.
func readOnlyACL(who string) *acl.ACL {
	return &acl.ACL{
		ACEs: []acl.ACE{{
			Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
			AccessMask: acl.ACE4_READ_DATA,
			Who:        who,
		}},
	}
}

// setupABEQueryDirTest wires a Handler against an in-memory metadata store
// where the share has AccessBasedEnumeration explicitly toggled and the
// caller identity is built from callerUID / callerGID. children are
// created beneath the share root with the per-entry attrs given.
//
// The returned SMBHandlerContext is pre-populated with TreeID + User so
// QueryDirectory's ABE branch (gated on h.GetTree(TreeID).AccessBasedEnumeration
// + BuildAuthContext(ctx), which itself reads ctx.User) finds both the
// share toggle and the caller's UID/GID.
func setupABEQueryDirTest(t *testing.T, abe bool, callerUID, callerGID uint32, children []abeChild) (*Handler, *OpenFile, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/abe"
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
	rootUID, rootGID := uint32(0), uint32(0)
	rootAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &rootUID,
			GID: &rootGID,
		},
	}
	metaSvc := rt.GetMetadataService()
	for _, c := range children {
		childFile, err := metaSvc.CreateFile(rootAuth, rootHandle, c.name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: c.mode,
			UID:  c.uid,
			GID:  c.gid,
		})
		if err != nil {
			t.Fatalf("CreateFile(%q): %v", c.name, err)
		}
		if c.acl == nil {
			continue
		}
		// SetFileAttributes with SetAttrs.ACL is the metadata-layer
		// entrypoint that SET_INFO Security calls after parsing the
		// wire SD; mirrors what a Windows client triggers via
		// smb2_setinfo_file.
		childHandle, err := metadata.EncodeFileHandle(childFile)
		if err != nil {
			t.Fatalf("EncodeFileHandle(%q): %v", c.name, err)
		}
		if err := metaSvc.SetFileAttributes(rootAuth, childHandle, &metadata.SetAttrs{ACL: c.acl}); err != nil {
			t.Fatalf("SetFileAttributes ACL(%q): %v", c.name, err)
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
	smbCtx := &SMBHandlerContext{
		Context: context.Background(),
		TreeID:  treeID,
		User: &models.User{
			Username: fmt.Sprintf("uid-%d", callerUID),
			UID:      &callerUID,
			Groups:   []models.Group{{GID: &callerGID}},
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
	resp, err := h.QueryDirectory(smbCtx, &QueryDirectoryRequest{
		FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
		Flags:              uint8(types.SMB2RestartScans),
		FileID:             openID,
		FileName:           "*",
		OutputBufferLength: 65536,
	})
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
	n, off := 0, 0
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

// TestQueryDirectory_ABE covers the share-level AccessBasedEnumeration toggle
// at the QUERY_DIRECTORY handler boundary. Each case seeds a single child
// under the share root and asserts the resulting entry count after the
// handler's per-entry ABE filter has run. Refs #573; mirrors
// smb2.acls.ACCESSBASED at source4/torture/smb2/acls.c:2308.
func TestQueryDirectory_ABE(t *testing.T) {
	const (
		callerUID = uint32(2000)
		callerGID = uint32(2000)
		// Owner UID for fixture children — distinct from callerUID so
		// owner-only DACLs deny the caller.
		ownerUID = uint32(1000)
		ownerGID = uint32(1000)
	)

	cases := []struct {
		name        string
		abe         bool
		child       abeChild
		wantEntries int // ". " + ".." + (child if visible)
	}{
		{
			name: "ABE_on_omits_non_readable",
			abe:  true,
			child: abeChild{
				name: "secret.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o600,
				acl:  readOnlyACL(acl.SpecialOwner),
			},
			wantEntries: 2,
		},
		{
			name: "ABE_on_includes_readable",
			abe:  true,
			child: abeChild{
				name: "public.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o644,
				acl:  readOnlyACL(acl.SpecialEveryone),
			},
			wantEntries: 3,
		},
		{
			name: "ABE_off_includes_all",
			abe:  false,
			child: abeChild{
				name: "secret.txt",
				uid:  ownerUID,
				gid:  ownerGID,
				mode: 0o600,
				acl:  readOnlyACL(acl.SpecialOwner),
			},
			wantEntries: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, open, smbCtx := setupABEQueryDirTest(t, tc.abe, callerUID, callerGID, []abeChild{tc.child})

			resp := callABEQuery(t, h, smbCtx, open.FileID)
			if resp.Status != types.StatusSuccess {
				t.Fatalf("status = 0x%x, want StatusSuccess", uint32(resp.Status))
			}
			if got := countABEEntries(resp.Data); got != tc.wantEntries {
				t.Fatalf("entries = %d, want %d", got, tc.wantEntries)
			}
		})
	}
}
