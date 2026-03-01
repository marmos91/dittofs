// Package handlers provides SMB2 command handlers and session management.
//
// This file provides SMB2 CREATE context integration for lease support.
// The actual lease encoding/decoding is in lease.go; this file provides
// the CREATE-specific helpers for processing lease contexts in CREATE requests.
//
// Reference: MS-SMB2 2.2.13.2 SMB2_CREATE_CONTEXT
package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Create Context Tag Constants [MS-SMB2] 2.2.13.2
// ============================================================================

const (
	// LeaseContextTagRequest is the create context name for requesting a lease.
	// "RqLs" - SMB2_CREATE_REQUEST_LEASE or SMB2_CREATE_REQUEST_LEASE_V2
	LeaseContextTagRequest = "RqLs"

	// LeaseContextTagResponse is the create context name for returning granted lease.
	// "RsLs" - SMB2_CREATE_RESPONSE_LEASE or SMB2_CREATE_RESPONSE_LEASE_V2
	LeaseContextTagResponse = "RsLs"

	// Other common create context tags (for reference):
	// "MxAc" - SMB2_CREATE_QUERY_MAXIMAL_ACCESS_REQUEST
	// "QFid" - SMB2_CREATE_QUERY_ON_DISK_ID
	// "TWrp" - SMB2_CREATE_TIMEWARP_TOKEN
	// "DH2Q" - SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
	// "DH2C" - SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
)

// ============================================================================
// Lease Response Context Builder
// ============================================================================

// LeaseResponseContext holds the lease response to include in CREATE response.
type LeaseResponseContext struct {
	LeaseKey       [16]byte
	LeaseState     uint32
	Flags          uint32 // SMB2_LEASE_FLAG_BREAK_IN_PROGRESS if breaking
	ParentLeaseKey [16]byte
	Epoch          uint16
}

// Encode serializes the LeaseResponseContext to wire format.
func (r *LeaseResponseContext) Encode() []byte {
	return EncodeLeaseResponseContext(r.LeaseKey, r.LeaseState, r.Flags, r.Epoch)
}

// ============================================================================
// Create Context Helper Functions
// ============================================================================

// FindCreateContext searches for a create context by name in the request.
// Returns the context data if found, nil if not found.
func FindCreateContext(contexts []CreateContext, name string) *CreateContext {
	for i := range contexts {
		if contexts[i].Name == name {
			return &contexts[i]
		}
	}
	return nil
}

// ProcessLeaseCreateContext processes a lease create context from a CREATE request.
//
// This function:
// 1. Parses the RqLs create context
// 2. Requests the lease through OplockManager
// 3. Returns a LeaseResponseContext to include in the CREATE response
//
// Parameters:
//   - oplockMgr: The oplock manager for requesting leases
//   - ctxData: The raw create context data (RqLs payload)
//   - fileHandle: The file handle for the opened file
//   - sessionID: The SMB session ID
//   - clientID: The connection tracker client ID
//   - shareName: The share name
//   - isDirectory: Whether the target is a directory
//
// Returns:
//   - *LeaseResponseContext: Response context to add to CREATE response (nil if not processing)
//   - error: Parsing or lease request error
func ProcessLeaseCreateContext(
	oplockMgr *OplockManager,
	ctxData []byte,
	fileHandle lock.FileHandle,
	sessionID uint64,
	clientID string,
	shareName string,
	isDirectory bool,
) (*LeaseResponseContext, error) {
	if oplockMgr == nil || oplockMgr.lockStore == nil {
		logger.Debug("ProcessLeaseCreateContext: no oplock manager or lock store")
		return nil, nil
	}

	// Parse the lease create context
	leaseReq, err := DecodeLeaseCreateContext(ctxData)
	if err != nil {
		logger.Debug("ProcessLeaseCreateContext: invalid lease context", "error", err)
		return nil, err
	}

	logger.Debug("ProcessLeaseCreateContext: parsed lease request",
		"leaseKey", leaseReq.LeaseKey,
		"requestedState", lock.LeaseStateToString(leaseReq.LeaseState),
		"isDirectory", isDirectory)

	// Request the lease through OplockManager
	grantedState, epoch, err := oplockMgr.RequestLease(
		context.TODO(), // lease operations are quick
		fileHandle,
		leaseReq.LeaseKey,
		sessionID,
		clientID,
		shareName,
		leaseReq.LeaseState,
		isDirectory,
	)
	if err != nil {
		logger.Debug("ProcessLeaseCreateContext: lease request failed", "error", err)
		grantedState = lock.LeaseStateNone
		epoch = 0
	}

	// Build response context
	var flags uint32
	// Check if break is in progress for this key
	state, _, found := oplockMgr.GetLeaseState(context.TODO(), leaseReq.LeaseKey)
	if found {
		// Check for break in progress by comparing to granted
		if state != grantedState {
			flags = LeaseBreakFlagAckRequired // SMB2_LEASE_FLAG_BREAK_IN_PROGRESS
		}
	}

	return &LeaseResponseContext{
		LeaseKey:       leaseReq.LeaseKey,
		LeaseState:     grantedState,
		Flags:          flags,
		ParentLeaseKey: leaseReq.ParentLeaseKey,
		Epoch:          epoch,
	}, nil
}

// ============================================================================
// Create Context Encoding for Response
// ============================================================================

// EncodeCreateContexts encodes create contexts for a CREATE response.
// Returns the encoded contexts and the offset/length to put in the response header.
//
// Per MS-SMB2 2.2.14, create contexts are appended after the fixed response header.
// Each context has a Next field pointing to the next context (0 for last).
//
// Wire format for each context:
//
//	Offset  Size  Field           Description
//	0       4     Next            Offset to next context (0 if last)
//	4       2     NameOffset      Offset to Name from start of context
//	6       2     NameLength      Length of Name in bytes
//	8       2     Reserved        Reserved (0)
//	10      2     DataOffset      Offset to Data from start of context
//	12      4     DataLength      Length of Data in bytes
//	16      var   Buffer          Name (padded to 8 bytes) + Data
func EncodeCreateContexts(contexts []CreateContext) ([]byte, uint32, uint32) {
	if len(contexts) == 0 {
		return nil, 0, 0
	}

	var result []byte
	for i, ctx := range contexts {
		// Build single context
		ctxBuf := encodeSingleCreateContext(ctx, i < len(contexts)-1)
		result = append(result, ctxBuf...)
	}

	// Offset is after the 89-byte CREATE response header
	// Per MS-SMB2, offset is from the start of the SMB2 header (64 bytes before response)
	offset := uint32(64 + 89) // SMB2 header + CREATE response
	length := uint32(len(result))

	return result, offset, length
}

// encodeSingleCreateContext encodes a single create context.
// hasNext indicates whether this is the last context (affects Next field).
func encodeSingleCreateContext(ctx CreateContext, hasNext bool) []byte {
	// Name is ASCII, padded to 8-byte boundary
	name := []byte(ctx.Name)
	namePadded := padTo8(name)

	// Data follows name
	data := ctx.Data

	// Calculate offsets
	// Header is 16 bytes, name starts at offset 16
	nameOffset := uint16(16)
	dataOffset := uint16(16 + len(namePadded))

	// Total size (before padding)
	totalSize := 16 + len(namePadded) + len(data)

	// Next offset (0 if last, otherwise padded size)
	var nextOffset uint32
	if hasNext {
		nextOffset = uint32(padSizeTo8(totalSize))
	}

	// Build buffer using smbenc Writer
	w := smbenc.NewWriter(totalSize)
	w.WriteUint32(nextOffset)        // Next
	w.WriteUint16(nameOffset)        // NameOffset
	w.WriteUint16(uint16(len(name))) // NameLength
	w.WriteUint16(0)                 // Reserved
	w.WriteUint16(dataOffset)        // DataOffset
	w.WriteUint32(uint32(len(data))) // DataLength
	w.WriteBytes(namePadded)         // Name (padded)
	w.WriteBytes(data)               // Data

	buf := w.Bytes()

	// Pad total context to 8-byte boundary if not last
	if hasNext {
		padded := make([]byte, padSizeTo8(totalSize))
		copy(padded, buf)
		return padded
	}

	return buf
}

// padTo8 pads a byte slice to 8-byte boundary.
func padTo8(b []byte) []byte {
	padded := padSizeTo8(len(b))
	if padded == len(b) {
		return b
	}
	result := make([]byte, padded)
	copy(result, b)
	return result
}

// padSizeTo8 returns the size padded to 8-byte boundary.
func padSizeTo8(size int) int {
	if size%8 == 0 {
		return size
	}
	return size + (8 - size%8)
}

// ============================================================================
// Tests helper: ParseLeaseCreateContext is exported via DecodeLeaseCreateContext
// ============================================================================

// ParseLeaseCreateContext is an alias for DecodeLeaseCreateContext for consistency
// with the plan naming convention.
var ParseLeaseCreateContext = DecodeLeaseCreateContext
