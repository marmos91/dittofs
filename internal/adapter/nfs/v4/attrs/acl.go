package attrs

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// FATTR4_ACL Encoding/Decoding
// ============================================================================
//
// NFSv4 ACL wire format per RFC 7531 Section 6.2.1 (nfsace4):
//
//   struct nfsace4 {
//       acetype4        type;
//       aceflag4        flag;
//       acemask4        access_mask;
//       utf8str_mixed   who;
//   };
//
// FATTR4_ACL is encoded as:
//   uint32  acecount;   // number of ACEs
//   nfsace4 aces[acecount];

// EncodeACLAttr encodes an ACL into XDR format for the FATTR4_ACL attribute.
//
// If fileACL is nil, encodes 0 ACEs (empty ACL on the wire).
// Each ACE is encoded as: type(uint32) + flag(uint32) + access_mask(uint32) + who(XDR string).
func EncodeACLAttr(buf *bytes.Buffer, fileACL *acl.ACL) error {
	if fileACL == nil || len(fileACL.ACEs) == 0 {
		// Encode 0 ACEs
		return xdr.WriteUint32(buf, 0)
	}

	// Encode ACE count
	if err := xdr.WriteUint32(buf, uint32(len(fileACL.ACEs))); err != nil {
		return fmt.Errorf("encode ACL acecount: %w", err)
	}

	// Encode each ACE
	for i, ace := range fileACL.ACEs {
		if err := xdr.WriteUint32(buf, ace.Type); err != nil {
			return fmt.Errorf("encode ACE %d type: %w", i, err)
		}
		if err := xdr.WriteUint32(buf, ace.Flag); err != nil {
			return fmt.Errorf("encode ACE %d flag: %w", i, err)
		}
		if err := xdr.WriteUint32(buf, ace.AccessMask); err != nil {
			return fmt.Errorf("encode ACE %d access_mask: %w", i, err)
		}
		if err := xdr.WriteXDRString(buf, ace.Who); err != nil {
			return fmt.Errorf("encode ACE %d who: %w", i, err)
		}
	}

	return nil
}

// DecodeACLAttr decodes an ACL from XDR format for the FATTR4_ACL attribute.
//
// Reads the ace count followed by each nfsace4 struct. Returns an error if
// the ace count exceeds acl.MaxACECount (128) to prevent resource exhaustion.
// An ace count of 0 returns an empty (non-nil) ACL.
func DecodeACLAttr(reader io.Reader) (*acl.ACL, error) {
	aceCount, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode ACL acecount: %w", err)
	}

	if aceCount > uint32(acl.MaxACECount) {
		return nil, fmt.Errorf("ACL ace count %d exceeds maximum %d", aceCount, acl.MaxACECount)
	}

	aces := make([]acl.ACE, aceCount)
	for i := uint32(0); i < aceCount; i++ {
		aceType, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode ACE %d type: %w", i, err)
		}
		flag, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode ACE %d flag: %w", i, err)
		}
		mask, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode ACE %d access_mask: %w", i, err)
		}
		who, err := xdr.DecodeString(reader)
		if err != nil {
			return nil, fmt.Errorf("decode ACE %d who: %w", i, err)
		}

		aces[i] = acl.ACE{
			Type:       aceType,
			Flag:       flag,
			AccessMask: mask,
			Who:        who,
		}
	}

	return &acl.ACL{ACEs: aces}, nil
}

// ============================================================================
// FATTR4_ACLSUPPORT Encoding
// ============================================================================

// EncodeACLSupportAttr encodes the FATTR4_ACLSUPPORT attribute value.
//
// Per the locked decision "Full FATTR4_ACLSUPPORT bitmap in GETATTR (all 4
// ACE types reported)", we report support for all four ACE types:
// ALLOW, DENY, AUDIT, and ALARM.
func EncodeACLSupportAttr(buf *bytes.Buffer) error {
	return xdr.WriteUint32(buf, acl.FullACLSupport)
}
