package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSelfRelativeSDWithACE builds a minimal self-relative security descriptor
// whose DACL carries a single ACE with the given raw on-wire ACE size field.
// Layout: SD header (20 bytes) followed by the ACL (8-byte header + ACE body).
func buildSelfRelativeSDWithACE(t *testing.T, aceSizeField uint16) []byte {
	t.Helper()

	const aceBodyLen = 12 // type(1) + flags(1) + size(2) + mask(4) + 4 bytes of SID prefix
	sd := make([]byte, sdHeaderSize+aclHeaderSize+aceBodyLen)

	sd[0] = 1 // Revision
	control := uint16(0x0004 | 0x8000)
	binary.LittleEndian.PutUint16(sd[2:], control)
	binary.LittleEndian.PutUint32(sd[4:], 0)  // offsetOwner
	binary.LittleEndian.PutUint32(sd[8:], 0)  // offsetGroup
	binary.LittleEndian.PutUint32(sd[12:], 0) // offsetSACL

	daclOffset := uint32(sdHeaderSize)
	binary.LittleEndian.PutUint32(sd[16:], daclOffset) // offsetDACL

	acl := sd[daclOffset:]
	acl[0] = 2 // AclRevision
	binary.LittleEndian.PutUint16(acl[2:], uint16(len(acl)))
	binary.LittleEndian.PutUint16(acl[4:], 1) // aceCount

	ace := acl[aclHeaderSize:]
	ace[0] = 0x00 // ACCESS_ALLOWED_ACE_TYPE
	ace[1] = 0    // flags
	binary.LittleEndian.PutUint16(ace[2:], aceSizeField)
	binary.LittleEndian.PutUint32(ace[4:], 0x001F01FF) // mask
	return sd
}

// TestParseSecurityDescriptor_MalformedACESize_NoPanic pins that an ACE whose
// declared size is smaller than the fixed ACE header is rejected with an error
// (surfaced as STATUS_INVALID_PARAMETER by the SET_INFO caller) instead of
// panicking on an inverted slice bound. Before the guard, aceSize < 8 made
// data[offset+8 : offset+aceSize] a low>high slice and crashed the handler.
func TestParseSecurityDescriptor_MalformedACESize_NoPanic(t *testing.T) {
	for _, aceSize := range []uint16{0, 1, 4, 7} {
		sd := buildSelfRelativeSDWithACE(t, aceSize)

		var gotErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("aceSize=%d: panicked instead of returning an error: %v", aceSize, r)
				}
			}()
			_, _, _, gotErr = ParseSecurityDescriptor(sd)
		}()

		if gotErr == nil {
			t.Errorf("aceSize=%d: expected an error, got nil", aceSize)
		}
	}
}

// TestParseSecurityDescriptor_MalformedACESize_SurfacesInvalidParameter pins
// the wire answer: the SET_INFO SecurityInformation path maps a malformed-ACE
// parse error to STATUS_INVALID_PARAMETER (no success, no panic).
func TestParseSecurityDescriptor_MalformedACESize_SurfacesInvalidParameter(t *testing.T) {
	sd := buildSelfRelativeSDWithACE(t, 2)
	_, _, fileACL, err := ParseSecurityDescriptor(sd)
	if err == nil {
		t.Fatalf("expected a parse error for aceSize=2, got nil (ACL=%+v)", fileACL)
	}
}

// TestIoctlRequestIsFSCTL pins the IS_FSCTL gate: an IOCTL request body
// without Flags.IS_FSCTL is rejected, one with the bit passes the gate.
func TestIoctlRequestIsFSCTL(t *testing.T) {
	body := make([]byte, 56)
	// Flags at offset 48 per MS-SMB2 2.2.31; IS_FSCTL = 0x00000001.
	if ioctlRequestIsFSCTL(body) {
		t.Fatal("body without IS_FSCTL reported as FSCTL")
	}
	binary.LittleEndian.PutUint32(body[48:], 0x00000001)
	if !ioctlRequestIsFSCTL(body) {
		t.Fatal("body with IS_FSCTL not recognized")
	}
}

// TestValidateNegotiate_MaxOutputResponse pins that VALIDATE_NEGOTIATE_INFO
// with an output buffer too small for the 24-byte reply is rejected before
// validation instead of replying into a buffer that can never hold it.
func TestValidateNegotiate_MaxOutputResponse(t *testing.T) {
	h := &Handler{MinDialect: types.Dialect0202, MaxDialect: types.Dialect0311}
	body := make([]byte, 64)
	// InputCount at 16, MaxOutputResponse at 44 per the IOCTL request layout
	// (parseIoctlMaxOutputSize reads body[44:48]).
	binary.LittleEndian.PutUint32(body[16:], 24) // minimum input
	binary.LittleEndian.PutUint32(body[44:], 8)  // MaxOutputResponse too small
	res, err := h.handleValidateNegotiateInfo(nil, body)
	if err != nil {
		t.Fatalf("handleValidateNegotiateInfo: %v", err)
	}
	if res.Status != types.StatusInvalidParameter {
		t.Fatalf("Status = 0x%08X, want STATUS_INVALID_PARAMETER (0x%08X)", res.Status, types.StatusInvalidParameter)
	}

	// MaxOutputResponse = 0 means "no preference" (MS-SMB2 2.2.31) and must be
	// honored, not rejected by the too-small gate. Use a valid VNEG payload
	// (buildVNEGRequest writes MaxOutputResponse=0) so the only variable is the
	// gate itself.
	res2, err := h.handleValidateNegotiateInfo(&SMBHandlerContext{Context: context.Background()}, buildVNEGRequest(0x00000006, [16]byte{1, 2, 3, 4}, 0x0001, []uint16{uint16(types.Dialect0202), uint16(types.Dialect0210)}))
	if err != nil {
		t.Fatalf("handleValidateNegotiateInfo(maxOutput=0): %v", err)
	}
	if res2.Status == types.StatusInvalidParameter {
		t.Fatal("MaxOutputResponse=0 (no preference) rejected by the too-small gate")
	}
}
