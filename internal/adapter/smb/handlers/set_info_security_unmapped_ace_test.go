package handlers

import (
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestSetSecurityInfo_UnmappedACETypeRefused drives SET_INFO Security with a
// DACL whose deny is an ACE type the ACL model cannot hold. Skipping that ACE
// would store only the allow, so the denied principal could write. The whole
// set must fail with STATUS_INVALID_PARAMETER and leave the stored ACL as it was.
func TestSetSecurityInfo_UnmappedACETypeRefused(t *testing.T) {
	const (
		fileAllAccess  uint32 = 0x001F01FF
		fileWriteData  uint32 = 0x00000002
		deniedCallback uint8  = 0x0A // ACCESS_DENIED_CALLBACK_ACE_TYPE
		deniedObject   uint8  = 0x06 // ACCESS_DENIED_OBJECT_ACE_TYPE
	)
	bob := buildSID(t, "S-1-5-21-1000-1000-1000-1104")
	everyone := buildSID(t, "S-1-1-0")

	// An object ACE carries Flags(4) and, with ACE_OBJECT_TYPE_PRESENT, a
	// 16-byte ObjectType GUID between the mask and the SID (MS-DTYP §2.4.4.4).
	objectBody := append([]byte{0x01, 0x00, 0x00, 0x00}, make([]byte, 16)...)
	for i := range 16 {
		objectBody[4+i] = byte(0xA0 + i)
	}
	objectBody = append(objectBody, bob...)

	// An allow ACE whose AceSize (4) is below the 8-byte header: its SID slice
	// would start past its end.
	shortAllow := buildACE(accessAllowedACEType, 0, fileAllAccess, bob)
	shortAllow[2], shortAllow[3] = 4, 0

	for _, tc := range []struct {
		name string
		deny []byte
	}{
		{"callback_deny", buildACE(deniedCallback, 0, fileWriteData, bob)},
		{"object_deny", buildACE(deniedObject, 0, fileWriteData, objectBody)},
		{"short_allow", shortAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, openFile, authCtx := setupACLToggleTest(t, "/unmapped-ace-"+tc.name, true)
			metaSvc := h.Registry.GetMetadataService()

			// Seed a known ACL so "unchanged" has something to compare against.
			seed := buildSelfRelativeSD(t, seDACLPresent, nil, nil,
				buildRawDACL(1, buildACE(accessAllowedACEType, 0, fileAllAccess, bob)))
			resp, err := h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, seed)
			if err != nil || resp.Status != types.StatusSuccess {
				t.Fatalf("seed setSecurityInfo: resp=%+v err=%v", resp, err)
			}
			before, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
			if err != nil || before.ACL == nil {
				t.Fatalf("GetFile before: acl=%v err=%v", before, err)
			}

			aces := append(append([]byte{}, tc.deny...),
				buildACE(accessAllowedACEType, 0, fileAllAccess, everyone)...)
			sd := buildSelfRelativeSD(t, seDACLPresent, nil, nil, buildRawDACL(2, aces))
			resp, err = h.setSecurityInfo(authCtx, openFile, DACLSecurityInformation, sd)
			if err != nil {
				t.Fatalf("setSecurityInfo: %v", err)
			}
			if resp.Status != types.StatusInvalidParameter {
				t.Errorf("status = %v, want STATUS_INVALID_PARAMETER", resp.Status)
			}

			after, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
			if err != nil {
				t.Fatalf("GetFile after: %v", err)
			}
			if !reflect.DeepEqual(before.ACL, after.ACL) {
				t.Errorf("stored ACL changed:\n before=%+v\n after =%+v", before.ACL, after.ACL)
			}
		})
	}
}
