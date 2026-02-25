// Package types - NFSv4.1 shared session types.
//
// This file contains XDR types shared across multiple NFSv4.1 operations,
// per RFC 8881. Types used by 2+ operations live here rather than in
// per-operation files.
//
// All types implement XdrEncoder and XdrDecoder interfaces from the xdr package,
// and fmt.Stringer for debug/log readability.
package types

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// SessionId4 - Session Identifier (16 bytes, fixed-size opaque)
// ============================================================================

// SessionId4 is an NFSv4.1 session identifier (opaque, 16 bytes).
// Per RFC 8881 Section 2.10.3, session IDs are fixed-size opaque values.
// Encoded as raw 16 bytes with NO length prefix (fixed-size XDR opaque).
type SessionId4 [NFS4_SESSIONID_SIZE]byte

// Encode writes the session ID as raw 16 bytes (fixed-size XDR opaque).
func (s *SessionId4) Encode(buf *bytes.Buffer) error {
	if _, err := buf.Write(s[:]); err != nil {
		return fmt.Errorf("encode session_id: %w", err)
	}
	return nil
}

// Decode reads 16 bytes from the reader into the session ID.
func (s *SessionId4) Decode(r io.Reader) error {
	if _, err := io.ReadFull(r, s[:]); err != nil {
		return fmt.Errorf("decode session_id: %w", err)
	}
	return nil
}

// String returns the session ID as a hex string.
func (s *SessionId4) String() string {
	return hex.EncodeToString(s[:])
}

// ============================================================================
// Bitmap4 - Variable-length bitmap
// ============================================================================

// Bitmap4 is an NFSv4 attribute bitmap (variable-length uint32 array).
// Per RFC 8881, bitmaps are encoded as XDR variable-length arrays of uint32.
type Bitmap4 []uint32

// Encode writes the bitmap as an XDR variable-length uint32 array.
func (b *Bitmap4) Encode(buf *bytes.Buffer) error {
	count := uint32(len(*b))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode bitmap count: %w", err)
	}
	for i, v := range *b {
		if err := xdr.WriteUint32(buf, v); err != nil {
			return fmt.Errorf("encode bitmap[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the bitmap from an XDR variable-length uint32 array.
func (b *Bitmap4) Decode(r io.Reader) error {
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode bitmap count: %w", err)
	}
	// Reasonable limit for bitmap length
	if count > 256 {
		return fmt.Errorf("bitmap count %d exceeds limit", count)
	}
	*b = make(Bitmap4, count)
	for i := uint32(0); i < count; i++ {
		v, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode bitmap[%d]: %w", i, err)
		}
		(*b)[i] = v
	}
	return nil
}

// String returns the bitmap as a hex representation.
func (b *Bitmap4) String() string {
	if b == nil || len(*b) == 0 {
		return "Bitmap4{}"
	}
	parts := make([]string, len(*b))
	for i, v := range *b {
		parts[i] = fmt.Sprintf("0x%08x", v)
	}
	return fmt.Sprintf("Bitmap4{%s}", fmt.Sprintf("%v", parts))
}

// ============================================================================
// ClientOwner4 - Client Owner
// ============================================================================

// ClientOwner4 represents client_owner4 per RFC 8881 Section 18.35.
// Contains a verifier and an opaque owner ID for client identification.
type ClientOwner4 struct {
	Verifier [8]byte
	OwnerID  []byte
}

// Encode writes the client owner: 8-byte verifier + opaque owner ID.
func (c *ClientOwner4) Encode(buf *bytes.Buffer) error {
	if _, err := buf.Write(c.Verifier[:]); err != nil {
		return fmt.Errorf("encode verifier: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, c.OwnerID); err != nil {
		return fmt.Errorf("encode owner_id: %w", err)
	}
	return nil
}

// Decode reads the client owner: 8-byte verifier + opaque owner ID.
func (c *ClientOwner4) Decode(r io.Reader) error {
	if _, err := io.ReadFull(r, c.Verifier[:]); err != nil {
		return fmt.Errorf("decode verifier: %w", err)
	}
	ownerID, err := xdr.DecodeOpaque(r)
	if err != nil {
		return fmt.Errorf("decode owner_id: %w", err)
	}
	c.OwnerID = ownerID
	return nil
}

// String returns a human-readable representation.
func (c *ClientOwner4) String() string {
	return fmt.Sprintf("ClientOwner4{verifier=%x, owner=%x}", c.Verifier, c.OwnerID)
}

// ============================================================================
// ServerOwner4 - Server Owner
// ============================================================================

// ServerOwner4 represents server_owner4 per RFC 8881 Section 18.35.
// Contains a minor ID and a major ID for server identification.
type ServerOwner4 struct {
	MinorID uint64
	MajorID []byte
}

// Encode writes the server owner: uint64 minor ID + opaque major ID.
func (s *ServerOwner4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, s.MinorID); err != nil {
		return fmt.Errorf("encode minor_id: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, s.MajorID); err != nil {
		return fmt.Errorf("encode major_id: %w", err)
	}
	return nil
}

// Decode reads the server owner: uint64 minor ID + opaque major ID.
func (s *ServerOwner4) Decode(r io.Reader) error {
	minorID, err := xdr.DecodeUint64(r)
	if err != nil {
		return fmt.Errorf("decode minor_id: %w", err)
	}
	s.MinorID = minorID
	majorID, err := xdr.DecodeOpaque(r)
	if err != nil {
		return fmt.Errorf("decode major_id: %w", err)
	}
	s.MajorID = majorID
	return nil
}

// String returns a human-readable representation.
func (s *ServerOwner4) String() string {
	return fmt.Sprintf("ServerOwner4{minor=%d, major=%x}", s.MinorID, s.MajorID)
}

// ============================================================================
// NfsImplId4 - Implementation Identifier
// ============================================================================

// NfsImplId4 represents nfs_impl_id4 per RFC 8881 Section 18.35.
// Identifies the NFS implementation (domain, name, build date).
type NfsImplId4 struct {
	Domain string
	Name   string
	Date   NFS4Time
}

// Encode writes the implementation ID: string domain, string name, nfstime4 date.
func (n *NfsImplId4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDRString(buf, n.Domain); err != nil {
		return fmt.Errorf("encode domain: %w", err)
	}
	if err := xdr.WriteXDRString(buf, n.Name); err != nil {
		return fmt.Errorf("encode name: %w", err)
	}
	if err := xdr.WriteInt64(buf, n.Date.Seconds); err != nil {
		return fmt.Errorf("encode date.seconds: %w", err)
	}
	if err := xdr.WriteUint32(buf, n.Date.Nseconds); err != nil {
		return fmt.Errorf("encode date.nseconds: %w", err)
	}
	return nil
}

// Decode reads the implementation ID from XDR.
func (n *NfsImplId4) Decode(r io.Reader) error {
	domain, err := xdr.DecodeString(r)
	if err != nil {
		return fmt.Errorf("decode domain: %w", err)
	}
	n.Domain = domain
	name, err := xdr.DecodeString(r)
	if err != nil {
		return fmt.Errorf("decode name: %w", err)
	}
	n.Name = name
	seconds, err := xdr.DecodeInt64(r)
	if err != nil {
		return fmt.Errorf("decode date.seconds: %w", err)
	}
	n.Date.Seconds = seconds
	nseconds, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode date.nseconds: %w", err)
	}
	n.Date.Nseconds = nseconds
	return nil
}

// String returns a human-readable representation.
func (n *NfsImplId4) String() string {
	return fmt.Sprintf("NfsImplId4{domain=%q, name=%q, date=%d.%09d}",
		n.Domain, n.Name, n.Date.Seconds, n.Date.Nseconds)
}

// ============================================================================
// ChannelAttrs - Channel Attributes (channel_attrs4)
// ============================================================================

// ChannelAttrs represents channel_attrs4 per RFC 8881 Section 18.36.
// Describes the channel parameters negotiated during CREATE_SESSION.
type ChannelAttrs struct {
	HeaderPadSize         uint32
	MaxRequestSize        uint32
	MaxResponseSize       uint32
	MaxResponseSizeCached uint32
	MaxOperations         uint32
	MaxRequests           uint32
	RdmaIrd               []uint32 // optional, max 1 (ca_rdma_ird<1>)
}

// Encode writes the channel attributes in XDR format.
func (c *ChannelAttrs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, c.HeaderPadSize); err != nil {
		return fmt.Errorf("encode header_pad_size: %w", err)
	}
	if err := xdr.WriteUint32(buf, c.MaxRequestSize); err != nil {
		return fmt.Errorf("encode max_request_size: %w", err)
	}
	if err := xdr.WriteUint32(buf, c.MaxResponseSize); err != nil {
		return fmt.Errorf("encode max_response_size: %w", err)
	}
	if err := xdr.WriteUint32(buf, c.MaxResponseSizeCached); err != nil {
		return fmt.Errorf("encode max_response_size_cached: %w", err)
	}
	if err := xdr.WriteUint32(buf, c.MaxOperations); err != nil {
		return fmt.Errorf("encode max_operations: %w", err)
	}
	if err := xdr.WriteUint32(buf, c.MaxRequests); err != nil {
		return fmt.Errorf("encode max_requests: %w", err)
	}
	// ca_rdma_ird<1> - variable-length array with max 1 element
	count := uint32(len(c.RdmaIrd))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode rdma_ird count: %w", err)
	}
	for i, v := range c.RdmaIrd {
		if err := xdr.WriteUint32(buf, v); err != nil {
			return fmt.Errorf("encode rdma_ird[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the channel attributes from XDR format.
func (c *ChannelAttrs) Decode(r io.Reader) error {
	var err error
	if c.HeaderPadSize, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode header_pad_size: %w", err)
	}
	if c.MaxRequestSize, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode max_request_size: %w", err)
	}
	if c.MaxResponseSize, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode max_response_size: %w", err)
	}
	if c.MaxResponseSizeCached, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode max_response_size_cached: %w", err)
	}
	if c.MaxOperations, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode max_operations: %w", err)
	}
	if c.MaxRequests, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode max_requests: %w", err)
	}
	// ca_rdma_ird<1>
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode rdma_ird count: %w", err)
	}
	if count > 1 {
		return fmt.Errorf("rdma_ird count %d exceeds max 1", count)
	}
	c.RdmaIrd = make([]uint32, count)
	for i := uint32(0); i < count; i++ {
		v, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode rdma_ird[%d]: %w", i, err)
		}
		c.RdmaIrd[i] = v
	}
	return nil
}

// String returns a human-readable representation.
func (c *ChannelAttrs) String() string {
	return fmt.Sprintf("ChannelAttrs{pad=%d, req=%d, res=%d, cached=%d, ops=%d, reqs=%d, rdma=%v}",
		c.HeaderPadSize, c.MaxRequestSize, c.MaxResponseSize,
		c.MaxResponseSizeCached, c.MaxOperations, c.MaxRequests, c.RdmaIrd)
}

// ============================================================================
// StateProtect4A - State Protection (client to server)
// ============================================================================

// StateProtect4A represents state_protect4_a per RFC 8881 Section 18.35.
// Union switched on How (SP4_NONE, SP4_MACH_CRED, SP4_SSV).
type StateProtect4A struct {
	How uint32 // SP4_NONE, SP4_MACH_CRED, or SP4_SSV

	// SP4_MACH_CRED fields
	MachOps    Bitmap4 // operations protected by RPCSEC_GSS machine cred
	EnforceOps Bitmap4 // operations that MUST use machine cred

	// SP4_SSV fields (stored as raw bytes - full SSV is out of scope)
	SsvOps []byte
}

// Encode writes the state protection union in XDR format.
func (s *StateProtect4A) Encode(buf *bytes.Buffer) error {
	if err := xdr.EncodeUnionDiscriminant(buf, s.How); err != nil {
		return fmt.Errorf("encode state_protect how: %w", err)
	}
	switch s.How {
	case SP4_NONE:
		// void
	case SP4_MACH_CRED:
		if err := s.MachOps.Encode(buf); err != nil {
			return fmt.Errorf("encode mach_ops: %w", err)
		}
		if err := s.EnforceOps.Encode(buf); err != nil {
			return fmt.Errorf("encode enforce_ops: %w", err)
		}
	case SP4_SSV:
		if err := xdr.WriteXDROpaque(buf, s.SsvOps); err != nil {
			return fmt.Errorf("encode ssv_ops: %w", err)
		}
	default:
		return fmt.Errorf("unknown state_protect how: %d", s.How)
	}
	return nil
}

// Decode reads the state protection union from XDR format.
func (s *StateProtect4A) Decode(r io.Reader) error {
	how, err := xdr.DecodeUnionDiscriminant(r)
	if err != nil {
		return fmt.Errorf("decode state_protect how: %w", err)
	}
	s.How = how
	switch s.How {
	case SP4_NONE:
		// void
	case SP4_MACH_CRED:
		if err := s.MachOps.Decode(r); err != nil {
			return fmt.Errorf("decode mach_ops: %w", err)
		}
		if err := s.EnforceOps.Decode(r); err != nil {
			return fmt.Errorf("decode enforce_ops: %w", err)
		}
	case SP4_SSV:
		ssvOps, err := xdr.DecodeOpaque(r)
		if err != nil {
			return fmt.Errorf("decode ssv_ops: %w", err)
		}
		s.SsvOps = ssvOps
	default:
		return fmt.Errorf("unknown state_protect how: %d", s.How)
	}
	return nil
}

// String returns a human-readable representation.
func (s *StateProtect4A) String() string {
	switch s.How {
	case SP4_NONE:
		return "StateProtect4A{SP4_NONE}"
	case SP4_MACH_CRED:
		return fmt.Sprintf("StateProtect4A{SP4_MACH_CRED, mach=%s, enforce=%s}",
			s.MachOps.String(), s.EnforceOps.String())
	case SP4_SSV:
		return fmt.Sprintf("StateProtect4A{SP4_SSV, ops=%d bytes}", len(s.SsvOps))
	default:
		return fmt.Sprintf("StateProtect4A{unknown=%d}", s.How)
	}
}

// ============================================================================
// StateProtect4R - State Protection (server to client)
// ============================================================================

// StateProtect4R represents state_protect4_r per RFC 8881 Section 18.35.
// Union switched on How (SP4_NONE, SP4_MACH_CRED, SP4_SSV).
type StateProtect4R struct {
	How uint32 // SP4_NONE, SP4_MACH_CRED, or SP4_SSV

	// SP4_MACH_CRED fields
	MachOps    Bitmap4
	EnforceOps Bitmap4

	// SP4_SSV fields (stored as raw bytes - full SSV is out of scope)
	SsvInfo []byte
}

// Encode writes the state protection result union in XDR format.
func (s *StateProtect4R) Encode(buf *bytes.Buffer) error {
	if err := xdr.EncodeUnionDiscriminant(buf, s.How); err != nil {
		return fmt.Errorf("encode state_protect_r how: %w", err)
	}
	switch s.How {
	case SP4_NONE:
		// void
	case SP4_MACH_CRED:
		if err := s.MachOps.Encode(buf); err != nil {
			return fmt.Errorf("encode mach_ops: %w", err)
		}
		if err := s.EnforceOps.Encode(buf); err != nil {
			return fmt.Errorf("encode enforce_ops: %w", err)
		}
	case SP4_SSV:
		if err := xdr.WriteXDROpaque(buf, s.SsvInfo); err != nil {
			return fmt.Errorf("encode ssv_info: %w", err)
		}
	default:
		return fmt.Errorf("unknown state_protect_r how: %d", s.How)
	}
	return nil
}

// Decode reads the state protection result union from XDR format.
func (s *StateProtect4R) Decode(r io.Reader) error {
	how, err := xdr.DecodeUnionDiscriminant(r)
	if err != nil {
		return fmt.Errorf("decode state_protect_r how: %w", err)
	}
	s.How = how
	switch s.How {
	case SP4_NONE:
		// void
	case SP4_MACH_CRED:
		if err := s.MachOps.Decode(r); err != nil {
			return fmt.Errorf("decode mach_ops: %w", err)
		}
		if err := s.EnforceOps.Decode(r); err != nil {
			return fmt.Errorf("decode enforce_ops: %w", err)
		}
	case SP4_SSV:
		ssvInfo, err := xdr.DecodeOpaque(r)
		if err != nil {
			return fmt.Errorf("decode ssv_info: %w", err)
		}
		s.SsvInfo = ssvInfo
	default:
		return fmt.Errorf("unknown state_protect_r how: %d", s.How)
	}
	return nil
}

// String returns a human-readable representation.
func (s *StateProtect4R) String() string {
	switch s.How {
	case SP4_NONE:
		return "StateProtect4R{SP4_NONE}"
	case SP4_MACH_CRED:
		return fmt.Sprintf("StateProtect4R{SP4_MACH_CRED, mach=%s, enforce=%s}",
			s.MachOps.String(), s.EnforceOps.String())
	case SP4_SSV:
		return fmt.Sprintf("StateProtect4R{SP4_SSV, info=%d bytes}", len(s.SsvInfo))
	default:
		return fmt.Sprintf("StateProtect4R{unknown=%d}", s.How)
	}
}

// ============================================================================
// CallbackSecParms4 - Callback Security Parameters
// ============================================================================

// AuthSysParms represents authsys_parms per RFC 5531 Section 8.2.
// This is the AUTH_SYS credential structure used in callback_sec_parms4.
type AuthSysParms struct {
	Stamp       uint32
	MachineName string
	UID         uint32
	GID         uint32
	GIDs        []uint32
}

// CallbackSecParms4 represents callback_sec_parms4 per RFC 8881 Section 18.35.
// Union switched on CbSecFlavor (AUTH_NONE=0, AUTH_SYS=1, RPCSEC_GSS=6).
type CallbackSecParms4 struct {
	CbSecFlavor uint32

	// AUTH_SYS: authsys_parms struct per RFC 5531 Section 8.2.
	// Encoded as struct fields directly in the XDR stream (NOT opaque-wrapped).
	AuthSysParms *AuthSysParms

	// RPCSEC_GSS: gss_cb_handles4 per RFC 8881 Section 18.35.
	// Stored as raw bytes (defer full parsing to handler phase).
	RpcGssData []byte
}

// Encode writes the callback security parameters in XDR format.
func (c *CallbackSecParms4) Encode(buf *bytes.Buffer) error {
	if err := xdr.EncodeUnionDiscriminant(buf, c.CbSecFlavor); err != nil {
		return fmt.Errorf("encode cb_secflavor: %w", err)
	}
	switch c.CbSecFlavor {
	case 0: // AUTH_NONE - void
		// nothing to encode
	case 1: // AUTH_SYS
		p := c.AuthSysParms
		if p == nil {
			p = &AuthSysParms{}
		}
		if err := binary.Write(buf, binary.BigEndian, p.Stamp); err != nil {
			return fmt.Errorf("encode auth_sys stamp: %w", err)
		}
		// machinename as XDR string (length-prefixed + padded)
		nameBytes := []byte(p.MachineName)
		if err := binary.Write(buf, binary.BigEndian, uint32(len(nameBytes))); err != nil {
			return fmt.Errorf("encode auth_sys machinename len: %w", err)
		}
		if _, err := buf.Write(nameBytes); err != nil {
			return fmt.Errorf("encode auth_sys machinename: %w", err)
		}
		padding := (4 - (len(nameBytes) % 4)) % 4
		for i := 0; i < padding; i++ {
			_ = buf.WriteByte(0)
		}
		if err := binary.Write(buf, binary.BigEndian, p.UID); err != nil {
			return fmt.Errorf("encode auth_sys uid: %w", err)
		}
		if err := binary.Write(buf, binary.BigEndian, p.GID); err != nil {
			return fmt.Errorf("encode auth_sys gid: %w", err)
		}
		if err := binary.Write(buf, binary.BigEndian, uint32(len(p.GIDs))); err != nil {
			return fmt.Errorf("encode auth_sys gids count: %w", err)
		}
		for i, gid := range p.GIDs {
			if err := binary.Write(buf, binary.BigEndian, gid); err != nil {
				return fmt.Errorf("encode auth_sys gid[%d]: %w", i, err)
			}
		}
	case 6: // RPCSEC_GSS
		if err := xdr.WriteXDROpaque(buf, c.RpcGssData); err != nil {
			return fmt.Errorf("encode rpc_gss_data: %w", err)
		}
	default:
		return fmt.Errorf("unknown cb_secflavor: %d", c.CbSecFlavor)
	}
	return nil
}

// Decode reads the callback security parameters from XDR format.
func (c *CallbackSecParms4) Decode(r io.Reader) error {
	flavor, err := xdr.DecodeUnionDiscriminant(r)
	if err != nil {
		return fmt.Errorf("decode cb_secflavor: %w", err)
	}
	c.CbSecFlavor = flavor
	switch c.CbSecFlavor {
	case 0: // AUTH_NONE - void
		// nothing to decode
	case 1: // AUTH_SYS
		// Per RFC 8881 Section 18.35 + RFC 5531 Section 8.2:
		// authsys_parms is encoded as struct fields (NOT opaque-wrapped).
		p := &AuthSysParms{}
		if p.Stamp, err = xdr.DecodeUint32(r); err != nil {
			return fmt.Errorf("decode auth_sys stamp: %w", err)
		}
		if p.MachineName, err = xdr.DecodeString(r); err != nil {
			return fmt.Errorf("decode auth_sys machinename: %w", err)
		}
		if p.UID, err = xdr.DecodeUint32(r); err != nil {
			return fmt.Errorf("decode auth_sys uid: %w", err)
		}
		if p.GID, err = xdr.DecodeUint32(r); err != nil {
			return fmt.Errorf("decode auth_sys gid: %w", err)
		}
		gidsCount, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode auth_sys gids count: %w", err)
		}
		if gidsCount > 16 {
			return fmt.Errorf("auth_sys gids count %d exceeds limit 16", gidsCount)
		}
		p.GIDs = make([]uint32, gidsCount)
		for i := uint32(0); i < gidsCount; i++ {
			if p.GIDs[i], err = xdr.DecodeUint32(r); err != nil {
				return fmt.Errorf("decode auth_sys gid[%d]: %w", i, err)
			}
		}
		c.AuthSysParms = p
	case 6: // RPCSEC_GSS
		data, err := xdr.DecodeOpaque(r)
		if err != nil {
			return fmt.Errorf("decode rpc_gss_data: %w", err)
		}
		c.RpcGssData = data
	default:
		return fmt.Errorf("unknown cb_secflavor: %d", c.CbSecFlavor)
	}
	return nil
}

// String returns a human-readable representation.
func (c *CallbackSecParms4) String() string {
	switch c.CbSecFlavor {
	case 0:
		return "CallbackSecParms4{AUTH_NONE}"
	case 1:
		if c.AuthSysParms != nil {
			return fmt.Sprintf("CallbackSecParms4{AUTH_SYS, uid=%d, gid=%d, machine=%s}",
				c.AuthSysParms.UID, c.AuthSysParms.GID, c.AuthSysParms.MachineName)
		}
		return "CallbackSecParms4{AUTH_SYS}"
	case 6:
		return fmt.Sprintf("CallbackSecParms4{RPCSEC_GSS, %d bytes}", len(c.RpcGssData))
	default:
		return fmt.Sprintf("CallbackSecParms4{flavor=%d}", c.CbSecFlavor)
	}
}

// ============================================================================
// ReferringCall4 - Referring Call (for CB_SEQUENCE)
// ============================================================================

// ReferringCall4 represents referring_call4 per RFC 8881 Section 20.9.
type ReferringCall4 struct {
	SequenceID uint32
	SlotID     uint32
}

// Encode writes the referring call in XDR format.
func (rc *ReferringCall4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, rc.SequenceID); err != nil {
		return fmt.Errorf("encode sequence_id: %w", err)
	}
	if err := xdr.WriteUint32(buf, rc.SlotID); err != nil {
		return fmt.Errorf("encode slot_id: %w", err)
	}
	return nil
}

// Decode reads the referring call from XDR format.
func (rc *ReferringCall4) Decode(r io.Reader) error {
	var err error
	if rc.SequenceID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode sequence_id: %w", err)
	}
	if rc.SlotID, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode slot_id: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (rc *ReferringCall4) String() string {
	return fmt.Sprintf("ReferringCall4{seq=%d, slot=%d}", rc.SequenceID, rc.SlotID)
}

// ============================================================================
// ReferringCallTriple - Referring Call Triple (for CB_SEQUENCE)
// ============================================================================

// ReferringCallTriple represents referring_call_list4 per RFC 8881 Section 20.9.
// Groups a session ID with referring calls from that session.
type ReferringCallTriple struct {
	SessionID      SessionId4
	ReferringCalls []ReferringCall4
}

// Encode writes the referring call triple in XDR format.
func (t *ReferringCallTriple) Encode(buf *bytes.Buffer) error {
	if err := t.SessionID.Encode(buf); err != nil {
		return fmt.Errorf("encode session_id: %w", err)
	}
	count := uint32(len(t.ReferringCalls))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode referring_calls count: %w", err)
	}
	for i := range t.ReferringCalls {
		if err := t.ReferringCalls[i].Encode(buf); err != nil {
			return fmt.Errorf("encode referring_call[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the referring call triple from XDR format.
func (t *ReferringCallTriple) Decode(r io.Reader) error {
	if err := t.SessionID.Decode(r); err != nil {
		return fmt.Errorf("decode session_id: %w", err)
	}
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode referring_calls count: %w", err)
	}
	if count > 1024 {
		return fmt.Errorf("referring_calls count %d exceeds limit", count)
	}
	t.ReferringCalls = make([]ReferringCall4, count)
	for i := uint32(0); i < count; i++ {
		if err := t.ReferringCalls[i].Decode(r); err != nil {
			return fmt.Errorf("decode referring_call[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (t *ReferringCallTriple) String() string {
	return fmt.Sprintf("ReferringCallTriple{session=%s, calls=%d}",
		t.SessionID.String(), len(t.ReferringCalls))
}
