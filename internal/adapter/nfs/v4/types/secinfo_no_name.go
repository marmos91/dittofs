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

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
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
