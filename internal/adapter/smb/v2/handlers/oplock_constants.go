// Package handlers provides SMB2 command handlers and session management.
//
// This file contains SMB2 oplock-level constants [MS-SMB2] 2.2.14 and the
// wire-format types for traditional oplock break acknowledgments and responses.
// The OplockManager was deleted; all lease/caching operations are handled by
// LeaseManager (internal/adapter/smb/lease). These constants remain because
// the CREATE response still uses OplockLevel in the fixed header.
package handlers

import (
	"crypto/sha256"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB2 Oplock Level Constants [MS-SMB2] 2.2.14
// ============================================================================

const (
	// OplockLevelNone means no oplock granted.
	OplockLevelNone uint8 = 0x00

	// OplockLevelII is a shared read-caching oplock.
	// Multiple clients can hold Level II oplocks on the same file.
	// The client can cache read data but must not cache writes.
	OplockLevelII uint8 = 0x01

	// OplockLevelExclusive is an exclusive read/write caching oplock.
	// Only one client can hold an exclusive oplock.
	// The client can cache both reads and writes.
	OplockLevelExclusive uint8 = 0x08

	// OplockLevelBatch is like Exclusive but also allows handle caching.
	// The client can delay close operations for better performance.
	OplockLevelBatch uint8 = 0x09

	// OplockLevelLease indicates the request uses SMB2.1+ lease semantics.
	// The lease state is conveyed via the RqLs/RsLs create contexts.
	OplockLevelLease uint8 = 0xFF
)

// ============================================================================
// OPLOCK_BREAK Request/Response Structures [MS-SMB2] 2.2.23, 2.2.24
// ============================================================================

// OplockBreakRequest represents an SMB2 OPLOCK_BREAK acknowledgment [MS-SMB2] 2.2.24.1.
//
// This is sent by the client in response to a server-initiated oplock break notification.
// The client acknowledges the break by specifying the new oplock level.
//
// Wire Format (24 bytes):
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      New oplock level (0x00, 0x01)
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakRequest struct {
	OplockLevel uint8
	FileID      [16]byte
}

// OplockBreakResponse represents an SMB2 OPLOCK_BREAK response [MS-SMB2] 2.2.25.
//
// Wire Format (24 bytes):
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      Acknowledged oplock level
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakResponse struct {
	SMBResponseBase
	OplockLevel uint8
	FileID      [16]byte
}

// OplockBreakNotification is sent by the server to notify a client that
// their oplock is being broken due to a conflicting open by another client.
//
// Wire Format (24 bytes):
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      New oplock level (level to break to)
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakNotification struct {
	OplockLevel uint8
	FileID      [16]byte
}

// DecodeOplockBreakRequest parses an OPLOCK_BREAK acknowledgment.
func DecodeOplockBreakRequest(body []byte) (*OplockBreakRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("OPLOCK_BREAK request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	structSize := r.ReadUint16()
	if structSize != 24 {
		return nil, fmt.Errorf("invalid OPLOCK_BREAK structure size: %d", structSize)
	}

	oplockLevel := r.ReadUint8()
	if oplockLevel != OplockLevelNone && oplockLevel != OplockLevelII {
		return nil, fmt.Errorf("invalid OPLOCK_BREAK acknowledgment level: 0x%02X", oplockLevel)
	}

	r.Skip(5) // Reserved(1) + Reserved2(4)

	req := &OplockBreakRequest{
		OplockLevel: oplockLevel,
	}
	fileIDBytes := r.ReadBytes(16)
	if r.Err() != nil {
		return nil, fmt.Errorf("OPLOCK_BREAK parse error: %w", r.Err())
	}
	copy(req.FileID[:], fileIDBytes)

	return req, nil
}

// Encode serializes the OplockBreakResponse to wire format.
func (resp *OplockBreakResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(24)
	w.WriteUint16(24)              // StructureSize
	w.WriteUint8(resp.OplockLevel) // OplockLevel
	w.WriteUint8(0)                // Reserved
	w.WriteUint32(0)               // Reserved2
	w.WriteBytes(resp.FileID[:])   // FileId

	return w.Bytes(), w.Err()
}

// Encode serializes the OplockBreakNotification to wire format.
func (n *OplockBreakNotification) Encode() ([]byte, error) {
	w := smbenc.NewWriter(24)
	w.WriteUint16(24)           // StructureSize
	w.WriteUint8(n.OplockLevel) // OplockLevel (break to level)
	w.WriteUint8(0)             // Reserved
	w.WriteUint32(0)            // Reserved2
	w.WriteBytes(n.FileID[:])   // FileId

	return w.Bytes(), w.Err()
}

// generateSyntheticLeaseKey creates a deterministic 16-byte lease key from an
// SMB2 FileID. This is used when a client requests traditional oplocks (not
// leases) — we internally map the oplock to a lease, and need a stable key
// so that CLOSE can release it and OPLOCK_BREAK can acknowledge it.
func generateSyntheticLeaseKey(fileID [16]byte) [16]byte {
	// Use SHA-256 of "oplock:" prefix + fileID to avoid collisions with real lease keys.
	h := sha256.New()
	h.Write([]byte("oplock:"))
	h.Write(fileID[:])
	digest := h.Sum(nil)
	var key [16]byte
	copy(key[:], digest[:16])
	return key
}

// leaseStateToOplockLevel maps a granted lease state back to the highest
// traditional oplock level it corresponds to.
func leaseStateToOplockLevel(state uint32) uint8 {
	switch {
	case state&(lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle) ==
		(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle):
		return OplockLevelBatch
	case state&(lock.LeaseStateRead|lock.LeaseStateWrite) ==
		(lock.LeaseStateRead | lock.LeaseStateWrite):
		return OplockLevelExclusive
	case state&lock.LeaseStateRead != 0:
		return OplockLevelII
	default:
		return OplockLevelNone
	}
}

// oplockLevelToLeaseState maps a traditional oplock level to equivalent
// lease state flags for OPLOCK_BREAK acknowledgment handling.
func oplockLevelToLeaseState(level uint8) uint32 {
	switch level {
	case OplockLevelBatch:
		return lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
	case OplockLevelExclusive:
		return lock.LeaseStateRead | lock.LeaseStateWrite
	case OplockLevelII:
		return lock.LeaseStateRead
	default:
		return lock.LeaseStateNone
	}
}

// oplockLevelName returns a human-readable name for an oplock level.
func oplockLevelName(level uint8) string {
	switch level {
	case OplockLevelNone:
		return "None"
	case OplockLevelII:
		return "LevelII"
	case OplockLevelExclusive:
		return "Exclusive"
	case OplockLevelBatch:
		return "Batch"
	case OplockLevelLease:
		return "Lease"
	default:
		return fmt.Sprintf("Unknown(0x%02X)", level)
	}
}
