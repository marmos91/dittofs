// Package types - FREE_STATEID operation types (RFC 8881 Section 18.38).
//
// FREE_STATEID frees a stateid that the client no longer needs.
// The server can then reclaim any resources associated with it.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// FREE_STATEID Args (RFC 8881 Section 18.38.1)
// ============================================================================

// FreeStateidArgs represents FREE_STATEID4args per RFC 8881 Section 18.38.
//
//	struct FREE_STATEID4args {
//	    stateid4 fsa_stateid;
//	};
type FreeStateidArgs struct {
	Stateid Stateid4
}

// Encode writes the FREE_STATEID args in XDR format.
func (a *FreeStateidArgs) Encode(buf *bytes.Buffer) error {
	EncodeStateid4(buf, &a.Stateid)
	return nil
}

// Decode reads the FREE_STATEID args from XDR format.
func (a *FreeStateidArgs) Decode(r io.Reader) error {
	sid, err := DecodeStateid4(r)
	if err != nil {
		return fmt.Errorf("decode free_stateid args: %w", err)
	}
	a.Stateid = *sid
	return nil
}

// String returns a human-readable representation.
func (a *FreeStateidArgs) String() string {
	return fmt.Sprintf("FreeStateidArgs{stateid={seq=%d, other=%x}}",
		a.Stateid.Seqid, a.Stateid.Other)
}

// ============================================================================
// FREE_STATEID Res (RFC 8881 Section 18.38.2)
// ============================================================================

// FreeStateidRes represents FREE_STATEID4res per RFC 8881 Section 18.38.
//
//	struct FREE_STATEID4res {
//	    nfsstat4 fsr_status;
//	};
type FreeStateidRes struct {
	Status uint32
}

// Encode writes the FREE_STATEID result in XDR format.
func (res *FreeStateidRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode free_stateid status: %w", err)
	}
	return nil
}

// Decode reads the FREE_STATEID result from XDR format.
func (res *FreeStateidRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode free_stateid status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *FreeStateidRes) String() string {
	return fmt.Sprintf("FreeStateidRes{status=%d}", res.Status)
}
