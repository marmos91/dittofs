// Package types - LAYOUTCOMMIT operation types (RFC 8881 Section 18.42).
//
// LAYOUTCOMMIT commits data written through a layout. It informs the metadata
// server about the regions that were written so it can update file metadata.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// LAYOUTCOMMIT Args (RFC 8881 Section 18.42.1)
// ============================================================================

// LayoutCommitArgs represents LAYOUTCOMMIT4args per RFC 8881 Section 18.42.
//
// PITFALL: This has 3 unions (offset, time, update). Encode conditionally
// based on the `Present` bool fields.
//
//	struct LAYOUTCOMMIT4args {
//	    offset4          loca_offset;
//	    length4          loca_length;
//	    bool             loca_reclaim;
//	    stateid4         loca_stateid;
//	    newoffset4       loca_last_write_offset;
//	    newtime4         loca_time_modify;
//	    layoutupdate4    loca_layoutupdate;
//	};
type LayoutCommitArgs struct {
	Offset           uint64
	Length           uint64
	Reclaim          bool
	Stateid          Stateid4
	NewOffsetPresent bool
	NewOffset        uint64   // only if NewOffsetPresent
	TimeModifyPresent bool
	TimeModify       NFS4Time // only if TimeModifyPresent
	LayoutUpdateType uint32
	LayoutUpdate     []byte   // opaque layout-type-specific data
}

// Encode writes the LAYOUTCOMMIT args in XDR format.
func (a *LayoutCommitArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, a.Offset); err != nil {
		return fmt.Errorf("encode layoutcommit offset: %w", err)
	}
	if err := xdr.WriteUint64(buf, a.Length); err != nil {
		return fmt.Errorf("encode layoutcommit length: %w", err)
	}
	if err := xdr.WriteBool(buf, a.Reclaim); err != nil {
		return fmt.Errorf("encode layoutcommit reclaim: %w", err)
	}
	EncodeStateid4(buf, &a.Stateid)

	// newoffset4 union: bool + optional uint64
	if err := xdr.WriteBool(buf, a.NewOffsetPresent); err != nil {
		return fmt.Errorf("encode layoutcommit new_offset_present: %w", err)
	}
	if a.NewOffsetPresent {
		if err := xdr.WriteUint64(buf, a.NewOffset); err != nil {
			return fmt.Errorf("encode layoutcommit new_offset: %w", err)
		}
	}

	// newtime4 union: bool + optional nfstime4
	if err := xdr.WriteBool(buf, a.TimeModifyPresent); err != nil {
		return fmt.Errorf("encode layoutcommit time_modify_present: %w", err)
	}
	if a.TimeModifyPresent {
		if err := xdr.WriteInt64(buf, a.TimeModify.Seconds); err != nil {
			return fmt.Errorf("encode layoutcommit time_modify.seconds: %w", err)
		}
		if err := xdr.WriteUint32(buf, a.TimeModify.Nseconds); err != nil {
			return fmt.Errorf("encode layoutcommit time_modify.nseconds: %w", err)
		}
	}

	// layoutupdate4: type + opaque body
	if err := xdr.WriteUint32(buf, a.LayoutUpdateType); err != nil {
		return fmt.Errorf("encode layoutcommit layout_update_type: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, a.LayoutUpdate); err != nil {
		return fmt.Errorf("encode layoutcommit layout_update: %w", err)
	}
	return nil
}

// Decode reads the LAYOUTCOMMIT args from XDR format.
func (a *LayoutCommitArgs) Decode(r io.Reader) error {
	var err error
	if a.Offset, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layoutcommit offset: %w", err)
	}
	if a.Length, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layoutcommit length: %w", err)
	}
	if a.Reclaim, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode layoutcommit reclaim: %w", err)
	}
	sid, err := DecodeStateid4(r)
	if err != nil {
		return fmt.Errorf("decode layoutcommit stateid: %w", err)
	}
	a.Stateid = *sid

	// newoffset4 union
	if a.NewOffsetPresent, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode layoutcommit new_offset_present: %w", err)
	}
	if a.NewOffsetPresent {
		if a.NewOffset, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode layoutcommit new_offset: %w", err)
		}
	}

	// newtime4 union
	if a.TimeModifyPresent, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode layoutcommit time_modify_present: %w", err)
	}
	if a.TimeModifyPresent {
		sec, err := xdr.DecodeInt64(r)
		if err != nil {
			return fmt.Errorf("decode layoutcommit time_modify.seconds: %w", err)
		}
		nsec, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode layoutcommit time_modify.nseconds: %w", err)
		}
		a.TimeModify = NFS4Time{Seconds: sec, Nseconds: nsec}
	}

	// layoutupdate4
	if a.LayoutUpdateType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutcommit layout_update_type: %w", err)
	}
	if a.LayoutUpdate, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode layoutcommit layout_update: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *LayoutCommitArgs) String() string {
	return fmt.Sprintf("LayoutCommitArgs{offset=%d, len=%d, reclaim=%t, new_offset=%t, time=%t, update_type=%d}",
		a.Offset, a.Length, a.Reclaim, a.NewOffsetPresent, a.TimeModifyPresent, a.LayoutUpdateType)
}

// ============================================================================
// LAYOUTCOMMIT Res (RFC 8881 Section 18.42.2)
// ============================================================================

// LayoutCommitRes represents LAYOUTCOMMIT4res per RFC 8881 Section 18.42.
//
//	struct LAYOUTCOMMIT4resok {
//	    newsize4 locr_newsize;
//	};
//	union newsize4 switch (bool ns_sizechanged) {
//	    case TRUE:  length4 ns_size;
//	    case FALSE: void;
//	};
type LayoutCommitRes struct {
	Status         uint32
	NewSizePresent bool   // only if NFS4_OK
	NewSize        uint64 // only if NewSizePresent
}

// Encode writes the LAYOUTCOMMIT result in XDR format.
func (res *LayoutCommitRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode layoutcommit status: %w", err)
	}
	if res.Status == NFS4_OK {
		if err := xdr.WriteBool(buf, res.NewSizePresent); err != nil {
			return fmt.Errorf("encode layoutcommit new_size_present: %w", err)
		}
		if res.NewSizePresent {
			if err := xdr.WriteUint64(buf, res.NewSize); err != nil {
				return fmt.Errorf("encode layoutcommit new_size: %w", err)
			}
		}
	}
	return nil
}

// Decode reads the LAYOUTCOMMIT result from XDR format.
func (res *LayoutCommitRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode layoutcommit status: %w", err)
	}
	res.Status = status
	if res.Status == NFS4_OK {
		nsp, err := xdr.DecodeBool(r)
		if err != nil {
			return fmt.Errorf("decode layoutcommit new_size_present: %w", err)
		}
		res.NewSizePresent = nsp
		if res.NewSizePresent {
			ns, err := xdr.DecodeUint64(r)
			if err != nil {
				return fmt.Errorf("decode layoutcommit new_size: %w", err)
			}
			res.NewSize = ns
		}
	}
	return nil
}

// String returns a human-readable representation.
func (res *LayoutCommitRes) String() string {
	if res.Status == NFS4_OK {
		if res.NewSizePresent {
			return fmt.Sprintf("LayoutCommitRes{status=OK, new_size=%d}", res.NewSize)
		}
		return "LayoutCommitRes{status=OK, no_size_change}"
	}
	return fmt.Sprintf("LayoutCommitRes{status=%d}", res.Status)
}
