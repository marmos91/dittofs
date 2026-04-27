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
	"errors"
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
	// Per MS-SMB2 2.2.14.2.10: "The Buffer field of the SMB2_CREATE_CONTEXT
	// structure MUST contain the name 'RqLs'" -- both request and response use
	// the same tag name.
	LeaseContextTagResponse = "RqLs"

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
	IsV1           bool // True when the client sent a V1 (32-byte) lease request
}

// Encode serializes the LeaseResponseContext to wire format.
// Uses V1 encoding (32 bytes) when the original request was V1 (SMB 2.1 clients).
// Uses V2 encoding (52 bytes) for V2 requests (SMB 3.x clients with
// ParentLeaseKey or Epoch).
func (r *LeaseResponseContext) Encode() []byte {
	if r.IsV1 {
		return smbenc.EncodeLeaseV1ResponseContext(r.LeaseKey, r.LeaseState, r.Flags)
	}
	return smbenc.EncodeLeaseV2ResponseContext(
		r.LeaseKey, r.LeaseState, r.Flags, r.ParentLeaseKey, r.HasParent, r.Epoch)
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
	ctx context.Context,
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

	// Parse the lease create context. Track whether the REQUEST is V1
	// (32-byte) or V2 (52-byte). Per smbtorture v2_epoch2 / v2_epoch3 the
	// RESPONSE format follows the lease's STICKY version (set on first
	// grant, immutable thereafter), not the current request's size — so
	// requestIsV1 is the initial guess only; the final responseIsV1 is
	// resolved below from the lease manager's recorded version.
	requestIsV1 := len(ctxData) < LeaseV2ContextSize
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
		ctx,
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
	// Per MS-SMB2 3.3.5.9.8: If the same-key lease is in Breaking state,
	// RequestLease returns ErrLeaseBreakInProgress with the current state/epoch.
	// Set SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02) in the response flags.
	var responseFlags uint32
	if errors.Is(err, lock.ErrLeaseBreakInProgress) {
		responseFlags = smbenc.LeaseResponseFlagBreakInProgress
	} else if errors.Is(err, lock.ErrInvalidLeaseState) || errors.Is(err, lock.ErrLeaseKeyInUse) {
		// Per MS-SMB2 3.3.5.9.8: Invalid lease states (Write without Read,
		// Handle without Read) and a lease key already bound to another file
		// (Samba lease_match) must fail the CREATE with
		// STATUS_INVALID_PARAMETER. Propagate the error to the caller, which
		// short-circuits before any open is granted.
		return nil, err
	} else if err != nil {
		logger.Debug("ProcessLeaseCreateContext: lease request failed", "error", err)
		grantedState = lock.LeaseStateNone
		epoch = 0
	}

	// Record the lease's protocol version on FIRST grant (sticky semantics:
	// once set the version does not change across reopens, even if a later
	// request uses the other version's create-context format). Skipped on
	// denials and ErrLeaseBreakInProgress (no new state). Per smbtorture
	// v2_epoch2 (V2 grant + V1 reopen → V2 response with running epoch) and
	// v2_epoch3 (V1 grant + V2 upgrade → V1 response throughout).
	if err == nil && grantedState != lock.LeaseStateNone {
		leaseMgr.MarkLeaseVersionIfUnset(leaseReq.LeaseKey, !requestIsV1)
	}

	// Resolve the response version from the lease's recorded version. Fall
	// back to the request's format when the lease has no recorded version
	// — that case only happens on denial (no grant occurred to mark) and on
	// ErrLeaseBreakInProgress (referencing a lease already marked at its
	// original grant; the !IsLeaseVersionKnown branch is defensive).
	responseIsV1 := requestIsV1
	if leaseMgr.IsLeaseVersionKnown(leaseReq.LeaseKey) {
		responseIsV1 = !leaseMgr.IsV2(leaseReq.LeaseKey)
	}

	// Per MS-SMB2 3.3.5.9.8: a V2 lease grant is a state change that MUST
	// advance Epoch by 1 over the client's requested value — unconditionally,
	// including a first-grant Epoch=0 (server must respond with Epoch=1).
	// Seed server state to max(current, client+1) so re-opens with the same
	// key pick up the client's evolving view while still advancing past any
	// server-side increments the client hasn't seen yet (e.g. prior breaks).
	//
	// Gate on err == nil: on ErrLeaseBreakInProgress the LockManager returns
	// the breaking lease's current state/epoch read-only and explicitly must
	// not be mutated. Advancing its epoch here would drift the state that
	// the in-flight break ACK will re-persist.
	//
	// When the grant is DENIED (state=None due to byte-range lock or other
	// conflict, but err==nil), there is no state change and therefore no
	// epoch increment per MS-SMB2 §2.2.14.2.11. Echo the client's requested
	// epoch so the response is internally consistent — smbtorture lease-epoch
	// asserts lease_epoch == requested when state == None.
	//
	// Gated on !responseIsV1 (not !requestIsV1): a V1-established lease
	// stays V1 in the wire response even when the client sends a V2 RqLs
	// blob with an epoch field — the epoch is not echoed because the V1
	// response format has no epoch slot.
	if !responseIsV1 && err == nil {
		// None-state probe: client requests state=0 to query the current lease
		// without taking new caching rights (smbtorture upgrade2 / breaking4 /
		// v2_rename use this to re-read after a break or rename). MS-SMB2
		// §2.2.14.2.11 requires the epoch to advance only on a granted state
		// CHANGE; a None-probe never changes state, so we MUST echo the
		// lease's current epoch verbatim — neither advancing past it nor
		// taking the client's requested value (which is itself the current
		// epoch and would drift the server one ahead per round-trip).
		//
		// Without this guard, every same-key None-probe SetLeaseEpoch's
		// to leaseReq.Epoch+1 and accumulates: smbtorture v2_rename re-opens
		// fname_dst (None-probe) → epoch drifts 0x4712→0x4713; later re-open
		// of fname after rename-back → drifts again, lease.c:4299 fails
		// 0x4714 ≠ expected 0x4713.
		switch {
		case leaseReq.LeaseState == lock.LeaseStateNone:
			// epoch already holds the lease's current value from RequestLease.
		case grantedState != lock.LeaseStateNone:
			nextEpoch := leaseReq.Epoch + 1
			if nextEpoch > epoch {
				leaseMgr.SetLeaseEpoch(leaseReq.LeaseKey, nextEpoch)
				epoch = nextEpoch
			}
		default:
			epoch = leaseReq.Epoch
		}
	}

	// Build response context.
	// Per MS-SMB2 2.2.14.2.10: Flags MUST be 0 for fresh grants.
	// SMB2_LEASE_FLAG_BREAK_IN_PROGRESS (0x02) is only set when a break is
	// actively in progress on a same-key lease.
	//
	// Per MS-SMB2 §2.2.13.2.10 / §2.2.14.2.11 the parent-lease-key linkage
	// is signaled by SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET (0x4) in the
	// request Flags field — NOT by inspecting the key contents. A request
	// with Flags=0 and a non-zero ParentLeaseKey carries no parent
	// linkage; the response MUST clear the flag bit and the parent key
	// (smbtorture v2_flags_parentkey: ls.lease_flags = 0 with LEASE1 in
	// ParentLeaseKey expects parent_lease_key=zeros in the response).
	hasParent := leaseReq.Flags&smbenc.LeaseResponseFlagParentKeySet != 0
	var parentKey [16]byte
	if hasParent {
		parentKey = leaseReq.ParentLeaseKey
	}
	return &LeaseResponseContext{
		LeaseKey:       leaseReq.LeaseKey,
		LeaseState:     grantedState,
		Flags:          responseFlags,
		ParentLeaseKey: parentKey,
		HasParent:      hasParent,
		Epoch:          epoch,
		IsV1:           responseIsV1,
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

	// Offset is after the 88-byte CREATE response fixed fields
	// Per MS-SMB2, offset is from the start of the SMB2 header (64 bytes before response)
	offset := uint32(64 + 88) // SMB2 header + CREATE response fixed fields
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
