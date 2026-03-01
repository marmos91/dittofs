package types

import (
	"errors"
	"fmt"
)

// Common errors for SMB2 protocol
var (
	ErrNotSupported = errors.New("operation not supported")
)

// =============================================================================
// NT_STATUS Codes
// =============================================================================

// Status represents an NT_STATUS code returned in SMB2 responses.
//
// NT_STATUS codes are 32-bit values divided into:
//   - Severity (bits 30-31): 00=Success, 01=Informational, 10=Warning, 11=Error
//   - Customer (bit 29): 0=Microsoft-defined, 1=Customer-defined
//   - Facility (bits 16-28): Component that generated the status
//   - Code (bits 0-15): Status code within the facility
//
// Common patterns:
//   - 0x00000000: Success
//   - 0x80000xxx: Warning (high bit set, bit 30 clear)
//   - 0xC0000xxx: Error (both high bits set)
//
// [MS-ERREF] Section 2.3
type Status uint32

const (
	// Success codes (severity = 00)

	// StatusSuccess indicates the operation completed successfully.
	StatusSuccess Status = 0x00000000

	// StatusPending indicates the operation is in progress (async).
	StatusPending Status = 0x00000103

	// StatusMoreEntries indicates more directory entries are available.
	StatusMoreEntries Status = 0x00000105

	// Warning codes (severity = 10, high bit set, bit 30 clear)

	// StatusBufferOverflow indicates the buffer was too small;
	// partial data was returned.
	StatusBufferOverflow Status = 0x80000005

	// StatusNoMoreFiles indicates directory enumeration is complete.
	// This is expected at the end of QUERY_DIRECTORY operations.
	StatusNoMoreFiles Status = 0x80000006

	// Error codes (severity = 11, both high bits set)

	// StatusInvalidInfoClass indicates an invalid information class was requested.
	StatusInvalidInfoClass Status = 0xC0000003

	// StatusInfoLengthMismatch indicates the buffer is smaller than the
	// minimum size required for the requested information class.
	StatusInfoLengthMismatch Status = 0xC0000004

	// StatusInvalidHandle indicates the file handle is invalid or closed.
	StatusInvalidHandle Status = 0xC0000008

	// StatusInvalidParameter indicates a parameter is invalid.
	StatusInvalidParameter Status = 0xC000000D

	// StatusNoSuchFile indicates the file was not found.
	StatusNoSuchFile Status = 0xC000000F

	// StatusInvalidDeviceRequest indicates the request is not valid for this device.
	StatusInvalidDeviceRequest Status = 0xC0000010

	// StatusEndOfFile indicates an attempt to read past end of file.
	StatusEndOfFile Status = 0xC0000011

	// StatusMoreProcessingRequired indicates more authentication steps needed.
	// Used during NTLM/Kerberos handshake.
	StatusMoreProcessingRequired Status = 0xC0000016

	// StatusAccessDenied indicates the caller lacks required permissions.
	StatusAccessDenied Status = 0xC0000022

	// StatusBufferTooSmall indicates the buffer is too small.
	StatusBufferTooSmall Status = 0xC0000023

	// StatusObjectNameInvalid indicates the object name is malformed.
	StatusObjectNameInvalid Status = 0xC0000033

	// StatusObjectNameNotFound indicates the named object was not found.
	StatusObjectNameNotFound Status = 0xC0000034

	// StatusObjectNameCollision indicates the name already exists.
	StatusObjectNameCollision Status = 0xC0000035

	// StatusObjectPathSyntaxBad indicates the path has invalid syntax (e.g., ".." traversal).
	StatusObjectPathSyntaxBad Status = 0xC000003B

	// StatusObjectPathNotFound indicates a path component was not found.
	StatusObjectPathNotFound Status = 0xC000003A

	// StatusSharingViolation indicates a sharing conflict.
	StatusSharingViolation Status = 0xC0000043

	// StatusDeletePending indicates the file is marked for deletion.
	StatusDeletePending Status = 0xC0000056

	// StatusLogonFailure indicates authentication failed.
	StatusLogonFailure Status = 0xC000006D

	// StatusInsufficientResources indicates server lacks resources.
	StatusInsufficientResources Status = 0xC000009A

	// StatusFileIsADirectory indicates a file operation was attempted on a directory.
	StatusFileIsADirectory Status = 0xC00000BA

	// StatusNotSupported indicates the operation is not supported.
	StatusNotSupported Status = 0xC00000BB

	// StatusNetworkNameDeleted indicates the share was deleted.
	StatusNetworkNameDeleted Status = 0xC00000C9

	// StatusBadNetworkName indicates the share name was not found.
	StatusBadNetworkName Status = 0xC00000CC

	// StatusRequestNotAccepted indicates the server cannot accept the request.
	StatusRequestNotAccepted Status = 0xC00000D0

	// StatusInternalError indicates an internal server error.
	StatusInternalError Status = 0xC00000E5

	// StatusDirectoryNotEmpty indicates the directory is not empty.
	StatusDirectoryNotEmpty Status = 0xC0000101

	// StatusNotADirectory indicates a directory operation was attempted on a file.
	StatusNotADirectory Status = 0xC0000103

	// StatusCancelled indicates the operation was cancelled.
	StatusCancelled Status = 0xC0000120

	// StatusFileClosed indicates the file handle was closed.
	StatusFileClosed Status = 0xC0000128

	// StatusUserSessionDeleted indicates the session was deleted.
	StatusUserSessionDeleted Status = 0xC0000203

	// StatusPathNotCovered indicates a DFS path is not covered.
	StatusPathNotCovered Status = 0xC0000257

	// StatusNetworkSessionExpired indicates the session expired.
	StatusNetworkSessionExpired Status = 0xC000035C

	// StatusDiskFull indicates the disk is full.
	StatusDiskFull Status = 0xC000007F

	// StatusUnexpectedIOError indicates an unexpected I/O error occurred.
	StatusUnexpectedIOError Status = 0xC00000E9

	// StatusNotAReparsePoint indicates the file is not a reparse point.
	StatusNotAReparsePoint Status = 0xC0000275

	// StatusLockNotGranted indicates the byte-range lock could not be acquired.
	// Used when a lock request conflicts with an existing lock.
	StatusLockNotGranted Status = 0xC0000054

	// StatusRangeNotLocked indicates no lock exists for the specified range.
	// Used when trying to unlock a range that was not locked.
	StatusRangeNotLocked Status = 0xC000007E
)

// String returns a human-readable name for the status code.
func (s Status) String() string {
	switch s {
	case StatusSuccess:
		return "STATUS_SUCCESS"
	case StatusPending:
		return "STATUS_PENDING"
	case StatusMoreProcessingRequired:
		return "STATUS_MORE_PROCESSING_REQUIRED"
	case StatusInvalidParameter:
		return "STATUS_INVALID_PARAMETER"
	case StatusNoSuchFile:
		return "STATUS_NO_SUCH_FILE"
	case StatusEndOfFile:
		return "STATUS_END_OF_FILE"
	case StatusMoreEntries:
		return "STATUS_MORE_ENTRIES"
	case StatusNoMoreFiles:
		return "STATUS_NO_MORE_FILES"
	case StatusAccessDenied:
		return "STATUS_ACCESS_DENIED"
	case StatusBufferOverflow:
		return "STATUS_BUFFER_OVERFLOW"
	case StatusObjectNameInvalid:
		return "STATUS_OBJECT_NAME_INVALID"
	case StatusObjectNameNotFound:
		return "STATUS_OBJECT_NAME_NOT_FOUND"
	case StatusObjectNameCollision:
		return "STATUS_OBJECT_NAME_COLLISION"
	case StatusObjectPathNotFound:
		return "STATUS_OBJECT_PATH_NOT_FOUND"
	case StatusSharingViolation:
		return "STATUS_SHARING_VIOLATION"
	case StatusDeletePending:
		return "STATUS_DELETE_PENDING"
	case StatusFileClosed:
		return "STATUS_FILE_CLOSED"
	case StatusInvalidHandle:
		return "STATUS_INVALID_HANDLE"
	case StatusNotSupported:
		return "STATUS_NOT_SUPPORTED"
	case StatusDirectoryNotEmpty:
		return "STATUS_DIRECTORY_NOT_EMPTY"
	case StatusNotADirectory:
		return "STATUS_NOT_A_DIRECTORY"
	case StatusFileIsADirectory:
		return "STATUS_FILE_IS_A_DIRECTORY"
	case StatusBadNetworkName:
		return "STATUS_BAD_NETWORK_NAME"
	case StatusUserSessionDeleted:
		return "STATUS_USER_SESSION_DELETED"
	case StatusNetworkSessionExpired:
		return "STATUS_NETWORK_SESSION_EXPIRED"
	case StatusInvalidDeviceRequest:
		return "STATUS_INVALID_DEVICE_REQUEST"
	case StatusInternalError:
		return "STATUS_INTERNAL_ERROR"
	case StatusInsufficientResources:
		return "STATUS_INSUFFICIENT_RESOURCES"
	case StatusRequestNotAccepted:
		return "STATUS_REQUEST_NOT_ACCEPTED"
	case StatusLogonFailure:
		return "STATUS_LOGON_FAILURE"
	case StatusPathNotCovered:
		return "STATUS_PATH_NOT_COVERED"
	case StatusNetworkNameDeleted:
		return "STATUS_NETWORK_NAME_DELETED"
	case StatusInvalidInfoClass:
		return "STATUS_INVALID_INFO_CLASS"
	case StatusBufferTooSmall:
		return "STATUS_BUFFER_TOO_SMALL"
	case StatusCancelled:
		return "STATUS_CANCELLED"
	case StatusDiskFull:
		return "STATUS_DISK_FULL"
	case StatusUnexpectedIOError:
		return "STATUS_UNEXPECTED_IO_ERROR"
	case StatusNotAReparsePoint:
		return "STATUS_NOT_A_REPARSE_POINT"
	case StatusLockNotGranted:
		return "STATUS_LOCK_NOT_GRANTED"
	case StatusRangeNotLocked:
		return "STATUS_RANGE_NOT_LOCKED"
	default:
		return fmt.Sprintf("STATUS_0x%08X", uint32(s))
	}
}

// IsSuccess returns true if the status indicates success.
// NT_STATUS success codes have severity 00 (bits 30-31 are 0).
func (s Status) IsSuccess() bool {
	return s == StatusSuccess || (uint32(s)&0x80000000) == 0
}

// IsError returns true if the status indicates an error.
// NT_STATUS error codes have severity 11 (bits 30-31 are both set).
func (s Status) IsError() bool {
	return (uint32(s) & 0xC0000000) == 0xC0000000
}

// IsWarning returns true if the status indicates a warning.
// NT_STATUS warning codes have severity 10 (bit 31 set, bit 30 clear).
func (s Status) IsWarning() bool {
	return (uint32(s) & 0xC0000000) == 0x80000000
}

// Severity returns the severity level (0-3) of the status.
//   - 0: Success
//   - 1: Informational
//   - 2: Warning
//   - 3: Error
func (s Status) Severity() int {
	return int((uint32(s) >> 30) & 0x3)
}
