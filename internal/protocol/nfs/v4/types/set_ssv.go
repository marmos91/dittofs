// Package types - SET_SSV operation types (RFC 8881 Section 18.47).
//
// SET_SSV sets the SSV (Secret State Verifier) for SP4_SSV state protection.
// SP4_SSV is out of scope per REQUIREMENTS.md, but the types are needed
// for wire compatibility with compliant clients.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// SET_SSV Args (RFC 8881 Section 18.47.1)
// ============================================================================

// SetSsvArgs represents SET_SSV4args per RFC 8881 Section 18.47.
//
//	struct SET_SSV4args {
//	    opaque ssa_ssv<>;
//	    opaque ssa_digest<>;
//	};
type SetSsvArgs struct {
	SSV    []byte // secret state verifier value
	Digest []byte // HMAC digest over SSV
}

// Encode writes the SET_SSV args in XDR format.
func (a *SetSsvArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDROpaque(buf, a.SSV); err != nil {
		return fmt.Errorf("encode set_ssv ssv: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, a.Digest); err != nil {
		return fmt.Errorf("encode set_ssv digest: %w", err)
	}
	return nil
}

// Decode reads the SET_SSV args from XDR format.
func (a *SetSsvArgs) Decode(r io.Reader) error {
	ssv, err := xdr.DecodeOpaque(r)
	if err != nil {
		return fmt.Errorf("decode set_ssv ssv: %w", err)
	}
	a.SSV = ssv
	digest, err := xdr.DecodeOpaque(r)
	if err != nil {
		return fmt.Errorf("decode set_ssv digest: %w", err)
	}
	a.Digest = digest
	return nil
}

// String returns a human-readable representation.
func (a *SetSsvArgs) String() string {
	return fmt.Sprintf("SetSsvArgs{ssv=%d bytes, digest=%d bytes}",
		len(a.SSV), len(a.Digest))
}

// ============================================================================
// SET_SSV Res (RFC 8881 Section 18.47.2)
// ============================================================================

// SetSsvRes represents SET_SSV4res per RFC 8881 Section 18.47.
//
//	struct SET_SSV4resok {
//	    opaque ssr_digest<>;
//	};
//	union SET_SSV4res switch (nfsstat4 ssr_status) {
//	    case NFS4_OK:
//	        SET_SSV4resok ssr_resok4;
//	    default:
//	        void;
//	};
type SetSsvRes struct {
	Status uint32
	Digest []byte // HMAC digest (only if NFS4_OK)
}

// Encode writes the SET_SSV result in XDR format.
func (res *SetSsvRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode set_ssv status: %w", err)
	}
	if res.Status == NFS4_OK {
		if err := xdr.WriteXDROpaque(buf, res.Digest); err != nil {
			return fmt.Errorf("encode set_ssv digest: %w", err)
		}
	}
	return nil
}

// Decode reads the SET_SSV result from XDR format.
func (res *SetSsvRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode set_ssv status: %w", err)
	}
	res.Status = status
	if res.Status != NFS4_OK {
		res.Digest = nil
		return nil
	}
	digest, err := xdr.DecodeOpaque(r)
	if err != nil {
		return fmt.Errorf("decode set_ssv digest: %w", err)
	}
	res.Digest = digest
	return nil
}

// String returns a human-readable representation.
func (res *SetSsvRes) String() string {
	if res.Status == NFS4_OK {
		return fmt.Sprintf("SetSsvRes{status=OK, digest=%d bytes}", len(res.Digest))
	}
	return fmt.Sprintf("SetSsvRes{status=%d}", res.Status)
}
