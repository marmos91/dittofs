// Package types - RECLAIM_COMPLETE operation types (RFC 8881 Section 18.51).
//
// RECLAIM_COMPLETE indicates the client has finished reclaiming state
// after a server restart. OneFS=false means all filesystems,
// OneFS=true means the current filesystem only.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// RECLAIM_COMPLETE Args (RFC 8881 Section 18.51.1)
// ============================================================================

// ReclaimCompleteArgs represents RECLAIM_COMPLETE4args per RFC 8881 Section 18.51.
//
//	struct RECLAIM_COMPLETE4args {
//	    bool rca_one_fs;
//	};
type ReclaimCompleteArgs struct {
	OneFS bool
}

// Encode writes the RECLAIM_COMPLETE args in XDR format.
func (a *ReclaimCompleteArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteBool(buf, a.OneFS); err != nil {
		return fmt.Errorf("encode reclaim_complete one_fs: %w", err)
	}
	return nil
}

// Decode reads the RECLAIM_COMPLETE args from XDR format.
func (a *ReclaimCompleteArgs) Decode(r io.Reader) error {
	oneFS, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode reclaim_complete one_fs: %w", err)
	}
	a.OneFS = oneFS
	return nil
}

// String returns a human-readable representation.
func (a *ReclaimCompleteArgs) String() string {
	return fmt.Sprintf("ReclaimCompleteArgs{one_fs=%t}", a.OneFS)
}

// ============================================================================
// RECLAIM_COMPLETE Res (RFC 8881 Section 18.51.2)
// ============================================================================

// ReclaimCompleteRes represents RECLAIM_COMPLETE4res per RFC 8881 Section 18.51.
//
//	struct RECLAIM_COMPLETE4res {
//	    nfsstat4 rcr_status;
//	};
type ReclaimCompleteRes struct {
	Status uint32
}

// Encode writes the RECLAIM_COMPLETE result in XDR format.
func (res *ReclaimCompleteRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode reclaim_complete status: %w", err)
	}
	return nil
}

// Decode reads the RECLAIM_COMPLETE result from XDR format.
func (res *ReclaimCompleteRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode reclaim_complete status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *ReclaimCompleteRes) String() string {
	return fmt.Sprintf("ReclaimCompleteRes{status=%d}", res.Status)
}
