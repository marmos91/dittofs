// Tests for SET_INFO Security with an owner/group SID that cannot be mapped to
// a local UID/GID (refs #1228, Fix B).
//
// Before the fix, ParseSecurityDescriptorWithOptions returned a nil
// ownerUID/ownerGID both when the SD omitted the SID section AND when the
// section was present but unmappable. setSecurityInfo then silently skipped the
// owner/group change and returned STATUS_SUCCESS — Windows believed the owner
// changed when nothing did.
//
// The corrected boundary (after smbtorture smb2.acls.SDFLAGSVSCHOWN): only a
// requested OWNER/GROUP change to a genuinely FOREIGN domain account (an
// S-1-5-21 SID from a different domain — resolvable only via AD/LDAP, #1231) is
// rejected (STATUS_INVALID_OWNER / STATUS_NONE_MAPPED). Well-known SIDs (World,
// BUILTIN\*, NT AUTHORITY\*), our own machine-domain SIDs, and DACL-only sets
// are accepted, matching real servers — Samba resolves well-known SIDs through
// idmap rather than failing.
//
// The test SID mapper (TestMain in security_test.go) is S-1-5-21-0-0-0, so:
//   - S-1-5-21-0-0-0-1000  → UID 0 (mappable owner, our domain)
//   - S-1-5-32-544          → BUILTIN\Administrators (well-known, unmappable, accepted)
//   - S-1-1-0               → World/Everyone (well-known, unmappable, accepted)
//   - S-1-5-21-9-9-9-500    → FOREIGN domain account (unmappable, rejected)
package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSDWithOwnerGroup assembles a self-relative SD carrying an optional owner
// SID, optional group SID, and a minimal DACL_PRESENT control with one allow
// ACE. Pass empty strings to omit the owner/group section entirely.
func buildSDWithOwnerGroup(t *testing.T, ownerSIDStr, groupSIDStr string) []byte {
	t.Helper()
	var ownerSID, groupSID []byte
	if ownerSIDStr != "" {
		ownerSID = buildSID(t, ownerSIDStr)
	}
	if groupSIDStr != "" {
		groupSID = buildSID(t, groupSIDStr)
	}
	// A single allow-WRITE_DATA ACE for a mappable SID so the DACL parse path
	// is exercised regardless of the owner/group SIDs.
	aceSID := buildSID(t, "S-1-5-21-0-0-0-1000")
	const writeData uint32 = 0x00000002
	ace := buildACE(accessAllowedACEType, 0x00, writeData, aceSID)
	dacl := buildRawDACL(1, ace)
	return buildSelfRelativeSD(t, uint16(seDACLPresent), ownerSID, groupSID, dacl)
}

func TestSetInfo_Security_UnmappableOwnerGroup(t *testing.T) {
	const (
		mappableSID   = "S-1-5-21-0-0-0-1000" // UID 0 under the test mapper (our domain)
		foreignSID    = "S-1-5-21-9-9-9-500"  // foreign domain account → rejected
		worldSID      = "S-1-1-0"             // well-known World/Everyone → accepted
		builtinAdmins = "S-1-5-32-544"        // well-known BUILTIN\Administrators → accepted
	)

	cases := []struct {
		name           string
		additionalInfo uint32
		ownerSIDStr    string
		groupSIDStr    string
		wantStatus     types.Status
	}{
		{
			// (a) owner requested + FOREIGN-domain unmappable SID present →
			// explicit failure, not silent success. The genuine #1228 fix.
			name:           "owner_requested_foreign_domain_sid_rejected",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    foreignSID,
			wantStatus:     types.StatusInvalidOwner,
		},
		{
			// (b) owner requested + mappable SID → success (owner applied).
			name:           "owner_requested_mappable_sid_succeeds",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    mappableSID,
			wantStatus:     types.StatusSuccess,
		},
		{
			// (c) DACL-only set with no owner info → unaffected by the gate, even
			// when a foreign owner SID is present in the SD but not requested.
			name:           "dacl_only_no_owner_info_unaffected",
			additionalInfo: DACLSecurityInformation,
			ownerSIDStr:    foreignSID, // present in SD but not requested
			wantStatus:     types.StatusSuccess,
		},
		{
			// group requested + FOREIGN-domain unmappable SID → STATUS_NONE_MAPPED.
			name:           "group_requested_foreign_domain_sid_rejected",
			additionalInfo: GroupSecurityInformation,
			groupSIDStr:    foreignSID,
			wantStatus:     types.StatusNoneMapped,
		},
		{
			// Owner requested but SD omits the owner section entirely → no gate
			// trips (nothing to map), set succeeds as a no-op.
			name:           "owner_requested_but_no_owner_sid_in_sd",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    "", // absent
			wantStatus:     types.StatusSuccess,
		},
		{
			// EXACT smbtorture smb2.acls.SDFLAGSVSCHOWN trigger: the test chowns
			// the owner to the World SID (S-1-1-0) via SECINFO_OWNER and expects
			// NT_STATUS_OK. World does not reverse to a local UID but is a
			// well-known principal, not a foreign domain account, so it must be
			// accepted as a no-op success — NOT STATUS_INVALID_OWNER.
			name:           "owner_set_to_world_sid_succeeds",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    worldSID,
			wantStatus:     types.StatusSuccess,
		},
		{
			// SDFLAGSVSCHOWN also re-sets the original owner. For a root-owned
			// file that is BUILTIN\Administrators (UserSID(0)) — well-known,
			// unmappable, must succeed.
			name:           "owner_set_to_builtin_administrators_succeeds",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    builtinAdmins,
			wantStatus:     types.StatusSuccess,
		},
		{
			// Group counterpart: setting the group to the World SID is a
			// well-known, unmappable no-op success.
			name:           "group_set_to_world_sid_succeeds",
			additionalInfo: GroupSecurityInformation,
			groupSIDStr:    worldSID,
			wantStatus:     types.StatusSuccess,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, openFile, authCtx := setupSecurityAuthzTest(t)
			// Grant every relevant right so the access gate (refs #559) never
			// masks the behavior under test.
			openFile.GrantedAccess = uint32(types.WriteDac | types.WriteOwner | types.AccessSystemSecurity)
			h.StoreOpenFile(openFile)

			sd := buildSDWithOwnerGroup(t, tc.ownerSIDStr, tc.groupSIDStr)

			resp, err := h.setSecurityInfo(authCtx, openFile, tc.additionalInfo, sd)
			if err != nil {
				t.Fatalf("setSecurityInfo: %v", err)
			}
			if resp.Status != tc.wantStatus {
				t.Fatalf("status = %s (0x%08x), want %s (0x%08x)",
					resp.Status, uint32(resp.Status), tc.wantStatus, uint32(tc.wantStatus))
			}
		})
	}
}

// TestSetInfo_Security_SDFLAGSVSCHOWN_ChownRoundTrip mirrors the exact
// owner-chown round-trip smbtorture smb2.acls.SDFLAGSVSCHOWN performs: set the
// owner to the World SID, then back to the file's original owner SID, asserting
// NT_STATUS_OK on both SETs (acls.c lines ~1760-1774). Both legs target
// SECINFO_OWNER with a well-known / current-owner SID that has no reverse UID,
// so neither may be rejected.
func TestSetInfo_Security_SDFLAGSVSCHOWN_ChownRoundTrip(t *testing.T) {
	h, openFile, authCtx := setupSecurityAuthzTest(t)
	openFile.GrantedAccess = uint32(types.WriteOwner)
	h.StoreOpenFile(openFile)

	// Leg 1: chown owner -> World (S-1-1-0).
	sdWorld := buildSDWithOwnerGroup(t, "S-1-1-0", "")
	resp, err := h.setSecurityInfo(authCtx, openFile, OwnerSecurityInformation, sdWorld)
	if err != nil {
		t.Fatalf("setSecurityInfo(world): %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("chown to World: status = %s (0x%08x), want StatusSuccess",
			resp.Status, uint32(resp.Status))
	}

	// Leg 2: chown owner -> original owner. The root-owned test file's owner SID
	// is UserSID(0) = BUILTIN\Administrators (S-1-5-32-544).
	sdOrig := buildSDWithOwnerGroup(t, "S-1-5-32-544", "")
	resp, err = h.setSecurityInfo(authCtx, openFile, OwnerSecurityInformation, sdOrig)
	if err != nil {
		t.Fatalf("setSecurityInfo(orig): %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("chown back to original owner: status = %s (0x%08x), want StatusSuccess",
			resp.Status, uint32(resp.Status))
	}
}

// TestSetInfo_Security_OwnerOffsetOutOfRange documents that presence detection
// is NOT bounds-gated: a SD with a non-zero OffsetOwner that points past the
// buffer is malformed-but-present. ParseSecurityDescriptorWithOptions ignores
// the out-of-range offset (ownerUID stays nil) and the raw owner SID fails to
// decode, so it is not a foreign domain SID. Under the corrected boundary that
// is accepted as a no-op success (only foreign domain SIDs error) — matching
// the lenient real-server semantics SDFLAGSVSCHOWN proved.
func TestSetInfo_Security_OwnerOffsetOutOfRange(t *testing.T) {
	h, openFile, authCtx := setupSecurityAuthzTest(t)
	openFile.GrantedAccess = uint32(types.WriteDac | types.WriteOwner)
	h.StoreOpenFile(openFile)

	// Start from a well-formed DACL-only SD (no owner section), then corrupt the
	// OffsetOwner field (header bytes 4..7, little-endian) to point past the end.
	sd := buildSDWithOwnerGroup(t, "", "")
	binary.LittleEndian.PutUint32(sd[4:8], uint32(len(sd))+0x100)

	resp, err := h.setSecurityInfo(authCtx, openFile, OwnerSecurityInformation, sd)
	if err != nil {
		t.Fatalf("setSecurityInfo: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = %s (0x%08x), want StatusSuccess (out-of-range owner "+
			"offset is not a foreign domain SID, so accepted as no-op)",
			resp.Status, uint32(resp.Status))
	}
}
