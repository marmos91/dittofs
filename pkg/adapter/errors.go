package adapter

// ProtocolError represents a protocol-specific error with a numeric status code.
//
// Each protocol adapter implements MapError to translate domain errors (e.g.,
// metadata.ErrNoEntity, payload.ErrContentNotFound) into wire-format error codes
// appropriate for the protocol:
//
//   - NFS: NFS3ERR_NOENT (2), NFS3ERR_ACCES (13), etc.
//   - SMB: STATUS_OBJECT_NAME_NOT_FOUND (0xC0000034), STATUS_ACCESS_DENIED, etc.
//
// ProtocolError extends the standard error interface and supports errors.Is()
// via Unwrap(), allowing callers to check for both the protocol-level error and
// the underlying domain error.
type ProtocolError interface {
	error

	// Code returns the numeric protocol status code (e.g., NFS3ERR_NOENT = 2,
	// NTSTATUS = 0xC0000034).
	Code() uint32

	// Message returns a human-readable description of the protocol error.
	Message() string

	// Unwrap returns the underlying domain error, enabling errors.Is() to match
	// the original sentinel error through the ProtocolError wrapper.
	Unwrap() error
}
