// Package types - WANT_DELEGATION operation types (RFC 8881 Section 18.49).
//
// WANT_DELEGATION allows a client to request a delegation on an already-opened
// file without re-opening it.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// WANT_DELEGATION Args (RFC 8881 Section 18.49.1)
// ============================================================================

// WantDelegationClaim represents the open_claim4 union for WANT_DELEGATION,
// limited to CLAIM_PREVIOUS per the operation definition.
//
//	union open_claim4 switch (open_claim_type4 claim) {
//	    case CLAIM_PREVIOUS:
//	        open_delegation_type4 delegate_type;
//	    default:
//	        void;
//	};
type WantDelegationClaim struct {
	ClaimType uint32 // CLAIM_PREVIOUS = 1
	DelegType uint32 // delegation type if CLAIM_PREVIOUS
}

// WantDelegationArgs represents WANT_DELEGATION4args per RFC 8881 Section 18.49.
//
//	struct WANT_DELEGATION4args {
//	    uint32_t wda_want;
//	    deleg_claim4 wda_claim;
//	};
type WantDelegationArgs struct {
	Want  uint32              // bitmap of delegation types wanted
	Claim WantDelegationClaim // delegation claim
}

// Encode writes the WANT_DELEGATION args in XDR format.
func (a *WantDelegationArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.Want); err != nil {
		return fmt.Errorf("encode want_delegation want: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.Claim.ClaimType); err != nil {
		return fmt.Errorf("encode want_delegation claim_type: %w", err)
	}
	if a.Claim.ClaimType == CLAIM_PREVIOUS {
		if err := xdr.WriteUint32(buf, a.Claim.DelegType); err != nil {
			return fmt.Errorf("encode want_delegation deleg_type: %w", err)
		}
	}
	return nil
}

// Decode reads the WANT_DELEGATION args from XDR format.
func (a *WantDelegationArgs) Decode(r io.Reader) error {
	want, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode want_delegation want: %w", err)
	}
	a.Want = want
	claimType, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode want_delegation claim_type: %w", err)
	}
	a.Claim.ClaimType = claimType
	if a.Claim.ClaimType == CLAIM_PREVIOUS {
		delegType, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode want_delegation deleg_type: %w", err)
		}
		a.Claim.DelegType = delegType
	}
	return nil
}

// String returns a human-readable representation.
func (a *WantDelegationArgs) String() string {
	return fmt.Sprintf("WantDelegationArgs{want=0x%08x, claim_type=%d, deleg_type=%d}",
		a.Want, a.Claim.ClaimType, a.Claim.DelegType)
}

// ============================================================================
// WANT_DELEGATION Res (RFC 8881 Section 18.49.2)
// ============================================================================

// WantDelegationRes represents WANT_DELEGATION4res per RFC 8881 Section 18.49.
//
// The result is a union switched on delegation_type:
//
//   - OPEN_DELEGATE_NONE: void (just status)
//
//   - OPEN_DELEGATE_READ/WRITE: delegation struct (stored as raw opaque)
//
//     union open_delegation4 switch (open_delegation_type4 delegation_type) {
//     case OPEN_DELEGATE_NONE:
//     void;
//     case OPEN_DELEGATE_READ:
//     open_read_delegation4 read;
//     case OPEN_DELEGATE_WRITE:
//     open_write_delegation4 write;
//     };
type WantDelegationRes struct {
	Status         uint32
	DelegationType uint32 // OPEN_DELEGATE_NONE/READ/WRITE (only if NFS4_OK)
	DelegationData []byte // raw opaque delegation body (only if READ/WRITE)
}

// Encode writes the WANT_DELEGATION result in XDR format.
func (res *WantDelegationRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode want_delegation status: %w", err)
	}
	if res.Status == NFS4_OK {
		if err := xdr.WriteUint32(buf, res.DelegationType); err != nil {
			return fmt.Errorf("encode want_delegation delegation_type: %w", err)
		}
		if res.DelegationType == OPEN_DELEGATE_READ || res.DelegationType == OPEN_DELEGATE_WRITE {
			if _, err := buf.Write(res.DelegationData); err != nil {
				return fmt.Errorf("encode want_delegation delegation_data: %w", err)
			}
		}
	}
	return nil
}

// Decode reads the WANT_DELEGATION result from XDR format.
func (res *WantDelegationRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode want_delegation status: %w", err)
	}
	res.Status = status
	if res.Status == NFS4_OK {
		delegType, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode want_delegation delegation_type: %w", err)
		}
		res.DelegationType = delegType
		// Delegation body is consumed by the handler for READ/WRITE types;
		// at the type level, we stop after the discriminant for NONE.
	}
	return nil
}

// String returns a human-readable representation.
func (res *WantDelegationRes) String() string {
	if res.Status == NFS4_OK {
		typeName := "NONE"
		switch res.DelegationType {
		case OPEN_DELEGATE_READ:
			typeName = "READ"
		case OPEN_DELEGATE_WRITE:
			typeName = "WRITE"
		}
		return fmt.Sprintf("WantDelegationRes{status=OK, deleg=%s, data=%d bytes}",
			typeName, len(res.DelegationData))
	}
	return fmt.Sprintf("WantDelegationRes{status=%d}", res.Status)
}
