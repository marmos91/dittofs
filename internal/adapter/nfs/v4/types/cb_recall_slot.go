// Package types - CB_RECALL_SLOT callback operation types (RFC 8881 Section 20.8).
//
// CB_RECALL_SLOT tells the client to reduce its backchannel slot table size
// to the specified target highest slot ID. The server uses this when it needs
// to reclaim backchannel resources.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// CB_RECALL_SLOT4args (RFC 8881 Section 20.8.1)
// ============================================================================

// CbRecallSlotArgs represents CB_RECALL_SLOT4args per RFC 8881 Section 20.8.
//
//	struct CB_RECALL_SLOT4args {
//	    slotid4 rsa_target_highest_slotid;
//	};
type CbRecallSlotArgs struct {
	TargetHighestSlotID uint32
}

// Encode writes the CB_RECALL_SLOT args in XDR format.
func (a *CbRecallSlotArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.TargetHighestSlotID); err != nil {
		return fmt.Errorf("encode cb_recall_slot target_highest_slotid: %w", err)
	}
	return nil
}

// Decode reads the CB_RECALL_SLOT args from XDR format.
func (a *CbRecallSlotArgs) Decode(r io.Reader) error {
	var err error
	if a.TargetHighestSlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_recall_slot target_highest_slotid: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbRecallSlotArgs) String() string {
	return fmt.Sprintf("CbRecallSlotArgs{target_highest_slotid=%d}", a.TargetHighestSlotID)
}

// ============================================================================
// CB_RECALL_SLOT4res (RFC 8881 Section 20.8.2)
// ============================================================================

// CbRecallSlotRes represents CB_RECALL_SLOT4res per RFC 8881 Section 20.8.
//
//	struct CB_RECALL_SLOT4res {
//	    nfsstat4 rsr_status;
//	};
type CbRecallSlotRes struct {
	Status uint32
}

// Encode writes the CB_RECALL_SLOT result in XDR format.
func (res *CbRecallSlotRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_recall_slot status: %w", err)
	}
	return nil
}

// Decode reads the CB_RECALL_SLOT result from XDR format.
func (res *CbRecallSlotRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_recall_slot status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbRecallSlotRes) String() string {
	return fmt.Sprintf("CbRecallSlotRes{status=%d}", res.Status)
}
