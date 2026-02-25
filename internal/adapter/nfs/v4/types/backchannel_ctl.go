// Package types - NFSv4.1 BACKCHANNEL_CTL operation types.
//
// BACKCHANNEL_CTL (op 40) per RFC 8881 Section 18.33.
// Allows the client to update the backchannel's callback program
// and security parameters without destroying the session.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// BACKCHANNEL_CTL4args - Request
// ============================================================================

// BackchannelCtlArgs represents BACKCHANNEL_CTL4args per RFC 8881 Section 18.33.
//
//	struct BACKCHANNEL_CTL4args {
//	    uint32_t              bca_cb_program;
//	    callback_sec_parms4   bca_sec_parms<>;
//	};
type BackchannelCtlArgs struct {
	CbProgram uint32
	SecParms  []CallbackSecParms4
}

// Encode writes the BACKCHANNEL_CTL args in XDR format.
func (a *BackchannelCtlArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.CbProgram); err != nil {
		return fmt.Errorf("encode cb_program: %w", err)
	}
	// bca_sec_parms<> - variable-length array
	count := uint32(len(a.SecParms))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode sec_parms count: %w", err)
	}
	for i := range a.SecParms {
		if err := a.SecParms[i].Encode(buf); err != nil {
			return fmt.Errorf("encode sec_parms[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the BACKCHANNEL_CTL args from XDR format.
func (a *BackchannelCtlArgs) Decode(r io.Reader) error {
	cbProg, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_program: %w", err)
	}
	a.CbProgram = cbProg
	// bca_sec_parms<>
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode sec_parms count: %w", err)
	}
	if count > 64 {
		return fmt.Errorf("sec_parms count %d exceeds limit", count)
	}
	a.SecParms = make([]CallbackSecParms4, count)
	for i := uint32(0); i < count; i++ {
		if err := a.SecParms[i].Decode(r); err != nil {
			return fmt.Errorf("decode sec_parms[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *BackchannelCtlArgs) String() string {
	return fmt.Sprintf("BACKCHANNEL_CTL4args{cb_program=0x%x, sec_parms=%d}",
		a.CbProgram, len(a.SecParms))
}

// ============================================================================
// BACKCHANNEL_CTL4res - Response
// ============================================================================

// BackchannelCtlRes represents BACKCHANNEL_CTL4res per RFC 8881 Section 18.33.
//
//	struct BACKCHANNEL_CTL4res {
//	    nfsstat4     bcr_status;
//	};
type BackchannelCtlRes struct {
	Status uint32
}

// Encode writes the BACKCHANNEL_CTL response in XDR format.
func (r *BackchannelCtlRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, r.Status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	return nil
}

// Decode reads the BACKCHANNEL_CTL response from XDR format.
func (r *BackchannelCtlRes) Decode(rd io.Reader) error {
	status, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	r.Status = status
	return nil
}

// String returns a human-readable representation.
func (r *BackchannelCtlRes) String() string {
	return fmt.Sprintf("BACKCHANNEL_CTL4res{status=%d}", r.Status)
}
