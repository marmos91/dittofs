// Package types - NFSv4.1 CREATE_SESSION operation types.
//
// CREATE_SESSION (op 43) per RFC 8881 Section 18.36.
// Creates a session bound to a client ID, negotiating channel attributes
// and callback security parameters.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// CREATE_SESSION4args - Request
// ============================================================================

// CreateSessionArgs represents CREATE_SESSION4args per RFC 8881 Section 18.36.
//
//	struct CREATE_SESSION4args {
//	    clientid4              csa_clientid;
//	    sequenceid4            csa_sequence;
//	    uint32_t               csa_flags;
//	    channel_attrs4         csa_fore_chan_attrs;
//	    channel_attrs4         csa_back_chan_attrs;
//	    uint32_t               csa_cb_program;
//	    callback_sec_parms4    csa_sec_parms<>;
//	};
type CreateSessionArgs struct {
	ClientID         uint64
	SequenceID       uint32
	Flags            uint32 // CREATE_SESSION4_FLAG_*
	ForeChannelAttrs ChannelAttrs
	BackChannelAttrs ChannelAttrs
	CbProgram        uint32
	CbSecParms       []CallbackSecParms4
}

// Encode writes the CREATE_SESSION args in XDR format.
func (a *CreateSessionArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, a.ClientID); err != nil {
		return fmt.Errorf("encode clientid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.SequenceID); err != nil {
		return fmt.Errorf("encode sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.Flags); err != nil {
		return fmt.Errorf("encode flags: %w", err)
	}
	if err := a.ForeChannelAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode fore_chan_attrs: %w", err)
	}
	if err := a.BackChannelAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode back_chan_attrs: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.CbProgram); err != nil {
		return fmt.Errorf("encode cb_program: %w", err)
	}
	// csa_sec_parms<> - variable-length array
	count := uint32(len(a.CbSecParms))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode sec_parms count: %w", err)
	}
	for i := range a.CbSecParms {
		if err := a.CbSecParms[i].Encode(buf); err != nil {
			return fmt.Errorf("encode sec_parms[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the CREATE_SESSION args from XDR format.
func (a *CreateSessionArgs) Decode(r io.Reader) error {
	clientID, err := xdr.DecodeUint64(r)
	if err != nil {
		return fmt.Errorf("decode clientid: %w", err)
	}
	a.ClientID = clientID
	seqID, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode sequenceid: %w", err)
	}
	a.SequenceID = seqID
	flags, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode flags: %w", err)
	}
	a.Flags = flags
	if err := a.ForeChannelAttrs.Decode(r); err != nil {
		return fmt.Errorf("decode fore_chan_attrs: %w", err)
	}
	if err := a.BackChannelAttrs.Decode(r); err != nil {
		return fmt.Errorf("decode back_chan_attrs: %w", err)
	}
	cbProg, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_program: %w", err)
	}
	a.CbProgram = cbProg
	// csa_sec_parms<>
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode sec_parms count: %w", err)
	}
	if count > 64 {
		return fmt.Errorf("sec_parms count %d exceeds limit", count)
	}
	a.CbSecParms = make([]CallbackSecParms4, count)
	for i := uint32(0); i < count; i++ {
		if err := a.CbSecParms[i].Decode(r); err != nil {
			return fmt.Errorf("decode sec_parms[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *CreateSessionArgs) String() string {
	return fmt.Sprintf("CREATE_SESSION4args{clientid=0x%x, seqid=%d, flags=0x%08x, cb_prog=%d, sec_parms=%d}",
		a.ClientID, a.SequenceID, a.Flags, a.CbProgram, len(a.CbSecParms))
}

// ============================================================================
// CREATE_SESSION4res - Response
// ============================================================================

// CreateSessionRes represents CREATE_SESSION4resok + status per RFC 8881 Section 18.36.
//
//	union CREATE_SESSION4res switch (nfsstat4 csr_status) {
//	 case NFS4_OK:
//	    sessionid4             csr_sessionid;
//	    sequenceid4            csr_sequence;
//	    uint32_t               csr_flags;
//	    channel_attrs4         csr_fore_chan_attrs;
//	    channel_attrs4         csr_back_chan_attrs;
//	 default:
//	    void;
//	};
type CreateSessionRes struct {
	Status           uint32
	SessionID        SessionId4   // only if NFS4_OK
	SequenceID       uint32       // only if NFS4_OK
	Flags            uint32       // only if NFS4_OK
	ForeChannelAttrs ChannelAttrs // only if NFS4_OK
	BackChannelAttrs ChannelAttrs // only if NFS4_OK
}

// Encode writes the CREATE_SESSION response in XDR format.
func (r *CreateSessionRes) Encode(buf *bytes.Buffer) error {
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
	if err := xdr.WriteUint32(buf, r.Flags); err != nil {
		return fmt.Errorf("encode flags: %w", err)
	}
	if err := r.ForeChannelAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode fore_chan_attrs: %w", err)
	}
	if err := r.BackChannelAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode back_chan_attrs: %w", err)
	}
	return nil
}

// Decode reads the CREATE_SESSION response from XDR format.
func (r *CreateSessionRes) Decode(rd io.Reader) error {
	status, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	r.Status = status
	if r.Status != NFS4_OK {
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
	flags, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode flags: %w", err)
	}
	r.Flags = flags
	if err := r.ForeChannelAttrs.Decode(rd); err != nil {
		return fmt.Errorf("decode fore_chan_attrs: %w", err)
	}
	if err := r.BackChannelAttrs.Decode(rd); err != nil {
		return fmt.Errorf("decode back_chan_attrs: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (r *CreateSessionRes) String() string {
	if r.Status != NFS4_OK {
		return fmt.Sprintf("CREATE_SESSION4res{status=%d}", r.Status)
	}
	return fmt.Sprintf("CREATE_SESSION4res{OK, session=%s, seqid=%d, flags=0x%08x}",
		r.SessionID.String(), r.SequenceID, r.Flags)
}
