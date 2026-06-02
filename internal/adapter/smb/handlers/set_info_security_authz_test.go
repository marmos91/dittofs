// Tests for SET_INFO Security access authorization against the open's
// GrantedAccess (Samba `fsp->access_mask`). Per MS-SMB2 §3.3.5.21.3 and
// MS-FSA §2.1.5.14 the SD-section→access-bit gate is:
//
//	SECINFO_DACL  → WRITE_DAC
//	SECINFO_OWNER → WRITE_OWNER
//	SECINFO_GROUP → WRITE_OWNER
//	SECINFO_SACL  → ACCESS_SYSTEM_SECURITY
//
// The handle's GrantedAccess captured at CREATE is the sole authority —
// the new SD being installed is irrelevant. Refs #559.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupSecurityAuthzTest provisions a runtime + share + file and returns a
// handler, the open file (caller stamps GrantedAccess), and an auth context.
func setupSecurityAuthzTest(t *testing.T) (*Handler, *OpenFile, *metadata.AuthContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("authz-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareName := "/authz"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "authz-meta",
		Enabled:       true,
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
	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "g.dat", &metadata.FileAttr{
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
		FileID:         [16]byte{9, 9, 9, 9},
		MetadataHandle: fileHandle,
		ParentHandle:   rootHandle,
		FileName:       "g.dat",
		Path:           "g.dat",
		ShareName:      shareName,
		// GrantedAccess intentionally zero; caller stamps per test.
	}
	h.StoreOpenFile(openFile)
	return h, openFile, authCtx
}

// minimalDACLSDForOwner builds a self-relative SD with a DACL_PRESENT control
// and a single allow-WRITE_DATA ACE for an arbitrary user-style SID. Mirrors
// the SD smb2.acls.OWNER (acls.c::test_owner_bits) installs at line 763.
func minimalDACLSDForOwner(t *testing.T) []byte {
	t.Helper()
	owner := buildSID(t, "S-1-5-21-1000-1000-1000-500")
	const writeData uint32 = 0x00000002
	ace := buildACE(accessAllowedACEType, 0x00, writeData, owner)
	dacl := buildRawDACL(1, ace)
	control := uint16(seDACLPresent)
	return buildSelfRelativeSD(t, control, owner, nil, dacl)
}

// TestSetInfo_SECINFO_DACL_AuthzAgainstHandleGrantedAccess — primary case
// for #559. SET_INFO SECINFO_DACL must succeed when the handle has WRITE_DAC
// and must deny when it does not, regardless of what the file's current DACL
// says or what the new SD looks like.
func TestSetInfo_SECINFO_DACL_AuthzAgainstHandleGrantedAccess(t *testing.T) {
	sd := minimalDACLSDForOwner(t)

	t.Run("grants_when_handle_has_WRITE_DAC", func(t *testing.T) {
		h, openFile, authCtx := setupSecurityAuthzTest(t)
		openFile.GrantedAccess = uint32(types.WriteDac)
		h.StoreOpenFile(openFile)

		resp, err := h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, sd)
		if err != nil {
			t.Fatalf("setSecurityInfo: %v", err)
		}
		if resp.Status != types.StatusSuccess {
			t.Fatalf("status = 0x%08x, want StatusSuccess (handle has WRITE_DAC)", resp.Status)
		}
	})

	t.Run("denies_when_handle_lacks_WRITE_DAC", func(t *testing.T) {
		h, openFile, authCtx := setupSecurityAuthzTest(t)
		// READ_CONTROL alone (no WRITE_DAC) — owner could have READ_CONTROL
		// implicitly but the handle was not opened with WRITE_DAC, so SET_INFO
		// SECINFO_DACL must be denied.
		openFile.GrantedAccess = uint32(types.ReadControl)
		h.StoreOpenFile(openFile)

		resp, err := h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, sd)
		if err != nil {
			t.Fatalf("setSecurityInfo: %v", err)
		}
		if resp.Status != types.StatusAccessDenied {
			t.Fatalf("status = 0x%08x, want StatusAccessDenied (handle missing WRITE_DAC)", resp.Status)
		}
	})
}

// TestSetInfo_SECINFO_OWNER_AuthzAgainstHandleGrantedAccess verifies the
// OWNER + GROUP sections both gate on WRITE_OWNER (MS-DTYP §2.5.3.3).
func TestSetInfo_SECINFO_OWNER_AuthzAgainstHandleGrantedAccess(t *testing.T) {
	sd := minimalDACLSDForOwner(t) // SD also carries an owner SID

	for _, tc := range []struct {
		name           string
		additionalInfo uint32
	}{
		{"owner_section", OwnerSecurityInformation},
		{"group_section", GroupSecurityInformation},
		{"owner_and_group", OwnerSecurityInformation | GroupSecurityInformation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("grants_when_handle_has_WRITE_OWNER", func(t *testing.T) {
				h, openFile, authCtx := setupSecurityAuthzTest(t)
				openFile.GrantedAccess = uint32(types.WriteOwner)
				h.StoreOpenFile(openFile)

				resp, err := h.setSecurityInfo(authCtx, openFile, tc.additionalInfo, sd)
				if err != nil {
					t.Fatalf("setSecurityInfo: %v", err)
				}
				// SUCCESS expected — the gate passes. Downstream SetFileAttributes
				// may still fail on the metadata layer for a non-root requester
				// trying to chown to a different UID, but that's a separate path
				// and not what this test asserts. We only verify the access gate.
				if resp.Status != types.StatusSuccess {
					t.Fatalf("status = 0x%08x, want StatusSuccess (handle has WRITE_OWNER)", resp.Status)
				}
			})

			t.Run("denies_when_handle_lacks_WRITE_OWNER", func(t *testing.T) {
				h, openFile, authCtx := setupSecurityAuthzTest(t)
				// WRITE_DAC alone is not WRITE_OWNER — must be denied.
				openFile.GrantedAccess = uint32(types.WriteDac)
				h.StoreOpenFile(openFile)

				resp, err := h.setSecurityInfo(authCtx, openFile, tc.additionalInfo, sd)
				if err != nil {
					t.Fatalf("setSecurityInfo: %v", err)
				}
				if resp.Status != types.StatusAccessDenied {
					t.Fatalf("status = 0x%08x, want StatusAccessDenied", resp.Status)
				}
			})
		})
	}
}

// TestSetInfo_SECINFO_SACL_AuthzAgainstHandleGrantedAccess verifies the SACL
// section gates on ACCESS_SYSTEM_SECURITY per MS-DTYP §2.5.3.3 / MS-SMB2
// §3.3.5.21.3.
func TestSetInfo_SECINFO_SACL_AuthzAgainstHandleGrantedAccess(t *testing.T) {
	sd := minimalDACLSDForOwner(t)

	t.Run("grants_when_handle_has_ACCESS_SYSTEM_SECURITY", func(t *testing.T) {
		h, openFile, authCtx := setupSecurityAuthzTest(t)
		openFile.GrantedAccess = uint32(types.AccessSystemSecurity)
		h.StoreOpenFile(openFile)

		resp, err := h.setSecurityInfo(authCtx, openFile, SACLSecurityInformation, sd)
		if err != nil {
			t.Fatalf("setSecurityInfo: %v", err)
		}
		// SACL changes are no-ops in DittoFS (no SACL persistence yet), so the
		// handler returns SUCCESS once the gate passes.
		if resp.Status != types.StatusSuccess {
			t.Fatalf("status = 0x%08x, want StatusSuccess (handle has ACCESS_SYSTEM_SECURITY)", resp.Status)
		}
	})

	t.Run("denies_when_handle_lacks_ACCESS_SYSTEM_SECURITY", func(t *testing.T) {
		h, openFile, authCtx := setupSecurityAuthzTest(t)
		openFile.GrantedAccess = uint32(types.WriteDac | types.WriteOwner)
		h.StoreOpenFile(openFile)

		resp, err := h.setSecurityInfo(authCtx, openFile, SACLSecurityInformation, sd)
		if err != nil {
			t.Fatalf("setSecurityInfo: %v", err)
		}
		if resp.Status != types.StatusAccessDenied {
			t.Fatalf("status = 0x%08x, want StatusAccessDenied", resp.Status)
		}
	})
}

// TestCheckSetInfoSecurityAccess_Helper covers the helper directly across
// the section→bit matrix and edge cases (zero info, MAXIMUM_ALLOWED).
func TestCheckSetInfoSecurityAccess_Helper(t *testing.T) {
	cases := []struct {
		name           string
		grantedAccess  uint32
		additionalInfo uint32
		wantStatus     types.Status
		wantOK         bool
	}{
		{"no_sections_no_gate", 0, 0, types.StatusSuccess, true},
		{"DACL_with_WRITE_DAC", uint32(types.WriteDac), DACLSecurityInformation, types.StatusSuccess, true},
		{"DACL_without_WRITE_DAC", uint32(types.ReadControl), DACLSecurityInformation, types.StatusAccessDenied, false},
		{"OWNER_with_WRITE_OWNER", uint32(types.WriteOwner), OwnerSecurityInformation, types.StatusSuccess, true},
		{"OWNER_without_WRITE_OWNER", uint32(types.WriteDac), OwnerSecurityInformation, types.StatusAccessDenied, false},
		{"GROUP_with_WRITE_OWNER", uint32(types.WriteOwner), GroupSecurityInformation, types.StatusSuccess, true},
		{"SACL_with_ACCESS_SYSTEM_SECURITY", uint32(types.AccessSystemSecurity), SACLSecurityInformation, types.StatusSuccess, true},
		{"SACL_without_ACCESS_SYSTEM_SECURITY", uint32(types.WriteDac), SACLSecurityInformation, types.StatusAccessDenied, false},
		{"DACL_with_MAXIMUM_ALLOWED", uint32(types.MaximumAllowed), DACLSecurityInformation, types.StatusSuccess, true},
		{"DACL_with_GENERIC_ALL", uint32(types.GenericAll), DACLSecurityInformation, types.StatusSuccess, true},
		{
			name:           "all_sections_with_all_bits",
			grantedAccess:  uint32(types.WriteDac | types.WriteOwner | types.AccessSystemSecurity),
			additionalInfo: OwnerSecurityInformation | GroupSecurityInformation | DACLSecurityInformation | SACLSecurityInformation,
			wantStatus:     types.StatusSuccess,
			wantOK:         true,
		},
		{
			name:           "DACL_and_OWNER_missing_OWNER_bit",
			grantedAccess:  uint32(types.WriteDac),
			additionalInfo: DACLSecurityInformation | OwnerSecurityInformation,
			wantStatus:     types.StatusAccessDenied,
			wantOK:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotOK := checkSetInfoSecurityAccess(tc.grantedAccess, tc.additionalInfo)
			if gotOK != tc.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotStatus != tc.wantStatus {
				t.Errorf("status = 0x%08x, want 0x%08x", gotStatus, tc.wantStatus)
			}
		})
	}
}
