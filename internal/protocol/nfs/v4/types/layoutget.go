// Package types - LAYOUTGET operation types (RFC 8881 Section 18.43).
//
// LAYOUTGET requests layout information for a file from the metadata server.
// The layout describes how to access the file's data directly from data servers
// (pNFS). Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// Layout I/O mode constants per RFC 8881 Section 3.3.20.
const (
	LAYOUTIOMODE4_READ = 1
	LAYOUTIOMODE4_RW   = 2
)

// ============================================================================
// Layout4 - Single layout segment
// ============================================================================

// Layout4 represents a single layout segment returned by LAYOUTGET.
//
//	struct layout4 {
//	    offset4        lo_offset;
//	    length4        lo_length;
//	    layoutiomode4  lo_iomode;
//	    layout_content4 lo_content;
//	};
//
//	struct layout_content4 {
//	    layouttype4 loc_type;
//	    opaque      loc_body<>;
//	};
type Layout4 struct {
	Offset uint64
	Length uint64
	IOMode uint32
	Type   uint32 // LAYOUT4_* constant
	Body   []byte // layout-type-specific opaque data
}

// Encode writes a Layout4 in XDR format.
func (l *Layout4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, l.Offset); err != nil {
		return fmt.Errorf("encode layout offset: %w", err)
	}
	if err := xdr.WriteUint64(buf, l.Length); err != nil {
		return fmt.Errorf("encode layout length: %w", err)
	}
	if err := xdr.WriteUint32(buf, l.IOMode); err != nil {
		return fmt.Errorf("encode layout iomode: %w", err)
	}
	if err := xdr.WriteUint32(buf, l.Type); err != nil {
		return fmt.Errorf("encode layout type: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, l.Body); err != nil {
		return fmt.Errorf("encode layout body: %w", err)
	}
	return nil
}

// Decode reads a Layout4 from XDR format.
func (l *Layout4) Decode(r io.Reader) error {
	var err error
	if l.Offset, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layout offset: %w", err)
	}
	if l.Length, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layout length: %w", err)
	}
	if l.IOMode, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layout iomode: %w", err)
	}
	if l.Type, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layout type: %w", err)
	}
	if l.Body, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode layout body: %w", err)
	}
	return nil
}

// ============================================================================
// LAYOUTGET Args (RFC 8881 Section 18.43.1)
// ============================================================================

// LayoutGetArgs represents LAYOUTGET4args per RFC 8881 Section 18.43.
//
//	struct LAYOUTGET4args {
//	    bool               loga_signal_layout_avail;
//	    layouttype4        loga_layout_type;
//	    layoutiomode4      loga_iomode;
//	    offset4            loga_offset;
//	    length4            loga_length;
//	    length4            loga_minlength;
//	    stateid4           loga_stateid;
//	    count4             loga_maxcount;
//	};
type LayoutGetArgs struct {
	Signal     bool
	LayoutType uint32 // LAYOUT4_* constant
	IOMode     uint32 // LAYOUTIOMODE4_READ or LAYOUTIOMODE4_RW
	Offset     uint64
	Length     uint64
	MinLength  uint64
	Stateid    Stateid4
	MaxCount   uint32
}

// Encode writes the LAYOUTGET args in XDR format.
func (a *LayoutGetArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteBool(buf, a.Signal); err != nil {
		return fmt.Errorf("encode layoutget signal: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.LayoutType); err != nil {
		return fmt.Errorf("encode layoutget layout_type: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.IOMode); err != nil {
		return fmt.Errorf("encode layoutget iomode: %w", err)
	}
	if err := xdr.WriteUint64(buf, a.Offset); err != nil {
		return fmt.Errorf("encode layoutget offset: %w", err)
	}
	if err := xdr.WriteUint64(buf, a.Length); err != nil {
		return fmt.Errorf("encode layoutget length: %w", err)
	}
	if err := xdr.WriteUint64(buf, a.MinLength); err != nil {
		return fmt.Errorf("encode layoutget min_length: %w", err)
	}
	EncodeStateid4(buf, &a.Stateid)
	if err := xdr.WriteUint32(buf, a.MaxCount); err != nil {
		return fmt.Errorf("encode layoutget max_count: %w", err)
	}
	return nil
}

// Decode reads the LAYOUTGET args from XDR format.
func (a *LayoutGetArgs) Decode(r io.Reader) error {
	signal, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode layoutget signal: %w", err)
	}
	a.Signal = signal
	if a.LayoutType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutget layout_type: %w", err)
	}
	if a.IOMode, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutget iomode: %w", err)
	}
	if a.Offset, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layoutget offset: %w", err)
	}
	if a.Length, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layoutget length: %w", err)
	}
	if a.MinLength, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode layoutget min_length: %w", err)
	}
	sid, err := DecodeStateid4(r)
	if err != nil {
		return fmt.Errorf("decode layoutget stateid: %w", err)
	}
	a.Stateid = *sid
	if a.MaxCount, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode layoutget max_count: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *LayoutGetArgs) String() string {
	return fmt.Sprintf("LayoutGetArgs{signal=%t, type=%d, iomode=%d, offset=%d, len=%d, min=%d, max=%d}",
		a.Signal, a.LayoutType, a.IOMode, a.Offset, a.Length, a.MinLength, a.MaxCount)
}

// ============================================================================
// LAYOUTGET Res (RFC 8881 Section 18.43.2)
// ============================================================================

// LayoutGetRes represents LAYOUTGET4res per RFC 8881 Section 18.43.
//
//	struct LAYOUTGET4resok {
//	    bool               logr_return_on_close;
//	    stateid4           logr_stateid;
//	    layout4            logr_layout<>;
//	};
//	union LAYOUTGET4res switch (nfsstat4 logr_status) {
//	    case NFS4_OK:
//	        LAYOUTGET4resok logr_resok4;
//	    case NFS4ERR_LAYOUTTRYLATER:
//	        bool            logr_will_signal_layout_avail;
//	    default:
//	        void;
//	};
type LayoutGetRes struct {
	Status        uint32
	ReturnOnClose bool      // only if NFS4_OK
	Stateid       Stateid4  // only if NFS4_OK
	Layouts       []Layout4 // only if NFS4_OK
	WillSignal    bool      // only if NFS4ERR_LAYOUTTRYLATER
}

// Encode writes the LAYOUTGET result in XDR format.
func (res *LayoutGetRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode layoutget status: %w", err)
	}
	switch res.Status {
	case NFS4_OK:
		if err := xdr.WriteBool(buf, res.ReturnOnClose); err != nil {
			return fmt.Errorf("encode layoutget return_on_close: %w", err)
		}
		EncodeStateid4(buf, &res.Stateid)
		count := uint32(len(res.Layouts))
		if err := xdr.WriteUint32(buf, count); err != nil {
			return fmt.Errorf("encode layoutget layouts count: %w", err)
		}
		for i := range res.Layouts {
			if err := res.Layouts[i].Encode(buf); err != nil {
				return fmt.Errorf("encode layoutget layout[%d]: %w", i, err)
			}
		}
	case NFS4ERR_LAYOUTTRYLATER:
		if err := xdr.WriteBool(buf, res.WillSignal); err != nil {
			return fmt.Errorf("encode layoutget will_signal: %w", err)
		}
	default:
		// void
	}
	return nil
}

// Decode reads the LAYOUTGET result from XDR format.
func (res *LayoutGetRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode layoutget status: %w", err)
	}
	res.Status = status
	switch res.Status {
	case NFS4_OK:
		roc, err := xdr.DecodeBool(r)
		if err != nil {
			return fmt.Errorf("decode layoutget return_on_close: %w", err)
		}
		res.ReturnOnClose = roc
		sid, err := DecodeStateid4(r)
		if err != nil {
			return fmt.Errorf("decode layoutget stateid: %w", err)
		}
		res.Stateid = *sid
		count, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode layoutget layouts count: %w", err)
		}
		if count > 256 {
			return fmt.Errorf("layoutget layouts count %d exceeds limit", count)
		}
		res.Layouts = make([]Layout4, count)
		for i := uint32(0); i < count; i++ {
			if err := res.Layouts[i].Decode(r); err != nil {
				return fmt.Errorf("decode layoutget layout[%d]: %w", i, err)
			}
		}
	case NFS4ERR_LAYOUTTRYLATER:
		ws, err := xdr.DecodeBool(r)
		if err != nil {
			return fmt.Errorf("decode layoutget will_signal: %w", err)
		}
		res.WillSignal = ws
	default:
		// void
	}
	return nil
}

// String returns a human-readable representation.
func (res *LayoutGetRes) String() string {
	if res.Status == NFS4_OK {
		return fmt.Sprintf("LayoutGetRes{status=OK, return_on_close=%t, layouts=%d}",
			res.ReturnOnClose, len(res.Layouts))
	}
	return fmt.Sprintf("LayoutGetRes{status=%d}", res.Status)
}
