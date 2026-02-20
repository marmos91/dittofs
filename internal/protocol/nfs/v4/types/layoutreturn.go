// Package types - LAYOUTRETURN operation types (RFC 8881 Section 18.44).
//
// LAYOUTRETURN returns layout segments to the metadata server.
// The return can be for a specific file, an entire filesystem, or all layouts.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// Layout return type constants per RFC 8881 Section 18.44.
const (
	LAYOUTRETURN4_FILE = 1
	LAYOUTRETURN4_FSID = 2
	LAYOUTRETURN4_ALL  = 3
)

// ============================================================================
// LAYOUTRETURN Args (RFC 8881 Section 18.44.1)
// ============================================================================

// LayoutReturnArgs represents LAYOUTRETURN4args per RFC 8881 Section 18.44.
//
//	struct LAYOUTRETURN4args {
//	    bool             lora_reclaim;
//	    layouttype4      lora_layout_type;
//	    layoutiomode4    lora_iomode;
//	    layoutreturn4    lora_layoutreturn;
//	};
//
//	union layoutreturn4 switch (layoutreturn_type4 lr_returntype) {
//	    case LAYOUTRETURN4_FILE:
//	        layoutreturn_file4 lr_layout;
//	    default:
//	        void;
//	};
type LayoutReturnArgs struct {
	Reclaim    bool
	LayoutType uint32 // LAYOUT4_* constant
	IOMode     uint32 // LAYOUTIOMODE4_READ or LAYOUTIOMODE4_RW
	ReturnType uint32 // LAYOUTRETURN4_FILE, LAYOUTRETURN4_FSID, or LAYOUTRETURN4_ALL

	// File-specific fields (only if ReturnType == LAYOUTRETURN4_FILE)
	Offset  uint64
	Length  uint64
	Stateid Stateid4
	Body    []byte // opaque layout-type-specific return data
}

// Encode writes the LAYOUTRETURN args in XDR format.
func (a *LayoutReturnArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteBool(buf, a.Reclaim); err != nil {
		return fmt.Errorf("encode layoutreturn reclaim: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.LayoutType); err != nil {
		return fmt.Errorf("encode layoutreturn layout_type: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.IOMode); err != nil {
		return fmt.Errorf("encode layoutreturn iomode: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.ReturnType); err != nil {
		return fmt.Errorf("encode layoutreturn return_type: %w", err)
	}
	if a.ReturnType == LAYOUTRETURN4_FILE {
		if err := xdr.WriteUint64(buf, a.Offset); err != nil {
			return fmt.Errorf("encode layoutreturn offset: %w", err)
		}
		if err := xdr.WriteUint64(buf, a.Length); err != nil {
			return fmt.Errorf("encode layoutreturn length: %w", err)
		}
		EncodeStateid4(buf, &a.Stateid)
		if err := xdr.WriteXDROpaque(buf, a.Body); err != nil {
			return fmt.Errorf("encode layoutreturn body: %w", err)
		}
	}
	// LAYOUTRETURN4_FSID and LAYOUTRETURN4_ALL: void
	return nil
}

// Decode reads the LAYOUTRETURN args from XDR format.
func (a *LayoutReturnArgs) Decode(r io.Reader) error {
	var err error
	if a.Reclaim, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode layoutreturn reclaim: %w", err)
	}
	if a.LayoutType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutreturn layout_type: %w", err)
	}
	if a.IOMode, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutreturn iomode: %w", err)
	}
	if a.ReturnType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutreturn return_type: %w", err)
	}
	if a.ReturnType == LAYOUTRETURN4_FILE {
		if a.Offset, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode layoutreturn offset: %w", err)
		}
		if a.Length, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode layoutreturn length: %w", err)
		}
		sid, err := DecodeStateid4(r)
		if err != nil {
			return fmt.Errorf("decode layoutreturn stateid: %w", err)
		}
		a.Stateid = *sid
		if a.Body, err = xdr.DecodeOpaque(r); err != nil {
			return fmt.Errorf("decode layoutreturn body: %w", err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *LayoutReturnArgs) String() string {
	typeName := "FILE"
	switch a.ReturnType {
	case LAYOUTRETURN4_FSID:
		typeName = "FSID"
	case LAYOUTRETURN4_ALL:
		typeName = "ALL"
	}
	return fmt.Sprintf("LayoutReturnArgs{reclaim=%t, type=%d, iomode=%d, return=%s}",
		a.Reclaim, a.LayoutType, a.IOMode, typeName)
}

// ============================================================================
// LAYOUTRETURN Res (RFC 8881 Section 18.44.2)
// ============================================================================

// LayoutReturnRes represents LAYOUTRETURN4res per RFC 8881 Section 18.44.
//
//	union layoutreturn_stateid switch (bool lrs_present) {
//	    case TRUE:  stateid4 lrs_stateid;
//	    case FALSE: void;
//	};
//	union LAYOUTRETURN4res switch (nfsstat4 lorr_status) {
//	    case NFS4_OK:
//	        layoutreturn_stateid lorr_stateid;
//	    default:
//	        void;
//	};
type LayoutReturnRes struct {
	Status         uint32
	StateidPresent bool     // only if NFS4_OK
	Stateid        Stateid4 // only if StateidPresent
}

// Encode writes the LAYOUTRETURN result in XDR format.
func (res *LayoutReturnRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode layoutreturn status: %w", err)
	}
	if res.Status == NFS4_OK {
		if err := xdr.WriteBool(buf, res.StateidPresent); err != nil {
			return fmt.Errorf("encode layoutreturn stateid_present: %w", err)
		}
		if res.StateidPresent {
			EncodeStateid4(buf, &res.Stateid)
		}
	}
	return nil
}

// Decode reads the LAYOUTRETURN result from XDR format.
func (res *LayoutReturnRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode layoutreturn status: %w", err)
	}
	res.Status = status
	if res.Status != NFS4_OK {
		res.StateidPresent = false
		res.Stateid = Stateid4{}
		return nil
	}
	sp, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode layoutreturn stateid_present: %w", err)
	}
	res.StateidPresent = sp
	if !res.StateidPresent {
		res.Stateid = Stateid4{}
		return nil
	}
	sid, err := DecodeStateid4(r)
	if err != nil {
		return fmt.Errorf("decode layoutreturn stateid: %w", err)
	}
	res.Stateid = *sid
	return nil
}

// String returns a human-readable representation.
func (res *LayoutReturnRes) String() string {
	if res.Status == NFS4_OK {
		if res.StateidPresent {
			return fmt.Sprintf("LayoutReturnRes{status=OK, stateid={seq=%d}}", res.Stateid.Seqid)
		}
		return "LayoutReturnRes{status=OK, no_stateid}"
	}
	return fmt.Sprintf("LayoutReturnRes{status=%d}", res.Status)
}
