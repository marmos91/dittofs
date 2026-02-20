// Package types - CB_LAYOUTRECALL callback operation types (RFC 8881 Section 20.3).
//
// CB_LAYOUTRECALL recalls layout segments from a client. The recall can target
// a specific file (with offset/length), an entire FSID, or all layouts.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// Layout recall type constants per RFC 8881 Section 20.3.
const (
	LAYOUTRECALL4_FILE = 1
	LAYOUTRECALL4_FSID = 2
	LAYOUTRECALL4_ALL  = 3
)

// ============================================================================
// CB_LAYOUTRECALL4args (RFC 8881 Section 20.3.1)
// ============================================================================

// CbLayoutRecallArgs represents CB_LAYOUTRECALL4args per RFC 8881 Section 20.3.
//
//	struct CB_LAYOUTRECALL4args {
//	    layouttype4             clora_type;
//	    layoutiomode4           clora_iomode;
//	    bool                    clora_changed;
//	    layoutrecall4           clora_recall;
//	};
//
//	union layoutrecall4 switch (layoutrecall_type4 lor_recalltype) {
//	    case LAYOUTRECALL4_FILE:
//	        layoutrecall_file4 lor_layout;
//	    case LAYOUTRECALL4_FSID:
//	        fsid4 lor_fsid;
//	    case LAYOUTRECALL4_ALL:
//	        void;
//	};
type CbLayoutRecallArgs struct {
	LayoutType uint32 // LAYOUT4_* constant
	IOMode     uint32 // LAYOUTIOMODE4_READ or LAYOUTIOMODE4_RW
	Changed    bool
	RecallType uint32 // LAYOUTRECALL4_FILE, LAYOUTRECALL4_FSID, or LAYOUTRECALL4_ALL

	// File-specific fields (only if RecallType == LAYOUTRECALL4_FILE)
	FileOffset  uint64
	FileLength  uint64
	FileStateid Stateid4
	FileFH      []byte // opaque filehandle

	// FSID-specific fields (only if RecallType == LAYOUTRECALL4_FSID)
	FsidMajor uint64
	FsidMinor uint64
}

// Encode writes the CB_LAYOUTRECALL args in XDR format.
func (a *CbLayoutRecallArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.LayoutType); err != nil {
		return fmt.Errorf("encode cb_layoutrecall layout_type: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.IOMode); err != nil {
		return fmt.Errorf("encode cb_layoutrecall iomode: %w", err)
	}
	if err := xdr.WriteBool(buf, a.Changed); err != nil {
		return fmt.Errorf("encode cb_layoutrecall changed: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.RecallType); err != nil {
		return fmt.Errorf("encode cb_layoutrecall recall_type: %w", err)
	}
	switch a.RecallType {
	case LAYOUTRECALL4_FILE:
		if err := xdr.WriteUint64(buf, a.FileOffset); err != nil {
			return fmt.Errorf("encode cb_layoutrecall offset: %w", err)
		}
		if err := xdr.WriteUint64(buf, a.FileLength); err != nil {
			return fmt.Errorf("encode cb_layoutrecall length: %w", err)
		}
		EncodeStateid4(buf, &a.FileStateid)
		if err := xdr.WriteXDROpaque(buf, a.FileFH); err != nil {
			return fmt.Errorf("encode cb_layoutrecall fh: %w", err)
		}
	case LAYOUTRECALL4_FSID:
		if err := xdr.WriteUint64(buf, a.FsidMajor); err != nil {
			return fmt.Errorf("encode cb_layoutrecall fsid_major: %w", err)
		}
		if err := xdr.WriteUint64(buf, a.FsidMinor); err != nil {
			return fmt.Errorf("encode cb_layoutrecall fsid_minor: %w", err)
		}
	case LAYOUTRECALL4_ALL:
		// void
	default:
		return fmt.Errorf("unknown cb_layoutrecall recall_type: %d", a.RecallType)
	}
	return nil
}

// Decode reads the CB_LAYOUTRECALL args from XDR format.
func (a *CbLayoutRecallArgs) Decode(r io.Reader) error {
	var err error
	if a.LayoutType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_layoutrecall layout_type: %w", err)
	}
	if a.IOMode, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_layoutrecall iomode: %w", err)
	}
	if a.Changed, err = xdr.DecodeBool(r); err != nil {
		return fmt.Errorf("decode cb_layoutrecall changed: %w", err)
	}
	if a.RecallType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_layoutrecall recall_type: %w", err)
	}
	switch a.RecallType {
	case LAYOUTRECALL4_FILE:
		if a.FileOffset, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode cb_layoutrecall offset: %w", err)
		}
		if a.FileLength, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode cb_layoutrecall length: %w", err)
		}
		sid, err := DecodeStateid4(r)
		if err != nil {
			return fmt.Errorf("decode cb_layoutrecall stateid: %w", err)
		}
		a.FileStateid = *sid
		if a.FileFH, err = xdr.DecodeOpaque(r); err != nil {
			return fmt.Errorf("decode cb_layoutrecall fh: %w", err)
		}
	case LAYOUTRECALL4_FSID:
		if a.FsidMajor, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode cb_layoutrecall fsid_major: %w", err)
		}
		if a.FsidMinor, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode cb_layoutrecall fsid_minor: %w", err)
		}
	case LAYOUTRECALL4_ALL:
		// void
	default:
		return fmt.Errorf("unknown cb_layoutrecall recall_type: %d", a.RecallType)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbLayoutRecallArgs) String() string {
	typeName := "FILE"
	switch a.RecallType {
	case LAYOUTRECALL4_FSID:
		typeName = "FSID"
	case LAYOUTRECALL4_ALL:
		typeName = "ALL"
	}
	return fmt.Sprintf("CbLayoutRecallArgs{layout=%d, iomode=%d, changed=%t, recall=%s}",
		a.LayoutType, a.IOMode, a.Changed, typeName)
}

// ============================================================================
// CB_LAYOUTRECALL4res (RFC 8881 Section 20.3.2)
// ============================================================================

// CbLayoutRecallRes represents CB_LAYOUTRECALL4res per RFC 8881 Section 20.3.
//
//	struct CB_LAYOUTRECALL4res {
//	    nfsstat4 clorr_status;
//	};
type CbLayoutRecallRes struct {
	Status uint32
}

// Encode writes the CB_LAYOUTRECALL result in XDR format.
func (res *CbLayoutRecallRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_layoutrecall status: %w", err)
	}
	return nil
}

// Decode reads the CB_LAYOUTRECALL result from XDR format.
func (res *CbLayoutRecallRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_layoutrecall status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbLayoutRecallRes) String() string {
	return fmt.Sprintf("CbLayoutRecallRes{status=%d}", res.Status)
}
