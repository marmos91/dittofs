package types

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// COMPOUND RPC Structures
// ============================================================================

// RawOp represents a single operation within a COMPOUND request.
// The operation arguments are decoded lazily by each handler.
type RawOp struct {
	// OpCode identifies the operation (e.g., OP_LOOKUP, OP_GETATTR).
	OpCode uint32

	// Data contains the raw XDR-encoded arguments for this operation.
	// Each handler decodes its own arguments from this slice.
	Data []byte
}

// Compound4Args represents the decoded COMPOUND4args XDR structure.
//
// Per RFC 7530 Section 16.2 / RFC 7531:
//
//	struct COMPOUND4args {
//	    utf8str_cs  tag;
//	    uint32_t    minorversion;
//	    nfs_argop4  argarray<>;
//	};
type Compound4Args struct {
	// Tag is an opaque value for client-side correlation.
	// Must be echoed back byte-for-byte in the response.
	Tag []byte

	// MinorVersion is the NFSv4 minor version (must be 0 for NFSv4.0).
	MinorVersion uint32

	// Ops contains the sequence of operations to execute.
	Ops []RawOp
}

// CompoundResult holds the result of a single operation within COMPOUND.
type CompoundResult struct {
	// Status is the NFS4 status code for this operation.
	Status uint32

	// OpCode identifies which operation this result corresponds to.
	OpCode uint32

	// Data contains the XDR-encoded operation-specific result.
	Data []byte
}

// Compound4Response represents the COMPOUND4res XDR structure.
//
// Per RFC 7530 Section 16.2 / RFC 7531:
//
//	struct COMPOUND4res {
//	    nfsstat4    status;
//	    utf8str_cs  tag;
//	    nfs_resop4  resarray<>;
//	};
type Compound4Response struct {
	// Status is the status of the last evaluated operation.
	Status uint32

	// Tag is echoed from the request (byte-for-byte).
	Tag []byte

	// Results contains the results for each evaluated operation,
	// up to and including the first failed operation.
	Results []CompoundResult
}

// ============================================================================
// Compound Context
// ============================================================================

// CompoundContext is the mutable state passed by pointer through all
// operations within a single COMPOUND call. It tracks the current and
// saved filehandles, authentication information, and Go context.
type CompoundContext struct {
	// CurrentFH is the current filehandle. Nil means no filehandle is set.
	// Set by PUTFH, PUTROOTFH, PUTPUBFH, LOOKUP, CREATE, etc.
	CurrentFH []byte

	// SavedFH is the saved filehandle for SAVEFH/RESTOREFH.
	// Nil means no saved filehandle.
	SavedFH []byte

	// ClientAddr is the remote address of the client connection.
	ClientAddr string

	// AuthFlavor is the RPC authentication flavor (AUTH_NULL, AUTH_UNIX, etc.).
	AuthFlavor uint32

	// UID is the effective user ID from AUTH_UNIX credentials.
	// Nil if no AUTH_UNIX credentials were provided.
	UID *uint32

	// GID is the effective group ID from AUTH_UNIX credentials.
	// Nil if no AUTH_UNIX credentials were provided.
	GID *uint32

	// GIDs contains supplementary group IDs from AUTH_UNIX credentials.
	GIDs []uint32

	// Context is the Go context for cancellation and timeout control.
	Context context.Context

	// ClientState holds minimal NFSv4 connection state.
	// This is a placeholder for Phase 9 (State Management).
	ClientState *V4ClientState

	// SkipOwnerSeqid is set to true by the v4.1 dispatch path after
	// SEQUENCE succeeds. When true, v4.0 handlers called from v4.1
	// compounds pass seqid=0 to StateManager, bypassing per-owner seqid
	// validation. The slot table provides replay protection for v4.1,
	// making per-owner seqid redundant.
	SkipOwnerSeqid bool
}

// V4ClientState holds NFSv4 client state associated with a connection.
// Extended in Phase 9 to carry the client ID for state lookups.
type V4ClientState struct {
	// ClientAddr is the address of the connected client.
	ClientAddr string

	// ClientID is the server-assigned client ID from SETCLIENTID_CONFIRM.
	// Zero means no confirmed client ID is associated with this connection.
	ClientID uint64
}

// ============================================================================
// Auxiliary Types
// ============================================================================

// FSID4 represents an NFSv4 filesystem identifier (fsid4).
// Each filesystem must have a unique FSID to allow clients to detect
// filesystem boundary crossings.
type FSID4 struct {
	Major uint64
	Minor uint64
}

// NFS4Time represents an NFSv4 time value (nfstime4).
//
// Per RFC 7530:
//
//	struct nfstime4 {
//	    int64_t   seconds;
//	    uint32_t  nseconds;
//	};
type NFS4Time struct {
	Seconds  int64
	Nseconds uint32
}

// ============================================================================
// Stateid4 (State Identifier)
// ============================================================================

// NFS4_OTHER_SIZE is the size of the "other" field in stateid4 (12 bytes).
const NFS4_OTHER_SIZE = 12

// Stateid4 represents an NFSv4 state identifier (stateid4).
// Per RFC 7530 Section 9.1.4:
//
//	struct stateid4 {
//	    uint32_t seqid;
//	    opaque   other[NFS4_OTHER_SIZE];
//	}
type Stateid4 struct {
	Seqid uint32
	Other [NFS4_OTHER_SIZE]byte
}

// IsSpecialStateid returns true if the stateid is a special stateid.
// Special stateids per RFC 7530 Section 9.1.4.3:
//   - Anonymous: seqid=0, other=all-zeros (standard access check, no lock state)
//   - READ bypass: seqid=0xFFFFFFFF, other=all-ones (bypass locks for read only)
func (s *Stateid4) IsSpecialStateid() bool {
	return s.isAnonymous() || s.isReadBypass()
}

// isAnonymous returns true if the stateid is the anonymous stateid
// (seqid=0, other=all-zeros).
func (s *Stateid4) isAnonymous() bool {
	if s.Seqid != 0 {
		return false
	}
	for _, b := range s.Other {
		if b != 0 {
			return false
		}
	}
	return true
}

// isReadBypass returns true if the stateid is the READ bypass stateid
// (seqid=0xFFFFFFFF, other=all-ones).
func (s *Stateid4) isReadBypass() bool {
	if s.Seqid != 0xFFFFFFFF {
		return false
	}
	for _, b := range s.Other {
		if b != 0xFF {
			return false
		}
	}
	return true
}

// DecodeStateid4 reads a stateid4 from an io.Reader.
func DecodeStateid4(reader io.Reader) (*Stateid4, error) {
	seqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, err
	}
	var other [NFS4_OTHER_SIZE]byte
	if _, err := io.ReadFull(reader, other[:]); err != nil {
		return nil, err
	}
	return &Stateid4{Seqid: seqid, Other: other}, nil
}

// EncodeStateid4 writes a stateid4 to a buffer.
func EncodeStateid4(buf *bytes.Buffer, sid *Stateid4) {
	_ = xdr.WriteUint32(buf, sid.Seqid)
	buf.Write(sid.Other[:])
}

// ============================================================================
// Filehandle Helpers
// ============================================================================

// RequireCurrentFH checks that the CompoundContext has a current filehandle.
// Returns NFS4_OK if CurrentFH is set, NFS4ERR_NOFILEHANDLE otherwise.
//
// This should be called at the start of every operation that operates on
// the current filehandle (GETATTR, LOOKUP, READ, WRITE, etc.).
func RequireCurrentFH(ctx *CompoundContext) uint32 {
	if ctx.CurrentFH == nil {
		return NFS4ERR_NOFILEHANDLE
	}
	return NFS4_OK
}

// RequireSavedFH checks that the CompoundContext has a saved filehandle.
// Returns NFS4_OK if SavedFH is set, NFS4ERR_RESTOREFH otherwise.
//
// This should be called by RESTOREFH before restoring the saved handle.
func RequireSavedFH(ctx *CompoundContext) uint32 {
	if ctx.SavedFH == nil {
		return NFS4ERR_RESTOREFH
	}
	return NFS4_OK
}

// ============================================================================
// NFSv4.1 Request Context
// ============================================================================

// V41RequestContext holds session context for NFSv4.1 operations.
// Populated by SEQUENCE processing and passed to subsequent operations
// within a COMPOUND request.
type V41RequestContext struct {
	// SessionID is the NFSv4.1 session identifier.
	SessionID SessionId4

	// SlotID identifies the slot within the session.
	SlotID uint32

	// SequenceID is the sequence number for this slot.
	SequenceID uint32

	// HighestSlot is the highest slot ID the client intends to use.
	HighestSlot uint32

	// CacheThis indicates whether the server should cache the reply.
	CacheThis bool
}

// String returns a human-readable representation of the V41RequestContext.
func (c *V41RequestContext) String() string {
	return fmt.Sprintf("V41Ctx{session=%s, slot=%d, seq=%d, highest=%d, cache=%t}",
		hex.EncodeToString(c.SessionID[:]), c.SlotID, c.SequenceID, c.HighestSlot, c.CacheThis)
}
