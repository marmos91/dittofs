// Package handlers provides SMB2 command handlers and session management.
//
// This file provides SMB2 lease wire-format types, encoding/decoding, and
// CREATE context integration for lease support. It contains the lease constants,
// request/response structures, break notification types, and the CREATE-specific
// helpers for processing lease contexts in CREATE requests.
//
// Reference: MS-SMB2 2.2.13.2 SMB2_CREATE_CONTEXT
// Reference: MS-SMB2 2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE_V2
// Reference: MS-SMB2 2.2.23.2, 2.2.24.2 Lease Break Notification/Acknowledgment
package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB2 Lease Constants [MS-SMB2] 2.2.13.2.8
// ============================================================================

const (
	// LeaseV1ContextSize is the size of the SMB2_CREATE_REQUEST_LEASE context
	LeaseV1ContextSize = 32

	// LeaseV2ContextSize is the size of the SMB2_CREATE_REQUEST_LEASE_V2 context
	LeaseV2ContextSize = 52

	// LeaseBreakNotificationSize is the size of a lease break notification [MS-SMB2] 2.2.23.2
	LeaseBreakNotificationSize = 44

	// LeaseBreakAckSize is the size of a lease break acknowledgment [MS-SMB2] 2.2.24.2
	LeaseBreakAckSize = 36
)

// Lease break notification flags
const (
	// LeaseBreakFlagAckRequired indicates the client must acknowledge the break
	LeaseBreakFlagAckRequired uint32 = 0x01
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
// Lease Request/Response Types [MS-SMB2] 2.2.13.2.8
// ============================================================================

// LeaseCreateContext represents an SMB2_CREATE_REQUEST_LEASE_V2 context.
//
// **Wire Format (52 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       16    LeaseKey         Client-generated 128-bit key
//	16      4     LeaseState       Requested R/W/H state
//	20      4     Flags            Reserved (0)
//	24      8     LeaseDuration    Reserved (0)
//	32      16    ParentLeaseKey   Parent directory lease key (SMB3)
//	48      2     Epoch            State change counter
//	50      2     Reserved         Reserved (0)
type LeaseCreateContext struct {
	LeaseKey       [16]byte
	LeaseState     uint32
	Flags          uint32
	LeaseDuration  uint64
	ParentLeaseKey [16]byte
	Epoch          uint16
}

// DecodeLeaseCreateContext parses an SMB2_CREATE_REQUEST_LEASE_V2 context.
func DecodeLeaseCreateContext(data []byte) (*LeaseCreateContext, error) {
	if len(data) < LeaseV2ContextSize {
		if len(data) >= LeaseV1ContextSize {
			// V1 format (32 bytes) - no parent key or epoch
			return decodeLeaseV1Context(data)
		}
		return nil, fmt.Errorf("lease context too short: %d bytes", len(data))
	}

	r := smbenc.NewReader(data)
	leaseKey := r.ReadBytes(16)
	leaseState := r.ReadUint32()
	flags := r.ReadUint32()
	leaseDuration := r.ReadUint64()
	parentLeaseKey := r.ReadBytes(16)
	epoch := r.ReadUint16()
	if r.Err() != nil {
		return nil, fmt.Errorf("failed to parse lease V2 context: %w", r.Err())
	}

	ctx := &LeaseCreateContext{
		LeaseState:    leaseState,
		Flags:         flags,
		LeaseDuration: leaseDuration,
		Epoch:         epoch,
	}
	copy(ctx.LeaseKey[:], leaseKey)
	copy(ctx.ParentLeaseKey[:], parentLeaseKey)

	return ctx, nil
}

// decodeLeaseV1Context parses an SMB2_CREATE_REQUEST_LEASE (V1) context.
func decodeLeaseV1Context(data []byte) (*LeaseCreateContext, error) {
	r := smbenc.NewReader(data)
	leaseKey := r.ReadBytes(16)
	leaseState := r.ReadUint32()
	flags := r.ReadUint32()
	leaseDuration := r.ReadUint64()
	if r.Err() != nil {
		return nil, fmt.Errorf("failed to parse lease V1 context: %w", r.Err())
	}

	ctx := &LeaseCreateContext{
		LeaseState:    leaseState,
		Flags:         flags,
		LeaseDuration: leaseDuration,
		Epoch:         0, // V1 has no epoch
	}
	copy(ctx.LeaseKey[:], leaseKey)
	// V1 has no parent lease key

	return ctx, nil
}

// EncodeLeaseResponseContext encodes an SMB2_CREATE_RESPONSE_LEASE_V2 context.
func EncodeLeaseResponseContext(leaseKey [16]byte, leaseState uint32, flags uint32, epoch uint16) []byte {
	w := smbenc.NewWriter(LeaseV2ContextSize)
	w.WriteBytes(leaseKey[:]) // LeaseKey (16 bytes)
	w.WriteUint32(leaseState) // LeaseState
	w.WriteUint32(flags)      // Flags
	w.WriteUint64(0)          // LeaseDuration
	w.WriteZeros(16)          // ParentLeaseKey (16 bytes)
	w.WriteUint16(epoch)      // Epoch
	w.WriteUint16(0)          // Reserved
	return w.Bytes()
}

// ParseLeaseCreateContext is an alias for DecodeLeaseCreateContext for consistency
// with the plan naming convention.
var ParseLeaseCreateContext = DecodeLeaseCreateContext

// ============================================================================
// Lease Break Notification [MS-SMB2] 2.2.23.2
// ============================================================================

// LeaseBreakNotification represents an SMB2 Lease Break Notification.
//
// **Wire Format (44 bytes):**
//
//	Offset  Size  Field              Description
//	------  ----  -----------------  ----------------------------------
//	0       2     StructureSize      Always 44
//	2       2     NewEpoch           New epoch value
//	4       4     Flags              ACK_REQUIRED flag
//	8       16    LeaseKey           Lease identifier
//	24      4     CurrentLeaseState  What client currently has
//	28      4     NewLeaseState      What client should break to
//	32      12    Reserved           Reserved (0)
type LeaseBreakNotification struct {
	NewEpoch          uint16
	Flags             uint32
	LeaseKey          [16]byte
	CurrentLeaseState uint32
	NewLeaseState     uint32
}

// Encode serializes the LeaseBreakNotification to wire format.
func (n *LeaseBreakNotification) Encode() []byte {
	w := smbenc.NewWriter(LeaseBreakNotificationSize)
	w.WriteUint16(LeaseBreakNotificationSize) // StructureSize
	w.WriteUint16(n.NewEpoch)                 // NewEpoch
	w.WriteUint32(n.Flags)                    // Flags
	w.WriteBytes(n.LeaseKey[:])               // LeaseKey (16 bytes)
	w.WriteUint32(n.CurrentLeaseState)        // CurrentLeaseState
	w.WriteUint32(n.NewLeaseState)            // NewLeaseState
	w.WriteZeros(12)                          // Reserved (12 bytes)
	return w.Bytes()
}

// ============================================================================
// Lease Break Acknowledgment [MS-SMB2] 2.2.24.2
// ============================================================================

// LeaseBreakAcknowledgment represents an SMB2 Lease Break Acknowledgment.
//
// **Wire Format (36 bytes):**
//
//	Offset  Size  Field          Description
//	------  ----  -------------  ----------------------------------
//	0       2     StructureSize  Always 36
//	2       2     Reserved       Reserved (0)
//	4       4     Flags          Reserved (0)
//	8       16    LeaseKey       Lease identifier
//	24      4     LeaseState     State client is acknowledging
//	28      8     Reserved       Reserved (0)
type LeaseBreakAcknowledgment struct {
	LeaseKey   [16]byte
	LeaseState uint32
}

// DecodeLeaseBreakAcknowledgment parses an SMB2 Lease Break Acknowledgment.
func DecodeLeaseBreakAcknowledgment(data []byte) (*LeaseBreakAcknowledgment, error) {
	if len(data) < LeaseBreakAckSize {
		return nil, fmt.Errorf("lease break ack too short: %d bytes", len(data))
	}

	r := smbenc.NewReader(data)
	structSize := r.ReadUint16()
	if structSize != LeaseBreakAckSize {
		return nil, fmt.Errorf("invalid lease break ack structure size: %d", structSize)
	}

	r.Skip(6) // Reserved(2) + Flags(4)
	leaseKey := r.ReadBytes(16)
	leaseState := r.ReadUint32()
	if r.Err() != nil {
		return nil, fmt.Errorf("failed to parse lease break ack: %w", r.Err())
	}

	ack := &LeaseBreakAcknowledgment{
		LeaseState: leaseState,
	}
	copy(ack.LeaseKey[:], leaseKey)

	return ack, nil
}

// EncodeLeaseBreakResponse encodes an SMB2 Lease Break Response.
func EncodeLeaseBreakResponse(leaseKey [16]byte, leaseState uint32) []byte {
	w := smbenc.NewWriter(LeaseBreakAckSize)
	w.WriteUint16(LeaseBreakAckSize) // StructureSize
	w.WriteUint16(0)                 // Reserved
	w.WriteUint32(0)                 // Flags
	w.WriteBytes(leaseKey[:])        // LeaseKey (16 bytes)
	w.WriteUint32(leaseState)        // LeaseState
	w.WriteZeros(8)                  // Reserved (8 bytes)
	return w.Bytes()
}

// ============================================================================
// Lease Response Context Builder
// ============================================================================

// LeaseResponseContext holds the lease response to include in CREATE response.
type LeaseResponseContext struct {
	LeaseKey       [16]byte
	LeaseState     uint32
	Flags          uint32 // SMB2_LEASE_FLAG_BREAK_IN_PROGRESS if breaking
	ParentLeaseKey [16]byte
	HasParent      bool // True if ParentLeaseKey is valid (V2)
	Epoch          uint16
}

// Encode serializes the LeaseResponseContext to wire format.
// Uses V2 encoding (52 bytes) when HasParent is true or Epoch > 0,
// otherwise falls back to V1 encoding (32 bytes) for backward compatibility.
func (r *LeaseResponseContext) Encode() []byte {
	if r.HasParent || r.Epoch > 0 {
		return smbenc.EncodeLeaseV2ResponseContext(
			r.LeaseKey, r.LeaseState, r.Flags, r.ParentLeaseKey, r.HasParent, r.Epoch)
	}
	return smbenc.EncodeLeaseV1ResponseContext(r.LeaseKey, r.LeaseState, r.Flags)
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
// 2. Requests the lease through LeaseManager (which delegates to shared LockManager)
// 3. Returns a LeaseResponseContext to include in the CREATE response
//
// Parameters:
//   - leaseMgr: The lease manager for requesting leases
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
	leaseMgr *lease.LeaseManager,
	ctxData []byte,
	fileHandle lock.FileHandle,
	sessionID uint64,
	clientID string,
	shareName string,
	isDirectory bool,
) (*LeaseResponseContext, error) {
	if leaseMgr == nil {
		logger.Debug("ProcessLeaseCreateContext: no lease manager")
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

	// Build owner ID for cross-protocol visibility
	ownerID := fmt.Sprintf("smb:lease:%x", leaseReq.LeaseKey)

	// Request the lease through LeaseManager (delegates to shared LockManager)
	grantedState, epoch, err := leaseMgr.RequestLease(
		context.TODO(), // lease operations are quick
		fileHandle,
		leaseReq.LeaseKey,
		leaseReq.ParentLeaseKey,
		sessionID,
		ownerID,
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
	state, _, found := leaseMgr.GetLeaseState(context.TODO(), leaseReq.LeaseKey)
	if found {
		// Check for break in progress by comparing to granted
		if state != grantedState {
			flags = LeaseBreakFlagAckRequired // SMB2_LEASE_FLAG_BREAK_IN_PROGRESS
		}
	}

	// Determine if parent lease key is valid (non-zero)
	hasParent := leaseReq.ParentLeaseKey != [16]byte{}

	return &LeaseResponseContext{
		LeaseKey:       leaseReq.LeaseKey,
		LeaseState:     grantedState,
		Flags:          flags,
		ParentLeaseKey: leaseReq.ParentLeaseKey,
		HasParent:      hasParent,
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
