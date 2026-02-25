// Package types - NFSv4.1 DESTROY_SESSION operation types.
//
// DESTROY_SESSION (op 44) per RFC 8881 Section 18.37.
// Destroys a session associated with a client ID.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// DESTROY_SESSION4args - Request
// ============================================================================

// DestroySessionArgs represents DESTROY_SESSION4args per RFC 8881 Section 18.37.
//
//	struct DESTROY_SESSION4args {
//	    sessionid4     dsa_sessionid;
//	};
type DestroySessionArgs struct {
	SessionID SessionId4
}

// Encode writes the DESTROY_SESSION args in XDR format.
func (a *DestroySessionArgs) Encode(buf *bytes.Buffer) error {
	if err := a.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode sessionid: %w", err)
	}
	return nil
}

// Decode reads the DESTROY_SESSION args from XDR format.
func (a *DestroySessionArgs) Decode(r io.Reader) error {
	if err := a.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode sessionid: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *DestroySessionArgs) String() string {
	return fmt.Sprintf("DESTROY_SESSION4args{session=%s}", a.SessionID.String())
}

// ============================================================================
// DESTROY_SESSION4res - Response
// ============================================================================

// DestroySessionRes represents DESTROY_SESSION4res per RFC 8881 Section 18.37.
//
//	struct DESTROY_SESSION4res {
//	    nfsstat4     dsr_status;
//	};
type DestroySessionRes struct {
	Status uint32
}

// Encode writes the DESTROY_SESSION response in XDR format.
func (r *DestroySessionRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, r.Status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	return nil
}

// Decode reads the DESTROY_SESSION response from XDR format.
func (r *DestroySessionRes) Decode(rd io.Reader) error {
	status, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	r.Status = status
	return nil
}

// String returns a human-readable representation.
func (r *DestroySessionRes) String() string {
	return fmt.Sprintf("DESTROY_SESSION4res{status=%d}", r.Status)
}
