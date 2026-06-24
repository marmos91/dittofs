// Tests for SET_INFO Security with an owner/group SID that cannot be mapped to
// a local UID/GID (refs #1228, Fix B).
//
// Before the fix, ParseSecurityDescriptorWithOptions returned a nil
// ownerUID/ownerGID both when the SD omitted the SID section AND when the
// section was present but unmappable. setSecurityInfo then silently skipped the
// owner/group change and returned STATUS_SUCCESS — Windows believed the owner
// changed when nothing did. The fix rejects a requested OWNER/GROUP change
// whose SID is present-but-unmappable with STATUS_INVALID_OWNER /
// STATUS_NONE_MAPPED, while leaving DACL-only sets and mappable SIDs untouched.
//
// The test SID mapper (TestMain in security_test.go) is S-1-5-21-0-0-0, so:
//   - S-1-5-21-0-0-0-1000  → UID 0 (mappable owner)
//   - S-1-5-21-9-9-9-500    → unmappable (foreign domain, #1231 scope)
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
		mappableSID   = "S-1-5-21-0-0-0-1000" // UID 0 under the test mapper
		unmappableSID = "S-1-5-21-9-9-9-500"  // foreign domain → no local mapping
	)

	cases := []struct {
		name           string
		additionalInfo uint32
		ownerSIDStr    string
		groupSIDStr    string
		wantStatus     types.Status
	}{
		{
			// (a) owner requested + unmappable SID present → explicit failure,
			// not silent success.
			name:           "owner_requested_unmappable_sid_rejected",
			additionalInfo: OwnerSecurityInformation,
			ownerSIDStr:    unmappableSID,
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
			// (c) DACL-only set with no owner info → unaffected by the new gate.
			name:           "dacl_only_no_owner_info_unaffected",
			additionalInfo: DACLSecurityInformation,
			ownerSIDStr:    unmappableSID, // present in SD but not requested
			wantStatus:     types.StatusSuccess,
		},
		{
			// group requested + unmappable SID present → STATUS_NONE_MAPPED.
			name:           "group_requested_unmappable_sid_rejected",
			additionalInfo: GroupSecurityInformation,
			groupSIDStr:    unmappableSID,
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

// TestSetInfo_Security_OwnerOffsetOutOfRange covers the regression that
// presence detection must NOT bounds-gate the owner/group offset: a SD with a
// non-zero OffsetOwner that points past the buffer is malformed but still
// "present". ParseSecurityDescriptorWithOptions ignores the out-of-range offset
// and leaves the owner unmapped, so a requested OWNER change must be rejected
// (STATUS_INVALID_OWNER) rather than treated as an absent section and silently
// succeeding — the very silent-no-op bug #1228 fixes.
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
	if resp.Status != types.StatusInvalidOwner {
		t.Fatalf("status = %s (0x%08x), want STATUS_INVALID_OWNER (0x%08x) — "+
			"out-of-range owner offset must NOT be a silent success",
			resp.Status, uint32(resp.Status), uint32(types.StatusInvalidOwner))
	}
}
