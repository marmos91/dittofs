// Tests the per-share `AclFlagInheritedCanonicalization` toggle wiring into
// setSecurityInfo (refs #514 T4). When the toggle is true (Windows default),
// SET_INFO Security with SE_DACL_AUTO_INHERITED but no SE_DACL_AUTO_INHERIT_REQ
// must canonicalize AutoInherited away. When the toggle is false (Samba opt-out
// via `acl flag inherited canonicalization = no`), the bit must be preserved
// verbatim so smbtorture smb2.acls_non_canonical.flags can round-trip it.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupACLToggleTest wires a runtime + memory metadata store with a single
// share whose AclFlagInheritedCanonicalization is set to `canonicalize`, and
// creates a single regular file under it. Returns a Handler, the file's
// OpenFile, and the auth context to drive setSecurityInfo.
func setupACLToggleTest(t *testing.T, shareName string, canonicalize bool) (*Handler, *OpenFile, *metadata.AuthContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("acl-toggle-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:                             shareName,
		MetadataStore:                    "acl-toggle-meta",
		Enabled:                          true,
		AclFlagInheritedCanonicalization: canonicalize,
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
		DesiredAccess:  uint32(types.FileWriteAttributes) | uint32(types.WriteDac),
		// Refs #559: setSecurityInfo gates on the open's GrantedAccess
		// (Samba fsp->access_mask); SECINFO_DACL requires WRITE_DAC.
		GrantedAccess: uint32(types.FileWriteAttributes) | uint32(types.WriteDac),
	}
	h.StoreOpenFile(openFile)

	return h, openFile, authCtx
}

// buildAutoInheritedOnlySD constructs a self-relative SD whose Control word
// carries DACL_PRESENT | DACL_AUTO_INHERITED (no AUTO_INHERIT_REQ). The DACL
// is a single allow-all ACE for an arbitrary user-style SID so it survives
// downstream ValidateACL. Reuses the raw-bytes helpers from
// security_setinfo_regression_test.go.
func buildAutoInheritedOnlySD(t *testing.T) []byte {
	t.Helper()
	owner := buildSID(t, "S-1-5-21-1000-1000-1000-500")
	const fileAllAccess uint32 = 0x001F01FF
	ace := buildACE(accessAllowedACEType, 0x00, fileAllAccess, owner)
	dacl := buildRawDACL(1, ace)
	// Control: DACL_PRESENT (0x0004) | DACL_AUTO_INHERITED (0x0400). No AUTO_INHERIT_REQ.
	control := uint16(seDACLPresent | seDACLAutoInherited)
	return buildSelfRelativeSD(t, control, owner, nil, dacl)
}

// TestSetSecurityInfo_AclCanonicalizationToggle — T4 of #514.
//
// Verifies setSecurityInfo honors the per-share toggle by routing it through
// ParseSecurityDescriptorWithOptions. Default (canonicalize=true) strips
// AutoInherited when AUTO_INHERIT_REQ is absent; canonicalize=false preserves
// it verbatim (Samba `acl flag inherited canonicalization = no`).
func TestSetSecurityInfo_AclCanonicalizationToggle(t *testing.T) {
	for _, tc := range []struct {
		name              string
		share             string
		canonicalize      bool
		wantAutoInherited bool
	}{
		{
			name:              "canonicalize_true_strips_AutoInherited",
			share:             "/acl-canon-on",
			canonicalize:      true,
			wantAutoInherited: false,
		},
		{
			name:              "canonicalize_false_preserves_AutoInherited",
			share:             "/acl-canon-off",
			canonicalize:      false,
			wantAutoInherited: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, openFile, authCtx := setupACLToggleTest(t, tc.share, tc.canonicalize)

			sdBuf := buildAutoInheritedOnlySD(t)
			resp, err := h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, sdBuf)
			if err != nil {
				t.Fatalf("setSecurityInfo: %v", err)
			}
			if resp == nil || resp.Status != types.StatusSuccess {
				t.Fatalf("setSecurityInfo: unexpected status, resp=%+v", resp)
			}

			// Read back the stored ACL via the metadata service.
			metaSvc := h.Registry.GetMetadataService()
			file, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
			if err != nil {
				t.Fatalf("GetFile: %v", err)
			}
			if file.ACL == nil {
				t.Fatalf("stored ACL is nil after SET_INFO Security")
			}
			if file.ACL.AutoInherited != tc.wantAutoInherited {
				t.Errorf("ACL.AutoInherited = %v, want %v (canonicalize=%v)",
					file.ACL.AutoInherited, tc.wantAutoInherited, tc.canonicalize)
			}
		})
	}
}

// TestParseSDOptsForShare_FallbackOnMissingShare — defensive guard for the
// "lookup fails" path in setSecurityInfo: a stale openFile that references an
// unknown share name must still get safe Windows-canonical defaults rather
// than fail the SET_INFO request. Pure unit-level check of the helper.
func TestParseSDOptsForShare_FallbackOnMissingShare(t *testing.T) {
	rt := runtime.New(nil)
	h := NewHandler()
	h.Registry = rt

	opts := h.parseSDOptsForShare("/does-not-exist")
	if !opts.CanonicalizeAutoInherited {
		t.Errorf("missing share: CanonicalizeAutoInherited = false, want true (safe Windows default)")
	}
}

// TestParseSDOptsForShare_NilRegistry — second defensive guard: a Handler
// constructed without a Runtime (test plumbing) must still return safe
// defaults instead of panicking.
func TestParseSDOptsForShare_NilRegistry(t *testing.T) {
	h := NewHandler()
	h.Registry = nil

	opts := h.parseSDOptsForShare("/any")
	if !opts.CanonicalizeAutoInherited {
		t.Errorf("nil registry: CanonicalizeAutoInherited = false, want true (safe Windows default)")
	}
}
