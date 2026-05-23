// Integration-level reproducer for smbtorture smb2.acls.ACCESSBASED
// (source4/torture/smb2/acls.c::test_access_based, asserting at acls.c:2308).
//
// The smbtorture sequence:
//  1. Connect to share "hideunread" (advertises
//     SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM).
//  2. Create file `smb2-testsd` with SEC_RIGHTS_FILE_ALL (so the open's
//     GrantedAccess includes WRITE_DAC for the subsequent SET_INFO).
//  3. Issue SET_INFO Security with SECINFO_DACL only. The DACL has a single
//     ALLOW ACE for the file owner's SID with an access mask that omits one
//     of {READ_DATA, READ_EA, READ_ATTRIBUTES} but always keeps
//     READ_CONTROL | SYNCHRONIZE.
//  4. Re-open the parent directory and QUERY_DIRECTORY.
//  5. Assert the file is HIDDEN (only "." and ".." are returned).
//
// Mirrors what the handler-level path does end-to-end: parse the wire SD,
// persist via SetFileAttributes, then enumerate through QueryDirectory.
// The unit test TestFilterByAccess_PartialReadMaskHidesFromOwner already
// proves the eval path is correct in isolation; this test pins the wiring
// between SET_INFO Security → metadata store → QueryDirectory ABE filter so
// regressions in the SD parser, the per-share toggle, or ListChildren ACL
// hydration are caught at the Go test level instead of only in CI smbtorture.
//
// Refs #603.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupAccessBasedReproShare wires a runtime + memory store + handler with a
// single share that has ABE enabled, mirroring how `hideunread` is configured
// in test/smb-conformance/bootstrap.sh. The returned auth context represents
// the smbtorture caller (wpts-admin, UID 1000) and is reused for both the
// CREATE and the SET_INFO Security step — wpts-admin owns the file and is
// also the principal whose SID lands inside the DACL.
//
// The returned SMBHandlerContext intentionally registers wpts-admin both on
// the session (h.SessionManager) AND on ctx.User. The session registration
// is what production code paths recover from openFile.SessionID — the per-
// request ctx.User is cleared before QUERY_DIRECTORY in the production
// dispatcher, so the handler must look it back up from the session for
// ABE to work (refs #603).
func setupAccessBasedReproShare(t *testing.T) (*Handler, *runtime.Runtime, metadata.FileHandle, *SMBHandlerContext, *metadata.AuthContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("abe-repro-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/hideunread"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:                   shareName,
		MetadataStore:          "abe-repro-meta",
		Enabled:                true,
		AccessBasedEnumeration: true,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			// World-writable root so the caller (UID 1000, distinct from
			// the default UID-0 owner) can create the test file. The
			// downstream ABE filter is what we're exercising — root
			// permission shaping is an unrelated setup concern.
			Mode: 0o777,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	const callerUID uint32 = 1000
	const callerGID uint32 = 1000
	uid, gid := callerUID, callerGID
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}

	h := NewHandler()
	h.Registry = rt

	// Register the caller as an authenticated SMB session. The handler will
	// look this back up from openFile.SessionID to recover ctx.User before
	// building the AuthContext. Without that hand-off BuildAuthContext takes
	// the anonymous (UID 0) arm and ABE root-bypasses the filter (refs #603).
	sess := h.CreateSession("127.0.0.1:12345", false, "wpts-admin", "")
	sess.User = &models.User{
		Username: "wpts-admin",
		UID:      &uid,
		Groups:   []models.Group{{GID: &gid}},
	}

	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:                 treeID,
		SessionID:              sess.SessionID,
		ShareName:              shareName,
		AccessBasedEnumeration: true,
	})

	// Production dispatcher does NOT populate ctx.User before
	// QUERY_DIRECTORY — that's the whole point of the bug. We mirror that
	// here: ctx.User stays nil and the handler must recover the identity
	// from openFile.SessionID via h.SessionManager.
	smbCtx := &SMBHandlerContext{
		Context:   context.Background(),
		TreeID:    treeID,
		SessionID: sess.SessionID,
	}

	return h, rt, rootHandle, smbCtx, authCtx
}

// setSDForAccessBased builds the SET_INFO Security wire SD that mirrors
// `security_descriptor_dacl_create(... owner_sid, ACCESS_ALLOWED, mask|SYNC,
// 0, NULL)` from source4/torture/smb2/acls.c. Owner section is absent (the
// smbtorture call passes SECINFO_DACL only); the lone ACE targets the file
// owner's SID with the per-iteration mask plus SEC_STD_SYNCHRONIZE.
func setSDForAccessBased(t *testing.T, ownerUID uint32, accessMask uint32) []byte {
	t.Helper()
	// defaultSIDMapper is the package-level mapper used by the handler.
	// UserSID(1000) returns the canonical wpts-admin SID for this build.
	ownerSID := defaultSIDMapper.UserSID(ownerUID)
	ownerSIDStr := sid.FormatSID(ownerSID)
	ownerBytes := buildSID(t, ownerSIDStr)

	const secStdSynchronize uint32 = 0x00100000
	ace := buildACE(accessAllowedACEType, 0x00, accessMask|secStdSynchronize, ownerBytes)
	dacl := buildRawDACL(1, ace)
	// Control: DACL_PRESENT only. No owner, no group, no SACL.
	return buildSelfRelativeSD(t, seDACLPresent, nil, nil, dacl)
}

// TestSetSecurityInfoThenQueryDirectory_AccessBasedHidesPartialMask is the
// end-to-end reproducer for smbtorture acls.c:2308. The test seeds a single
// file under an ABE-enabled share, sets a DACL via setSecurityInfo (the
// same handler the SMB SET_INFO dispatcher calls), and then drives
// QueryDirectory and counts the visible entries.
//
// All sub-cases enumerate AS THE FILE'S OWNER. Iteration 0 grants the full
// Samba `user_can_read_fsp` mask and asserts the file IS visible (mirrors
// smbtorture's expected count of 3 — ".", "..", file). Subsequent iterations
// strip one read bit at a time and assert count is 2 (".", "..", no file).
func TestSetSecurityInfoThenQueryDirectory_AccessBasedHidesPartialMask(t *testing.T) {
	const (
		// MS-DTYP §2.4.4.1 Windows access mask bits as used by smbtorture's
		// SEC_RIGHTS_FILE_READ shape.
		secStdReadControl  uint32 = 0x00020000
		fileReadData       uint32 = 0x00000001
		fileReadEA         uint32 = 0x00000008
		fileReadAttributes uint32 = 0x00000080
		secRightsFileFull  uint32 = 0x001F01FF
	)

	cases := []struct {
		name        string
		accessMask  uint32
		wantEntries int // ".", "..", + file when visible.
	}{
		{
			name:        "iter0_full_mask_visible",
			accessMask:  secStdReadControl | fileReadData | fileReadAttributes | fileReadEA,
			wantEntries: 3,
		},
		{
			name:        "iter1_missing_read_ea_hidden_0x20081",
			accessMask:  secStdReadControl | fileReadData | fileReadAttributes,
			wantEntries: 2,
		},
		{
			name:        "iter2_missing_read_attributes_hidden",
			accessMask:  secStdReadControl | fileReadData | fileReadEA,
			wantEntries: 2,
		},
		{
			name:        "iter3_missing_read_data_hidden",
			accessMask:  secStdReadControl | fileReadAttributes | fileReadEA,
			wantEntries: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rt, rootHandle, smbCtx, authCtx := setupAccessBasedReproShare(t)

			// Replicate the smbtorture layout: BASEDIR `smb2-testsd` is a
			// subdirectory, `testfile` is the regular file inside it whose
			// SD we will set. Enumeration is then on the BASEDIR.
			metaSvc := rt.GetMetadataService()
			baseDir, err := metaSvc.CreateDirectory(authCtx, rootHandle, "smb2-testsd",
				&metadata.FileAttr{
					Mode: 0o755,
				})
			if err != nil {
				t.Fatalf("CreateFile (BASEDIR): %v", err)
			}
			baseDirHandle, err := metadata.EncodeFileHandle(baseDir)
			if err != nil {
				t.Fatalf("EncodeFileHandle (BASEDIR): %v", err)
			}

			child, err := metaSvc.CreateFile(authCtx, baseDirHandle, "testfile",
				&metadata.FileAttr{
					Type: metadata.FileTypeRegular,
					Mode: 0o644,
				})
			if err != nil {
				t.Fatalf("CreateFile (testfile): %v", err)
			}
			childHandle, err := metadata.EncodeFileHandle(child)
			if err != nil {
				t.Fatalf("EncodeFileHandle: %v", err)
			}

			// Open the file with full access so the SECINFO_DACL gate in
			// setSecurityInfo (handle must hold WRITE_DAC per
			// MS-SMB2 §3.3.5.21.3) is satisfied. Mirrors smbtorture which
			// opens with SEC_RIGHTS_FILE_ALL before the SETINFO.
			fileOpen := &OpenFile{
				FileID:         h.GenerateFileID(),
				TreeID:         smbCtx.TreeID,
				SessionID:      smbCtx.SessionID,
				ShareName:      "/hideunread",
				MetadataHandle: childHandle,
				ParentHandle:   baseDirHandle,
				FileName:       "testfile",
				Path:           "/hideunread/smb2-testsd/testfile",
				DesiredAccess:  secRightsFileFull,
				GrantedAccess:  secRightsFileFull,
			}
			h.StoreOpenFile(fileOpen)

			// Build and apply the wire SD via the same handler entrypoint
			// the SET_INFO dispatcher uses.
			sdBuf := setSDForAccessBased(t, 1000, tc.accessMask)
			resp, err := h.setSecurityInfo(authCtx, fileOpen, DACLSecurityInformation, sdBuf)
			if err != nil {
				t.Fatalf("setSecurityInfo: %v", err)
			}
			if resp == nil || resp.Status != types.StatusSuccess {
				var got uint32
				if resp != nil {
					got = uint32(resp.Status)
				}
				t.Fatalf("setSecurityInfo: status = 0x%x, want StatusSuccess", got)
			}

			// Sanity-check that the DACL actually landed on the persisted
			// file. If this fails the leak is at the SD parser / store
			// boundary, not in the ABE filter.
			persisted, err := metaSvc.GetFile(authCtx.Context, childHandle)
			if err != nil {
				t.Fatalf("GetFile after SET_INFO: %v", err)
			}
			if persisted.ACL == nil {
				t.Fatalf("hypothesis-2 confirmed: persisted ACL is nil after SET_INFO Security; ParseSecurityDescriptor dropped the DACL on the floor (wire SD: % x)", sdBuf)
			}
			if len(persisted.ACL.ACEs) == 0 {
				t.Fatalf("persisted ACL has no ACEs; SET_INFO Security wire path lost the entire ACE array")
			}

			// Open the BASEDIR for QUERY_DIRECTORY — matches the
			// smbtorture `torture_smb2_testdir(tree1, BASEDIR, &dhandle)`
			// step where the enumeration happens against the subdirectory,
			// not the share root.
			dirOpen := &OpenFile{
				FileID:         h.GenerateFileID(),
				TreeID:         smbCtx.TreeID,
				SessionID:      smbCtx.SessionID,
				ShareName:      "/hideunread",
				MetadataHandle: baseDirHandle,
				ParentHandle:   rootHandle,
				Path:           "/hideunread/smb2-testsd",
				IsDirectory:    true,
				DesiredAccess:  secRightsFileFull,
				GrantedAccess:  secRightsFileFull,
			}
			h.StoreOpenFile(dirOpen)

			qResp := callABEQuery(t, h, smbCtx, dirOpen.FileID)
			if qResp.Status != types.StatusSuccess {
				t.Fatalf("QueryDirectory: status = 0x%x, want StatusSuccess",
					uint32(qResp.Status))
			}
			got := countABEEntries(qResp.Data)
			if got != tc.wantEntries {
				visibility := "should be visible"
				if tc.wantEntries == 2 {
					visibility = "should be hidden"
				}
				t.Fatalf("entries = %d, want %d (mask=0x%x — file %s)",
					got, tc.wantEntries, tc.accessMask, visibility)
			}
		})
	}
}
