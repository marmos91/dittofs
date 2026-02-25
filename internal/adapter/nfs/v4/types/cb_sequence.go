// Package types - CB_SEQUENCE callback operation types (RFC 8881 Section 20.9).
//
// CB_SEQUENCE is the callback-channel analogue of SEQUENCE. It must be the
// first operation in every CB_COMPOUND sent to a v4.1 client. It carries
// slot-based exactly-once semantics and referring_call_lists for duplicate
// request detection across sessions.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// CB_SEQUENCE4args (RFC 8881 Section 20.9.1)
// ============================================================================

// CbSequenceArgs represents CB_SEQUENCE4args per RFC 8881 Section 20.9.
//
//	struct CB_SEQUENCE4args {
//	    sessionid4           csa_sessionid;
//	    sequenceid4          csa_sequenceid;
//	    slotid4              csa_slotid;
//	    slotid4              csa_highest_slotid;
//	    bool                 csa_cachethis;
//	    referring_call_list4 csa_referring_call_lists<>;
//	};
type CbSequenceArgs struct {
	SessionID          SessionId4
	SequenceID         uint32
	SlotID             uint32
	HighestSlotID      uint32
	CacheThis          bool
	ReferringCallLists []ReferringCallTriple
}

// Encode writes the CB_SEQUENCE args in XDR format.
func (a *CbSequenceArgs) Encode(buf *bytes.Buffer) error {
	if err := a.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode cb_sequence sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.SequenceID); err != nil {
		return fmt.Errorf("encode cb_sequence sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.SlotID); err != nil {
		return fmt.Errorf("encode cb_sequence slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.HighestSlotID); err != nil {
		return fmt.Errorf("encode cb_sequence highest_slotid: %w", err)
	}
	if err := xdr.WriteBool(buf, a.CacheThis); err != nil {
		return fmt.Errorf("encode cb_sequence cachethis: %w", err)
	}
	// referring_call_lists<>
	count := uint32(len(a.ReferringCallLists))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode cb_sequence referring_call_lists count: %w", err)
	}
	for i := range a.ReferringCallLists {
		if err := a.ReferringCallLists[i].Encode(buf); err != nil {
			return fmt.Errorf("encode cb_sequence referring_call_list[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the CB_SEQUENCE args from XDR format.
func (a *CbSequenceArgs) Decode(r io.Reader) error {
	if err := a.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode cb_sequence sessionid: %w", err)
	}
	var err error
	if a.SequenceID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence sequenceid: %w", err)
	}
	if a.SlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence slotid: %w", err)
	}
	if a.HighestSlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence highest_slotid: %w", err)
	}
	if a.CacheThis, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode cb_sequence cachethis: %w", err)
	}
	// referring_call_lists<>
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_sequence referring_call_lists count: %w", err)
	}
	if count > 1024 {
		return fmt.Errorf("cb_sequence referring_call_lists count %d exceeds limit", count)
	}
	a.ReferringCallLists = make([]ReferringCallTriple, count)
	for i := uint32(0); i < count; i++ {
		if err := a.ReferringCallLists[i].Decode(r); err != nil {
			return fmt.Errorf("decode cb_sequence referring_call_list[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbSequenceArgs) String() string {
	return fmt.Sprintf("CbSequenceArgs{session=%s, seq=%d, slot=%d, highest=%d, cache=%t, refs=%d}",
		a.SessionID.String(), a.SequenceID, a.SlotID, a.HighestSlotID,
		a.CacheThis, len(a.ReferringCallLists))
}

// ============================================================================
// CB_SEQUENCE4res (RFC 8881 Section 20.9.2)
// ============================================================================

// CbSequenceRes represents CB_SEQUENCE4res per RFC 8881 Section 20.9.
//
//	union CB_SEQUENCE4res switch (nfsstat4 csr_status) {
//	 case NFS4_OK:
//	    sessionid4      csr_sessionid;
//	    sequenceid4     csr_sequenceid;
//	    slotid4         csr_slotid;
//	    slotid4         csr_highest_slotid;
//	    slotid4         csr_target_highest_slotid;
//	 default:
//	    void;
//	};
type CbSequenceRes struct {
	Status              uint32
	SessionID           SessionId4 // only if NFS4_OK
	SequenceID          uint32     // only if NFS4_OK
	SlotID              uint32     // only if NFS4_OK
	HighestSlotID       uint32     // only if NFS4_OK
	TargetHighestSlotID uint32     // only if NFS4_OK
}

// Encode writes the CB_SEQUENCE result in XDR format.
func (res *CbSequenceRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_sequence status: %w", err)
	}
	if res.Status != NFS4_OK {
		return nil
	}
	if err := res.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode cb_sequence sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, res.SequenceID); err != nil {
		return fmt.Errorf("encode cb_sequence sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, res.SlotID); err != nil {
		return fmt.Errorf("encode cb_sequence slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, res.HighestSlotID); err != nil {
		return fmt.Errorf("encode cb_sequence highest_slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, res.TargetHighestSlotID); err != nil {
		return fmt.Errorf("encode cb_sequence target_highest_slotid: %w", err)
	}
	return nil
}

// Decode reads the CB_SEQUENCE result from XDR format.
func (res *CbSequenceRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_sequence status: %w", err)
	}
	res.Status = status
	if res.Status != NFS4_OK {
		return nil
	}
	if err := res.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode cb_sequence sessionid: %w", err)
	}
	if res.SequenceID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence sequenceid: %w", err)
	}
	if res.SlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence slotid: %w", err)
	}
	if res.HighestSlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence highest_slotid: %w", err)
	}
	if res.TargetHighestSlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_sequence target_highest_slotid: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (res *CbSequenceRes) String() string {
	if res.Status != NFS4_OK {
		return fmt.Sprintf("CbSequenceRes{status=%d}", res.Status)
	}
	return fmt.Sprintf("CbSequenceRes{OK, session=%s, seq=%d, slot=%d, highest=%d, target=%d}",
		res.SessionID.String(), res.SequenceID, res.SlotID,
		res.HighestSlotID, res.TargetHighestSlotID)
}
