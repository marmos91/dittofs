// Package types - CB_PUSH_DELEG callback operation types (RFC 8881 Section 20.5).
//
// CB_PUSH_DELEG offers a delegation to the client for a specified filehandle.
// The delegation is encoded as an open_delegation4 union -- stored as raw opaque
// since full delegation encoding already exists in v4.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// CB_PUSH_DELEG4args (RFC 8881 Section 20.5.1)
// ============================================================================

// CbPushDelegArgs represents CB_PUSH_DELEG4args per RFC 8881 Section 20.5.
//
//	struct CB_PUSH_DELEG4args {
//	    nfs_fh4          cpda_fh;
//	    open_delegation4 cpda_delegation;
//	};
type CbPushDelegArgs struct {
	FH         []byte // filehandle
	Delegation []byte // open_delegation4 as raw opaque
}

// Encode writes the CB_PUSH_DELEG args in XDR format.
func (a *CbPushDelegArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDROpaque(buf, a.FH); err != nil {
		return fmt.Errorf("encode cb_push_deleg fh: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, a.Delegation); err != nil {
		return fmt.Errorf("encode cb_push_deleg delegation: %w", err)
	}
	return nil
}

// Decode reads the CB_PUSH_DELEG args from XDR format.
func (a *CbPushDelegArgs) Decode(r io.Reader) error {
	var err error
	if a.FH, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode cb_push_deleg fh: %w", err)
	}
	if a.Delegation, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode cb_push_deleg delegation: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbPushDelegArgs) String() string {
	return fmt.Sprintf("CbPushDelegArgs{fh=%d bytes, delegation=%d bytes}",
		len(a.FH), len(a.Delegation))
}

// ============================================================================
// CB_PUSH_DELEG4res (RFC 8881 Section 20.5.2)
// ============================================================================

// CbPushDelegRes represents CB_PUSH_DELEG4res per RFC 8881 Section 20.5.
//
//	struct CB_PUSH_DELEG4res {
//	    nfsstat4 cpdr_status;
//	};
type CbPushDelegRes struct {
	Status uint32
}

// Encode writes the CB_PUSH_DELEG result in XDR format.
func (res *CbPushDelegRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_push_deleg status: %w", err)
	}
	return nil
}

// Decode reads the CB_PUSH_DELEG result from XDR format.
func (res *CbPushDelegRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_push_deleg status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbPushDelegRes) String() string {
	return fmt.Sprintf("CbPushDelegRes{status=%d}", res.Status)
}
