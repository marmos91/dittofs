// Package types - CB_WANTS_CANCELLED callback operation types (RFC 8881 Section 20.10).
//
// CB_WANTS_CANCELLED tells the client that previously registered WANT
// notifications have been cancelled by the server. The two booleans indicate
// which want notifications are cancelled (contended vs resourced).
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// CB_WANTS_CANCELLED4args (RFC 8881 Section 20.10.1)
// ============================================================================

// CbWantsCancelledArgs represents CB_WANTS_CANCELLED4args per RFC 8881 Section 20.10.
//
//	struct CB_WANTS_CANCELLED4args {
//	    bool cwca_contended_wants_cancelled;
//	    bool cwca_resourced_wants_cancelled;
//	};
type CbWantsCancelledArgs struct {
	ContendedWantsCancelled bool
	ResourcedWantsCancelled bool
}

// Encode writes the CB_WANTS_CANCELLED args in XDR format.
func (a *CbWantsCancelledArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteBool(buf, a.ContendedWantsCancelled); err != nil {
		return fmt.Errorf("encode cb_wants_cancelled contended: %w", err)
	}
	if err := xdr.WriteBool(buf, a.ResourcedWantsCancelled); err != nil {
		return fmt.Errorf("encode cb_wants_cancelled resourced: %w", err)
	}
	return nil
}

// Decode reads the CB_WANTS_CANCELLED args from XDR format.
func (a *CbWantsCancelledArgs) Decode(r io.Reader) error {
	var err error
	if a.ContendedWantsCancelled, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode cb_wants_cancelled contended: %w", err)
	}
	if a.ResourcedWantsCancelled, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode cb_wants_cancelled resourced: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbWantsCancelledArgs) String() string {
	return fmt.Sprintf("CbWantsCancelledArgs{contended=%t, resourced=%t}",
		a.ContendedWantsCancelled, a.ResourcedWantsCancelled)
}

// ============================================================================
// CB_WANTS_CANCELLED4res (RFC 8881 Section 20.10.2)
// ============================================================================

// CbWantsCancelledRes represents CB_WANTS_CANCELLED4res per RFC 8881 Section 20.10.
//
//	struct CB_WANTS_CANCELLED4res {
//	    nfsstat4 cwcr_status;
//	};
type CbWantsCancelledRes struct {
	Status uint32
}

// Encode writes the CB_WANTS_CANCELLED result in XDR format.
func (res *CbWantsCancelledRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_wants_cancelled status: %w", err)
	}
	return nil
}

// Decode reads the CB_WANTS_CANCELLED result from XDR format.
func (res *CbWantsCancelledRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_wants_cancelled status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbWantsCancelledRes) String() string {
	return fmt.Sprintf("CbWantsCancelledRes{status=%d}", res.Status)
}
