package smbenc

// ============================================================================
// Lease Constants
// ============================================================================

const (
	// LeaseV1ContextSize is the size of the SMB2_CREATE_RESPONSE_LEASE context (32 bytes).
	LeaseV1ContextSize = 32

	// LeaseV2ContextSize is the size of the SMB2_CREATE_RESPONSE_LEASE_V2 context (52 bytes).
	LeaseV2ContextSize = 52

	// LeaseBreakNotificationSize is the size of a lease break notification [MS-SMB2] 2.2.23.2.
	LeaseBreakNotificationSize = 44

	// LeaseBreakAckSize is the size of a lease break acknowledgment [MS-SMB2] 2.2.24.2.
	LeaseBreakAckSize = 36
)

// Lease response flags [MS-SMB2] 2.2.14.2.10
const (
	// LeaseResponseFlagBreakInProgress indicates a break is in progress.
	LeaseResponseFlagBreakInProgress uint32 = 0x02

	// LeaseResponseFlagParentKeySet indicates ParentLeaseKey is valid.
	// SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET (0x04).
	LeaseResponseFlagParentKeySet uint32 = 0x04
)

// Lease break notification flags [MS-SMB2] 2.2.23.2
const (
	// LeaseBreakFlagAckRequired indicates the client must acknowledge the break.
	LeaseBreakFlagAckRequired uint32 = 0x01
)

// ============================================================================
// Lease V1 Response Context Encoding
// ============================================================================

// EncodeLeaseV1ResponseContext encodes an SMB2_CREATE_RESPONSE_LEASE context (32 bytes).
//
// Wire format:
//
//	Offset  Size  Field            Description
//	0       16    LeaseKey         128-bit lease identifier
//	16      4     LeaseState       Granted R/W/H state
//	20      4     Flags            Response flags
//	24      8     LeaseDuration    Reserved (0)
func EncodeLeaseV1ResponseContext(leaseKey [16]byte, leaseState uint32, flags uint32) []byte {
	w := NewWriter(LeaseV1ContextSize)
	w.WriteBytes(leaseKey[:]) // LeaseKey (16 bytes)
	w.WriteUint32(leaseState) // LeaseState
	w.WriteUint32(flags)      // Flags
	w.WriteUint64(0)          // LeaseDuration
	return w.Bytes()
}

// ============================================================================
// Lease V2 Response Context Encoding
// ============================================================================

// EncodeLeaseV2ResponseContext encodes an SMB2_CREATE_RESPONSE_LEASE_V2 context (52 bytes).
//
// Per MS-SMB2 2.2.14.2.10, the V2 response includes ParentLeaseKey and epoch.
// When hasParent is true, SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET (0x04) is set
// in the Flags field.
//
// Wire format:
//
//	Offset  Size  Field            Description
//	0       16    LeaseKey         128-bit lease identifier
//	16      4     LeaseState       Granted R/W/H state
//	20      4     Flags            SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET if parent set
//	24      8     LeaseDuration    Reserved (0)
//	32      16    ParentLeaseKey   Parent directory lease key
//	48      2     Epoch            State change counter
//	50      2     Reserved         Reserved (0)
func EncodeLeaseV2ResponseContext(
	leaseKey [16]byte,
	leaseState uint32,
	flags uint32,
	parentLeaseKey [16]byte,
	hasParent bool,
	epoch uint16,
) []byte {
	if hasParent {
		flags |= LeaseResponseFlagParentKeySet
	}

	w := NewWriter(LeaseV2ContextSize)
	w.WriteBytes(leaseKey[:])       // LeaseKey (16 bytes)
	w.WriteUint32(leaseState)       // LeaseState
	w.WriteUint32(flags)            // Flags
	w.WriteUint64(0)                // LeaseDuration
	w.WriteBytes(parentLeaseKey[:]) // ParentLeaseKey (16 bytes)
	w.WriteUint16(epoch)            // Epoch
	w.WriteUint16(0)                // Reserved
	return w.Bytes()
}

// ============================================================================
// Lease Break Notification Encoding
// ============================================================================

// EncodeLeaseBreakNotification encodes an SMB2 Lease Break Notification (44 bytes).
//
// Per MS-SMB2 2.2.23.2:
//   - Flags: 0x01 if ackRequired (SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED)
//   - NewEpoch: Set for V2 clients (epoch + 1 from current lease epoch)
//
// Wire format:
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 44
//	2       2     NewEpoch           New epoch value
//	4       4     Flags              ACK_REQUIRED flag
//	8       16    LeaseKey           Lease identifier
//	24      4     CurrentLeaseState  What client currently has
//	28      4     NewLeaseState      What client should break to
//	32      12    Reserved           Reserved (0)
func EncodeLeaseBreakNotification(
	leaseKey [16]byte,
	currentState, newState uint32,
	ackRequired bool,
	epoch uint16,
) []byte {
	var flags uint32
	if ackRequired {
		flags = LeaseBreakFlagAckRequired
	}

	w := NewWriter(LeaseBreakNotificationSize)
	w.WriteUint16(LeaseBreakNotificationSize) // StructureSize
	w.WriteUint16(epoch)                      // NewEpoch
	w.WriteUint32(flags)                      // Flags
	w.WriteBytes(leaseKey[:])                 // LeaseKey (16 bytes)
	w.WriteUint32(currentState)               // CurrentLeaseState
	w.WriteUint32(newState)                   // NewLeaseState
	w.WriteZeros(12)                          // Reserved (12 bytes)
	return w.Bytes()
}

// ============================================================================
// Lease Break Acknowledgment Decoding
// ============================================================================

// DecodeLeaseBreakAck parses an SMB2 Lease Break Acknowledgment (36 bytes).
//
// Wire format:
//
//	Offset  Size  Field          Description
//	0       2     StructureSize  Always 36
//	2       2     Reserved       Reserved (0)
//	4       4     Flags          Reserved (0)
//	8       16    LeaseKey       Lease identifier
//	24      4     LeaseState     State client is acknowledging
//	28      8     Reserved       Reserved (0)
//
// Returns the parsed lease key, lease state, and any error.
func DecodeLeaseBreakAck(data []byte) (leaseKey [16]byte, leaseState uint32, err error) {
	if len(data) < LeaseBreakAckSize {
		return leaseKey, 0, &ParseError{Field: "LeaseBreakAck", Message: "too short"}
	}

	r := NewReader(data)
	structSize := r.ReadUint16()
	if structSize != LeaseBreakAckSize {
		return leaseKey, 0, &ParseError{Field: "StructureSize", Message: "invalid"}
	}

	r.Skip(6) // Reserved(2) + Flags(4)
	keyBytes := r.ReadBytes(16)
	leaseState = r.ReadUint32()
	if r.Err() != nil {
		return leaseKey, 0, r.Err()
	}

	copy(leaseKey[:], keyBytes)
	return leaseKey, leaseState, nil
}

// ParseError describes a parse error for SMB lease structures.
type ParseError struct {
	Field   string
	Message string
}

func (e *ParseError) Error() string {
	return "smbenc: " + e.Field + ": " + e.Message
}
