// Package types - DESTROY_CLIENTID operation types (RFC 8881 Section 18.50).
//
// DESTROY_CLIENTID destroys the client ID and all state associated with it.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// DESTROY_CLIENTID Args (RFC 8881 Section 18.50.1)
// ============================================================================

// DestroyClientidArgs represents DESTROY_CLIENTID4args per RFC 8881 Section 18.50.
//
//	struct DESTROY_CLIENTID4args {
//	    clientid4 dca_clientid;
//	};
type DestroyClientidArgs struct {
	ClientID uint64
}

// Encode writes the DESTROY_CLIENTID args in XDR format.
func (a *DestroyClientidArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, a.ClientID); err != nil {
		return fmt.Errorf("encode destroy_clientid client_id: %w", err)
	}
	return nil
}

// Decode reads the DESTROY_CLIENTID args from XDR format.
func (a *DestroyClientidArgs) Decode(r io.Reader) error {
	cid, err := xdr.DecodeUint64(r)
	if err != nil {
		return fmt.Errorf("decode destroy_clientid client_id: %w", err)
	}
	a.ClientID = cid
	return nil
}

// String returns a human-readable representation.
func (a *DestroyClientidArgs) String() string {
	return fmt.Sprintf("DestroyClientidArgs{client_id=0x%016x}", a.ClientID)
}

// ============================================================================
// DESTROY_CLIENTID Res (RFC 8881 Section 18.50.2)
// ============================================================================

// DestroyClientidRes represents DESTROY_CLIENTID4res per RFC 8881 Section 18.50.
//
//	struct DESTROY_CLIENTID4res {
//	    nfsstat4 dcr_status;
//	};
type DestroyClientidRes struct {
	Status uint32
}

// Encode writes the DESTROY_CLIENTID result in XDR format.
func (res *DestroyClientidRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode destroy_clientid status: %w", err)
	}
	return nil
}

// Decode reads the DESTROY_CLIENTID result from XDR format.
func (res *DestroyClientidRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode destroy_clientid status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *DestroyClientidRes) String() string {
	return fmt.Sprintf("DestroyClientidRes{status=%d}", res.Status)
}
