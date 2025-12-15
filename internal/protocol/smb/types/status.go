package types

import (
	"errors"
	"fmt"
)

// Common errors for SMB2 protocol
var (
	ErrNotSupported = errors.New("operation not supported")
)

// NT_STATUS codes [MS-ERREF] 2.3
const (
	StatusSuccess                uint32 = 0x00000000
	StatusPending                uint32 = 0x00000103
	StatusMoreProcessingRequired uint32 = 0xC0000016
	StatusInvalidParameter       uint32 = 0xC000000D
	StatusNoSuchFile             uint32 = 0xC000000F
	StatusEndOfFile              uint32 = 0xC0000011
	StatusMoreEntries            uint32 = 0x00000105
	StatusNoMoreFiles            uint32 = 0x80000006
	StatusAccessDenied           uint32 = 0xC0000022
	StatusBufferOverflow         uint32 = 0x80000005
	StatusObjectNameInvalid      uint32 = 0xC0000033
	StatusObjectNameNotFound     uint32 = 0xC0000034
	StatusObjectNameCollision    uint32 = 0xC0000035
	StatusObjectPathNotFound     uint32 = 0xC000003A
	StatusSharingViolation       uint32 = 0xC0000043
	StatusDeletePending          uint32 = 0xC0000056
	StatusFileClosed             uint32 = 0xC0000128
	StatusInvalidHandle          uint32 = 0xC0000008
	StatusNotSupported           uint32 = 0xC00000BB
	StatusDirectoryNotEmpty      uint32 = 0xC0000101
	StatusNotADirectory          uint32 = 0xC0000103
	StatusFileIsADirectory       uint32 = 0xC00000BA
	StatusBadNetworkName         uint32 = 0xC00000CC
	StatusUserSessionDeleted     uint32 = 0xC0000203
	StatusNetworkSessionExpired  uint32 = 0xC000035C
	StatusInvalidDeviceRequest   uint32 = 0xC0000010
	StatusInternalError          uint32 = 0xC00000E5
	StatusInsufficientResources  uint32 = 0xC000009A
	StatusRequestNotAccepted     uint32 = 0xC00000D0
	StatusLogonFailure           uint32 = 0xC000006D
	StatusPathNotCovered         uint32 = 0xC0000257
	StatusNetworkNameDeleted     uint32 = 0xC00000C9
	StatusInvalidInfoClass       uint32 = 0xC0000003
	StatusBufferTooSmall         uint32 = 0xC0000023
	StatusCancelled              uint32 = 0xC0000120
)

// StatusName returns a human-readable name for NT_STATUS codes
func StatusName(status uint32) string {
	switch status {
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
	default:
		return fmt.Sprintf("STATUS_0x%08X", status)
	}
}

// IsSuccess returns true if the status indicates success
func IsSuccess(status uint32) bool {
	// NT_STATUS success codes have the high bit clear
	return status == StatusSuccess || (status&0x80000000) == 0
}

// IsError returns true if the status indicates an error
func IsError(status uint32) bool {
	// NT_STATUS error codes have the two high bits set (0xC0000000)
	return (status & 0xC0000000) == 0xC0000000
}

// IsWarning returns true if the status indicates a warning
func IsWarning(status uint32) bool {
	// NT_STATUS warning codes have bit 31 set but not bit 30 (0x80000000)
	return (status&0xC0000000) == 0x80000000
}
