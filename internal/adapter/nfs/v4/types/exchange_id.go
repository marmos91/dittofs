// Package types - NFSv4.1 EXCHANGE_ID operation types.
//
// EXCHANGE_ID (op 42) per RFC 8881 Section 18.35.
// Establishes a client ID with the server. This is the first operation
// a v4.1 client sends (before CREATE_SESSION).
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// EXCHANGE_ID4args - Request
// ============================================================================

// ExchangeIdArgs represents EXCHANGE_ID4args per RFC 8881 Section 18.35.
//
//	struct EXCHANGE_ID4args {
//	    client_owner4          eia_clientowner;
//	    uint32_t               eia_flags;
//	    state_protect4_a       eia_state_protect;
//	    nfs_impl_id4           eia_client_impl_id<1>;
//	};
//
// PITFALL: eia_client_impl_id<1> is a variable-length array with max 1 element,
// NOT an XDR optional. Encode as uint32 count + 0 or 1 elements.
type ExchangeIdArgs struct {
	ClientOwner  ClientOwner4
	Flags        uint32         // EXCHGID4_FLAG_* bitmask
	StateProtect StateProtect4A // union switched on SP4_*
	ClientImplId []NfsImplId4   // max 1 element (XDR array <1>)
}

// Encode writes the EXCHANGE_ID args in XDR format.
func (a *ExchangeIdArgs) Encode(buf *bytes.Buffer) error {
	if err := a.ClientOwner.Encode(buf); err != nil {
		return fmt.Errorf("encode client_owner: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.Flags); err != nil {
		return fmt.Errorf("encode flags: %w", err)
	}
	if err := a.StateProtect.Encode(buf); err != nil {
		return fmt.Errorf("encode state_protect: %w", err)
	}
	// eia_client_impl_id<1> - variable-length array, max 1
	count := uint32(len(a.ClientImplId))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode impl_id count: %w", err)
	}
	for i := range a.ClientImplId {
		if err := a.ClientImplId[i].Encode(buf); err != nil {
			return fmt.Errorf("encode impl_id[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the EXCHANGE_ID args from XDR format.
func (a *ExchangeIdArgs) Decode(r io.Reader) error {
	if err := a.ClientOwner.Decode(r); err != nil {
		return fmt.Errorf("decode client_owner: %w", err)
	}
	flags, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode flags: %w", err)
	}
	a.Flags = flags
	if err := a.StateProtect.Decode(r); err != nil {
		return fmt.Errorf("decode state_protect: %w", err)
	}
	// eia_client_impl_id<1>
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode impl_id count: %w", err)
	}
	if count > 1 {
		return fmt.Errorf("impl_id count %d exceeds max 1", count)
	}
	a.ClientImplId = make([]NfsImplId4, count)
	for i := uint32(0); i < count; i++ {
		if err := a.ClientImplId[i].Decode(r); err != nil {
			return fmt.Errorf("decode impl_id[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *ExchangeIdArgs) String() string {
	implCount := len(a.ClientImplId)
	return fmt.Sprintf("EXCHANGE_ID4args{owner=%s, flags=0x%08x, protect=%s, impl_id=%d}",
		a.ClientOwner.String(), a.Flags, a.StateProtect.String(), implCount)
}

// ============================================================================
// EXCHANGE_ID4res - Response
// ============================================================================

// ExchangeIdRes represents EXCHANGE_ID4resok + status per RFC 8881 Section 18.35.
//
//	union EXCHANGE_ID4res switch (nfsstat4 eir_status) {
//	 case NFS4_OK:
//	    clientid4              eir_clientid;
//	    sequenceid4            eir_sequenceid;
//	    uint32_t               eir_flags;
//	    state_protect4_r       eir_state_protect;
//	    server_owner4          eir_server_owner;
//	    opaque                 eir_server_scope<NFS4_OPAQUE_LIMIT>;
//	    nfs_impl_id4           eir_server_impl_id<1>;
//	 default:
//	    void;
//	};
type ExchangeIdRes struct {
	Status       uint32
	ClientID     uint64         // only if NFS4_OK
	SequenceID   uint32         // only if NFS4_OK
	Flags        uint32         // only if NFS4_OK
	StateProtect StateProtect4R // only if NFS4_OK
	ServerOwner  ServerOwner4   // only if NFS4_OK
	ServerScope  []byte         // only if NFS4_OK
	ServerImplId []NfsImplId4   // only if NFS4_OK, max 1
}

// Encode writes the EXCHANGE_ID response in XDR format.
// If Status != NFS4_OK, only the status is encoded.
func (r *ExchangeIdRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, r.Status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	if r.Status != NFS4_OK {
		return nil
	}
	if err := xdr.WriteUint64(buf, r.ClientID); err != nil {
		return fmt.Errorf("encode clientid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.SequenceID); err != nil {
		return fmt.Errorf("encode sequenceid: %w", err)
	}
	if err := xdr.WriteUint32(buf, r.Flags); err != nil {
		return fmt.Errorf("encode flags: %w", err)
	}
	if err := r.StateProtect.Encode(buf); err != nil {
		return fmt.Errorf("encode state_protect: %w", err)
	}
	if err := r.ServerOwner.Encode(buf); err != nil {
		return fmt.Errorf("encode server_owner: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, r.ServerScope); err != nil {
		return fmt.Errorf("encode server_scope: %w", err)
	}
	// eir_server_impl_id<1>
	count := uint32(len(r.ServerImplId))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode server_impl_id count: %w", err)
	}
	for i := range r.ServerImplId {
		if err := r.ServerImplId[i].Encode(buf); err != nil {
			return fmt.Errorf("encode server_impl_id[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the EXCHANGE_ID response from XDR format.
// If Status != NFS4_OK, only the status is decoded.
func (r *ExchangeIdRes) Decode(rd io.Reader) error {
	status, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	r.Status = status
	if r.Status != NFS4_OK {
		return nil
	}
	clientID, err := xdr.DecodeUint64(rd)
	if err != nil {
		return fmt.Errorf("decode clientid: %w", err)
	}
	r.ClientID = clientID
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
	if err := r.StateProtect.Decode(rd); err != nil {
		return fmt.Errorf("decode state_protect: %w", err)
	}
	if err := r.ServerOwner.Decode(rd); err != nil {
		return fmt.Errorf("decode server_owner: %w", err)
	}
	scope, err := xdr.DecodeOpaque(rd)
	if err != nil {
		return fmt.Errorf("decode server_scope: %w", err)
	}
	r.ServerScope = scope
	// eir_server_impl_id<1>
	count, err := xdr.DecodeUint32(rd)
	if err != nil {
		return fmt.Errorf("decode server_impl_id count: %w", err)
	}
	if count > 1 {
		return fmt.Errorf("server_impl_id count %d exceeds max 1", count)
	}
	r.ServerImplId = make([]NfsImplId4, count)
	for i := uint32(0); i < count; i++ {
		if err := r.ServerImplId[i].Decode(rd); err != nil {
			return fmt.Errorf("decode server_impl_id[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (r *ExchangeIdRes) String() string {
	if r.Status != NFS4_OK {
		return fmt.Sprintf("EXCHANGE_ID4res{status=%d}", r.Status)
	}
	return fmt.Sprintf("EXCHANGE_ID4res{OK, clientid=0x%x, seqid=%d, flags=0x%08x, owner=%s, impl=%d}",
		r.ClientID, r.SequenceID, r.Flags, r.ServerOwner.String(), len(r.ServerImplId))
}
