package session

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// IsCreditExempt returns true for commands that bypass credit checks.
// Per MS-SMB2 3.3.5.1 (NEGOTIATE) and 3.3.5.2.3 (CANCEL).
// First SESSION_SETUP (SessionID=0) is also exempt because the client
// starts with 0 credits per MS-SMB2 3.3.5.1.
//
// NEGOTIATE and first SESSION_SETUP are exempt only when SessionID is 0,
// indicating these are pre-authentication requests before any credits
// have been granted. CANCEL is always exempt regardless of SessionID.
func IsCreditExempt(command types.Command, sessionID uint64) bool {
	// CANCEL is always exempt per MS-SMB2 3.3.5.2.3
	if command == types.CommandCancel {
		return true
	}

	// NEGOTIATE and first SESSION_SETUP (SessionID=0) are exempt
	// per MS-SMB2 3.3.5.1
	if sessionID == 0 {
		if command == types.CommandNegotiate || command == types.CommandSessionSetup {
			return true
		}
	}

	return false
}

// EffectiveCreditCharge returns the actual credit charge for a request.
// CreditCharge=0 means 1 credit (SMB 2.0.2 compatibility, per MS-SMB2 3.3.5.2.3).
func EffectiveCreditCharge(creditCharge uint16) uint16 {
	if creditCharge == 0 {
		return 1
	}
	return creditCharge
}

// ValidateCreditCharge validates CreditCharge against the payload/response size.
// Per MS-SMB2 3.3.5.2.5: the server MUST verify that CreditCharge is sufficient
// for the payload. Only applies to commands with variable-length payloads:
// READ (response Length), WRITE (data length), IOCTL (MaxOutputResponse),
// QUERY_DIRECTORY (OutputBufferLength).
//
// Parameters:
//   - command: the SMB2 command code
//   - creditCharge: the CreditCharge from the request header
//   - body: the request body bytes (used to extract payload size fields)
//
// Returns nil if valid, error describing the violation if invalid.
func ValidateCreditCharge(command types.Command, creditCharge uint16, body []byte) error {
	payloadSize, ok := extractPayloadSize(command, body)
	if !ok {
		// Not a payload command or body too short — no validation needed
		return nil
	}

	effectiveCharge := EffectiveCreditCharge(creditCharge)
	requiredCharge := CalculateCreditCharge(payloadSize)

	if requiredCharge > effectiveCharge {
		return fmt.Errorf(
			"insufficient CreditCharge for %s: have %d, need %d (payload %d bytes)",
			command, effectiveCharge, requiredCharge, payloadSize,
		)
	}

	return nil
}

// extractPayloadSize extracts the payload/response size from a request body
// based on the command type. Returns the size and true if this is a payload
// command, or 0 and false if no validation is needed.
func extractPayloadSize(command types.Command, body []byte) (uint32, bool) {
	switch command {
	case types.CommandRead:
		// SMB2 READ request: Length field at body offset 4, uint32 LE
		// [MS-SMB2] Section 2.2.19
		if len(body) < 8 {
			return 0, false
		}
		return binary.LittleEndian.Uint32(body[4:8]), true

	case types.CommandWrite:
		// SMB2 WRITE request: Length (DataLength) field at body offset 4, uint32 LE
		// [MS-SMB2] Section 2.2.21
		if len(body) < 8 {
			return 0, false
		}
		return binary.LittleEndian.Uint32(body[4:8]), true

	case types.CommandIoctl:
		// SMB2 IOCTL request: MaxOutputResponse at body offset 28, uint32 LE
		// [MS-SMB2] Section 2.2.31
		if len(body) < 32 {
			return 0, false
		}
		return binary.LittleEndian.Uint32(body[28:32]), true

	case types.CommandQueryDirectory:
		// SMB2 QUERY_DIRECTORY request: OutputBufferLength at body offset 4, uint32 LE
		// [MS-SMB2] Section 2.2.33
		if len(body) < 8 {
			return 0, false
		}
		return binary.LittleEndian.Uint32(body[4:8]), true

	default:
		// All other commands: no payload validation needed
		return 0, false
	}
}
