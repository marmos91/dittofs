// Package types - NFSv4.1 SEQUENCE operation types.
//
// SEQUENCE (op 53) per RFC 8881 Section 18.46.
// SEQUENCE must be the first operation in every NFSv4.1 COMPOUND.
// It establishes slot-based exactly-once semantics for request replay detection.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// SEQUENCE4args - Request
// ============================================================================

// SequenceArgs represents SEQUENCE4args per RFC 8881 Section 18.46.
//
//	struct SEQUENCE4args {
//	    sessionid4     sa_sessionid;
//	    sequenceid4    sa_sequenceid;
//	    slotid4        sa_slotid;
//	    slotid4        sa_highest_slotid;
//	    bool           sa_cachethis;
//	};
type SequenceArgs struct {
	SessionID     SessionId4
	SequenceID    uint32
	SlotID        uint32
	HighestSlotID uint32
	CacheThis     bool // XDR bool: uint32 where 0=false, 1=true
}

// Encode writes the SEQUENCE args in XDR format.
func (a *SequenceArgs) Encode(buf *bytes.Buffer) error {
	if err := a.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.SequenceID); err != nil {
		return fmt.Errorf("encode sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.SlotID); err != nil {
		return fmt.Errorf("encode slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.HighestSlotID); err != nil {
		return fmt.Errorf("encode highest_slotid: %w", err)
	}
	if err := xdr.WriteBool(buf, a.CacheThis); err != nil {
		return fmt.Errorf("encode cachethis: %w", err)
	}
	return nil
}

// Decode reads the SEQUENCE args from XDR format.
func (a *SequenceArgs) Decode(r io.Reader) error {
	if err := a.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode sessionid: %w", err)
	}
	seqID, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode sequenceid: %w", err)
	}
	a.SequenceID = seqID
	slotID, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode slotid: %w", err)
	}
	a.SlotID = slotID
	highSlot, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode highest_slotid: %w", err)
	}
	a.HighestSlotID = highSlot
	cache, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode cachethis: %w", err)
	}
	a.CacheThis = cache
	return nil
}

// String returns a human-readable representation.
func (a *SequenceArgs) String() string {
	return fmt.Sprintf("SEQUENCE4args{session=%s, seq=%d, slot=%d, highest=%d, cache=%t}",
		a.SessionID.String(), a.SequenceID, a.SlotID, a.HighestSlotID, a.CacheThis)
}

// ============================================================================
// SEQUENCE4res - Response
// ============================================================================

// SequenceRes represents SEQUENCE4resok + status per RFC 8881 Section 18.46.
//
//	union SEQUENCE4res switch (nfsstat4 sr_status) {
//	 case NFS4_OK:
//	    sessionid4      sr_sessionid;
//	    sequenceid4     sr_sequenceid;
//	    slotid4         sr_slotid;
//	    slotid4         sr_highest_slotid;
//	    slotid4         sr_target_highest_slotid;
//	    uint32_t        sr_status_flags;
//	 default:
//	    void;
//	};
type SequenceRes struct {
	Status              uint32
	SessionID           SessionId4 // only if NFS4_OK
	SequenceID          uint32     // only if NFS4_OK
	SlotID              uint32     // only if NFS4_OK
	HighestSlotID       uint32     // only if NFS4_OK
	TargetHighestSlotID uint32     // only if NFS4_OK
	StatusFlags         uint32     // only if NFS4_OK, SEQ4_STATUS_* bitmask
}

// Encode writes the SEQUENCE response in XDR format.
func (r *SequenceRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, r.Status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	if r.Status != NFS4_OK {
		return nil
	}
	if err := r.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.SequenceID); err != nil {
		return fmt.Errorf("encode sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.SlotID); err != nil {
		return fmt.Errorf("encode slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.HighestSlotID); err != nil {
		return fmt.Errorf("encode highest_slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.TargetHighestSlotID); err != nil {
		return fmt.Errorf("encode target_highest_slotid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.StatusFlags); err != nil {
		return fmt.Errorf("encode status_flags: %w", err)
	}
	return nil
}

// Decode reads the SEQUENCE response from XDR format.
func (r *SequenceRes) Decode(rd io.Reader) error {
	status, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	r.Status = status
	if r.Status != NFS4_OK {
		r.SessionID = SessionId4{}
		r.SequenceID = 0
		r.SlotID = 0
		r.HighestSlotID = 0
		r.TargetHighestSlotID = 0
		r.StatusFlags = 0
		return nil
	}
	if err := r.SessionID.Decode(rd); err != nil {
		return fmt.Errorf("decode sessionid: %w", err)
	}
	seqID, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode sequenceid: %w", err)
	}
	r.SequenceID = seqID
	slotID, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode slotid: %w", err)
	}
	r.SlotID = slotID
	highSlot, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode highest_slotid: %w", err)
	}
	r.HighestSlotID = highSlot
	targetHigh, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode target_highest_slotid: %w", err)
	}
	r.TargetHighestSlotID = targetHigh
	flags, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status_flags: %w", err)
	}
	r.StatusFlags = flags
	return nil
}

// String returns a human-readable representation.
func (r *SequenceRes) String() string {
	if r.Status != NFS4_OK {
		return fmt.Sprintf("SEQUENCE4res{status=%d}", r.Status)
	}
	return fmt.Sprintf("SEQUENCE4res{OK, session=%s, seq=%d, slot=%d, highest=%d, target=%d, flags=0x%08x}",
		r.SessionID.String(), r.SequenceID, r.SlotID,
		r.HighestSlotID, r.TargetHighestSlotID, r.StatusFlags)
}
