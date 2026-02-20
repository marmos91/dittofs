// Package types - NFSv4.1 BIND_CONN_TO_SESSION operation types.
//
// BIND_CONN_TO_SESSION (op 41) per RFC 8881 Section 18.34.
// Associates the current TCP connection with a session and channel direction.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// BIND_CONN_TO_SESSION4args - Request
// ============================================================================

// BindConnToSessionArgs represents BIND_CONN_TO_SESSION4args per RFC 8881 Section 18.34.
//
//	struct BIND_CONN_TO_SESSION4args {
//	    sessionid4     bctsa_sessid;
//	    channel_dir_from_client4  bctsa_dir;
//	    bool           bctsa_use_conn_in_rdma_mode;
//	};
type BindConnToSessionArgs struct {
	SessionID       SessionId4
	Dir             uint32 // CDFC4_FORE, CDFC4_BACK, CDFC4_FORE_OR_BOTH, CDFC4_BACK_OR_BOTH
	UseConnInRDMAMode bool
}

// Encode writes the BIND_CONN_TO_SESSION args in XDR format.
func (a *BindConnToSessionArgs) Encode(buf *bytes.Buffer) error {
	if err := a.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.Dir); err != nil {
		return fmt.Errorf("encode dir: %w", err)
	}
	if err := xdr.WriteBool(buf, a.UseConnInRDMAMode); err != nil {
		return fmt.Errorf("encode use_conn_in_rdma_mode: %w", err)
	}
	return nil
}

// Decode reads the BIND_CONN_TO_SESSION args from XDR format.
func (a *BindConnToSessionArgs) Decode(r io.Reader) error {
	if err := a.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode sessionid: %w", err)
	}
	dir, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode dir: %w", err)
	}
	a.Dir = dir
	rdma, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode use_conn_in_rdma_mode: %w", err)
	}
	a.UseConnInRDMAMode = rdma
	return nil
}

// String returns a human-readable representation.
func (a *BindConnToSessionArgs) String() string {
	dirName := channelDirFromClientName(a.Dir)
	return fmt.Sprintf("BIND_CONN_TO_SESSION4args{session=%s, dir=%s, rdma=%t}",
		a.SessionID.String(), dirName, a.UseConnInRDMAMode)
}

// ============================================================================
// BIND_CONN_TO_SESSION4res - Response
// ============================================================================

// BindConnToSessionRes represents BIND_CONN_TO_SESSION4resok + status per RFC 8881 Section 18.34.
//
//	union BIND_CONN_TO_SESSION4res switch (nfsstat4 bctsr_status) {
//	 case NFS4_OK:
//	    sessionid4                bctsr_sessid;
//	    channel_dir_from_server4  bctsr_dir;
//	    bool                      bctsr_use_conn_in_rdma_mode;
//	 default:
//	    void;
//	};
type BindConnToSessionRes struct {
	Status            uint32
	SessionID         SessionId4 // only if NFS4_OK
	Dir               uint32     // only if NFS4_OK: CDFS4_FORE, CDFS4_BACK, CDFS4_BOTH
	UseConnInRDMAMode bool       // only if NFS4_OK
}

// Encode writes the BIND_CONN_TO_SESSION response in XDR format.
func (r *BindConnToSessionRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, r.Status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	if r.Status != NFS4_OK {
		return nil
	}
	if err := r.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode sessionid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.Dir); err != nil {
		return fmt.Errorf("encode dir: %w", err)
	}
	if err := xdr.WriteBool(buf, r.UseConnInRDMAMode); err != nil {
		return fmt.Errorf("encode use_conn_in_rdma_mode: %w", err)
	}
	return nil
}

// Decode reads the BIND_CONN_TO_SESSION response from XDR format.
func (r *BindConnToSessionRes) Decode(rd io.Reader) error {
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
	dir, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode dir: %w", err)
	}
	r.Dir = dir
	rdma, err := xdr.DecodeBool(rd)
	if err != nil {
		return fmt.Errorf("decode use_conn_in_rdma_mode: %w", err)
	}
	r.UseConnInRDMAMode = rdma
	return nil
}

// String returns a human-readable representation.
func (r *BindConnToSessionRes) String() string {
	if r.Status != NFS4_OK {
		return fmt.Sprintf("BIND_CONN_TO_SESSION4res{status=%d}", r.Status)
	}
	dirName := channelDirFromServerName(r.Dir)
	return fmt.Sprintf("BIND_CONN_TO_SESSION4res{OK, session=%s, dir=%s, rdma=%t}",
		r.SessionID.String(), dirName, r.UseConnInRDMAMode)
}

// ============================================================================
// Helper functions for direction enum names
// ============================================================================

func channelDirFromClientName(dir uint32) string {
	switch dir {
	case CDFC4_FORE:
		return "CDFC4_FORE"
	case CDFC4_BACK:
		return "CDFC4_BACK"
	case CDFC4_FORE_OR_BOTH:
		return "CDFC4_FORE_OR_BOTH"
	case CDFC4_BACK_OR_BOTH:
		return "CDFC4_BACK_OR_BOTH"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", dir)
	}
}

func channelDirFromServerName(dir uint32) string {
	switch dir {
	case CDFS4_FORE:
		return "CDFS4_FORE"
	case CDFS4_BACK:
		return "CDFS4_BACK"
	case CDFS4_BOTH:
		return "CDFS4_BOTH"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", dir)
	}
}
