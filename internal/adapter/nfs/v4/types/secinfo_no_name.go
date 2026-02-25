// Package types - SECINFO_NO_NAME operation types (RFC 8881 Section 18.45).
//
// SECINFO_NO_NAME is like SECINFO but operates on the current filehandle
// rather than a named object. The style indicates whether to query the
// current FH (SECINFO_STYLE4_CURRENT_FH=0) or its parent (SECINFO_STYLE4_PARENT=1).
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// SECINFO_NO_NAME Args (RFC 8881 Section 18.45.1)
// ============================================================================

// SecinfoNoNameArgs represents SECINFO_NO_NAME4args per RFC 8881 Section 18.45.
//
//	enum secinfo_style4 {
//	    SECINFO_STYLE4_CURRENT_FH = 0,
//	    SECINFO_STYLE4_PARENT     = 1
//	};
//	typedef secinfo_style4 SECINFO_NO_NAME4args;
type SecinfoNoNameArgs struct {
	Style uint32
}

// Encode writes the SECINFO_NO_NAME args in XDR format.
func (a *SecinfoNoNameArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.Style); err != nil {
		return fmt.Errorf("encode secinfo_no_name style: %w", err)
	}
	return nil
}

// Decode reads the SECINFO_NO_NAME args from XDR format.
func (a *SecinfoNoNameArgs) Decode(r io.Reader) error {
	style, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode secinfo_no_name style: %w", err)
	}
	a.Style = style
	return nil
}

// String returns a human-readable representation.
func (a *SecinfoNoNameArgs) String() string {
	styleName := "CURRENT_FH"
	if a.Style == SECINFO_STYLE4_PARENT {
		styleName = "PARENT"
	}
	return fmt.Sprintf("SecinfoNoNameArgs{style=%s(%d)}", styleName, a.Style)
}

// ============================================================================
// SECINFO_NO_NAME Res (RFC 8881 Section 18.45.2)
// ============================================================================

// SecinfoNoNameRes represents SECINFO_NO_NAME4res per RFC 8881 Section 18.45.
// The response reuses the SECINFO4res structure. For type definition purposes,
// the secinfo entries are stored as raw opaque bytes (the secinfo4 entries are
// complex and already handled by the existing SECINFO handler).
//
//	typedef SECINFO4res SECINFO_NO_NAME4res;
type SecinfoNoNameRes struct {
	Status  uint32
	SecInfo []byte // raw SECINFO4res body (opaque, only if NFS4_OK)
}

// Encode writes the SECINFO_NO_NAME result in XDR format.
// Per RFC 8881, when NFS4_OK the secinfo list must always be present on the
// wire (at minimum a zero-length list) to avoid XDR stream desync.
func (res *SecinfoNoNameRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode secinfo_no_name status: %w", err)
	}
	if res.Status == NFS4_OK {
		if len(res.SecInfo) > 0 {
			if _, err := buf.Write(res.SecInfo); err != nil {
				return fmt.Errorf("encode secinfo_no_name secinfo: %w", err)
			}
		} else {
			// Empty secinfo list: encode array length 0
			if err := xdr.WriteUint32(buf, 0); err != nil {
				return fmt.Errorf("encode secinfo_no_name empty list: %w", err)
			}
		}
	}
	return nil
}

// Decode reads the SECINFO_NO_NAME result from XDR format.
func (res *SecinfoNoNameRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode secinfo_no_name status: %w", err)
	}
	res.Status = status
	if res.Status != NFS4_OK {
		res.SecInfo = nil
		return nil
	}
	// Consume the secinfo list length to keep the XDR stream aligned.
	// The actual secinfo4 entries are complex and handled by the SECINFO handler path.
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode secinfo_no_name list length: %w", err)
	}
	if count == 0 {
		res.SecInfo = nil
		return nil
	}
	// Re-encode the count as raw opaque prefix for SecInfo
	var raw bytes.Buffer
	_ = xdr.WriteUint32(&raw, count)
	res.SecInfo = raw.Bytes()
	return nil
}

// String returns a human-readable representation.
func (res *SecinfoNoNameRes) String() string {
	return fmt.Sprintf("SecinfoNoNameRes{status=%d, secinfo=%d bytes}",
		res.Status, len(res.SecInfo))
}
