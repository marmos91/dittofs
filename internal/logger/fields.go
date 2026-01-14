package logger

import (
	"fmt"
	"log/slog"
)

// Standard field keys for structured logging.
// These keys are designed to be protocol-agnostic, supporting NFS, SMB, and future protocols.
// Use these keys consistently across all log statements for log aggregation and querying.
const (
	// ========================================================================
	// Distributed Tracing
	// ========================================================================
	KeyTraceID = "trace_id" // OpenTelemetry trace ID for request correlation
	KeySpanID  = "span_id"  // OpenTelemetry span ID for operation tracking

	// ========================================================================
	// Protocol & Operation (protocol-agnostic)
	// ========================================================================
	KeyProtocol  = "protocol"   // Protocol type: nfs, smb, webdav, etc.
	KeyProcedure = "procedure"  // Operation/procedure name: READ, WRITE, CREATE, etc.
	KeyHandle    = "handle"     // File handle (protocol-specific opaque identifier)
	KeyShare     = "share"      // Share/export name: /export, \\server\share, etc.
	KeyStatus    = "status"     // Operation status code (protocol-specific)
	KeyStatusMsg = "status_msg" // Human-readable status message

	// ========================================================================
	// File System Operations
	// ========================================================================
	KeyPath       = "path"        // Full file/directory path
	KeyFilename   = "filename"    // File or directory name (basename)
	KeyParentPath = "parent_path" // Parent directory path
	KeyOldPath    = "old_path"    // Source path for rename/move operations
	KeyNewPath    = "new_path"    // Destination path for rename/move operations
	KeyType       = "type"        // File type: file, directory, symlink, etc.
	KeySize       = "size"        // File size in bytes
	KeyMode       = "mode"        // File mode/permissions (Unix-style)

	// ========================================================================
	// I/O Operations
	// ========================================================================
	KeyOffset       = "offset"        // File offset for read/write operations
	KeyCount        = "count"         // Byte count requested
	KeyBytesRead    = "bytes_read"    // Actual bytes read
	KeyBytesWritten = "bytes_written" // Actual bytes written
	KeyEOF          = "eof"           // End of file indicator
	KeyStable       = "stable"        // Write durability level (sync, async, etc.)

	// ========================================================================
	// Client Identification
	// ========================================================================
	KeyClientIP   = "client_ip"   // Client IP address
	KeyClientPort = "client_port" // Client source port
	KeyClientHost = "client_host" // Client hostname (if resolved)
	KeyUID        = "uid"         // User ID (Unix UID or mapped ID)
	KeyGID        = "gid"         // Group ID (Unix GID or mapped ID)
	KeyUsername   = "username"    // Username (SMB, WebDAV)
	KeyDomain     = "domain"      // Domain name (SMB, AD)
	KeyAuth       = "auth"        // Authentication method/flavor

	// ========================================================================
	// Session & Connection
	// ========================================================================
	KeySessionID    = "session_id"    // Session identifier (SMB session, etc.)
	KeyConnectionID = "connection_id" // Connection identifier
	KeyRequestID    = "request_id"    // Protocol-specific request ID (XID, MessageID)

	// ========================================================================
	// Operation Metadata
	// ========================================================================
	KeyDurationMs = "duration_ms" // Operation duration in milliseconds
	KeyError      = "error"       // Error message
	KeyErrorCode  = "error_code"  // Numeric error code
	KeySource     = "source"      // Data source: cache, content_store, metadata_store
	KeyOperation  = "operation"   // Sub-operation type for complex operations

	// ========================================================================
	// Storage Backend (Content Store)
	// ========================================================================
	KeyPayloadID  = "content_id"  // Content identifier in content store
	KeyStoreName  = "store_name"  // Named store identifier from registry
	KeyStoreType  = "store_type"  // Store type: memory, filesystem, s3, azure, gcs
	KeyBucket     = "bucket"      // Cloud bucket name (S3, GCS)
	KeyContainer  = "container"   // Cloud container name (Azure Blob)
	KeyKey        = "key"         // Object key in cloud storage
	KeyRegion     = "region"      // Cloud region
	KeyAttempt    = "attempt"     // Retry attempt number
	KeyMaxRetries = "max_retries" // Maximum retry attempts

	// ========================================================================
	// Metadata Store
	// ========================================================================
	KeyMetadataStore = "metadata_store" // Metadata store name
	KeyInodeID       = "inode_id"       // Inode identifier (for inode-based stores)

	// ========================================================================
	// Cache Layer
	// ========================================================================
	KeyCacheHit      = "cache_hit"      // Cache hit indicator
	KeyCacheState    = "cache_state"    // Cache state: dirty, clean, uploading
	KeyCacheSize     = "cache_size"     // Current cache size
	KeyCacheCapacity = "cache_capacity" // Maximum cache capacity
	KeyEvicted       = "evicted"        // Number of entries evicted

	// ========================================================================
	// Directory Operations
	// ========================================================================
	KeyEntries    = "entries"     // Number of directory entries
	KeyCookieEnd  = "cookie_end"  // Continuation cookie/marker
	KeyPattern    = "pattern"     // Search/filter pattern (SMB wildcard, etc.)
	KeyMaxEntries = "max_entries" // Maximum entries requested

	// ========================================================================
	// Link Operations
	// ========================================================================
	KeyLinkTarget = "link_target" // Symbolic link target path
	KeyLinkCount  = "link_count"  // Hard link count

	// ========================================================================
	// Locking (future NLM, SMB oplocks)
	// ========================================================================
	KeyLockType   = "lock_type"   // Lock type: read, write, exclusive
	KeyLockOffset = "lock_offset" // Lock range start
	KeyLockLength = "lock_length" // Lock range length
	KeyLockOwner  = "lock_owner"  // Lock owner identifier
)

// ============================================================================
// Field constructors for type safety
// These functions provide type-safe construction of slog.Attr values.
// ============================================================================

// ----------------------------------------------------------------------------
// Distributed Tracing
// ----------------------------------------------------------------------------

// TraceID returns a slog.Attr for OpenTelemetry trace ID
func TraceID(id string) slog.Attr {
	return slog.String(KeyTraceID, id)
}

// SpanID returns a slog.Attr for OpenTelemetry span ID
func SpanID(id string) slog.Attr {
	return slog.String(KeySpanID, id)
}

// ----------------------------------------------------------------------------
// Protocol & Operation
// ----------------------------------------------------------------------------

// Protocol returns a slog.Attr for protocol type (nfs, smb, webdav, etc.)
func Protocol(proto string) slog.Attr {
	return slog.String(KeyProtocol, proto)
}

// Procedure returns a slog.Attr for operation/procedure name
func Procedure(name string) slog.Attr {
	return slog.String(KeyProcedure, name)
}

// Handle returns a slog.Attr for a file handle (formatted as hex)
func Handle(h []byte) slog.Attr {
	return slog.String(KeyHandle, fmt.Sprintf("%x", h))
}

// HandleHex returns a slog.Attr for a file handle already in hex format
func HandleHex(h string) slog.Attr {
	return slog.String(KeyHandle, h)
}

// Share returns a slog.Attr for share/export name
func Share(name string) slog.Attr {
	return slog.String(KeyShare, name)
}

// Status returns a slog.Attr for operation status code
func Status(code int) slog.Attr {
	return slog.Int(KeyStatus, code)
}

// StatusMsg returns a slog.Attr for human-readable status message
func StatusMsg(msg string) slog.Attr {
	return slog.String(KeyStatusMsg, msg)
}

// ----------------------------------------------------------------------------
// File System Operations
// ----------------------------------------------------------------------------

// Path returns a slog.Attr for file/directory path
func Path(p string) slog.Attr {
	return slog.String(KeyPath, p)
}

// Filename returns a slog.Attr for filename (basename)
func Filename(name string) slog.Attr {
	return slog.String(KeyFilename, name)
}

// ParentPath returns a slog.Attr for parent directory path
func ParentPath(p string) slog.Attr {
	return slog.String(KeyParentPath, p)
}

// OldPath returns a slog.Attr for source path in rename/move operations
func OldPath(p string) slog.Attr {
	return slog.String(KeyOldPath, p)
}

// NewPath returns a slog.Attr for destination path in rename/move operations
func NewPath(p string) slog.Attr {
	return slog.String(KeyNewPath, p)
}

// Type returns a slog.Attr for file type
func Type(t int) slog.Attr {
	return slog.Int(KeyType, t)
}

// TypeStr returns a slog.Attr for file type as string
func TypeStr(t string) slog.Attr {
	return slog.String(KeyType, t)
}

// Size returns a slog.Attr for file size
func Size(s uint64) slog.Attr {
	return slog.Uint64(KeySize, s)
}

// Mode returns a slog.Attr for file mode/permissions
func Mode(m uint32) slog.Attr {
	return slog.Any(KeyMode, m)
}

// ----------------------------------------------------------------------------
// I/O Operations
// ----------------------------------------------------------------------------

// Offset returns a slog.Attr for file offset
func Offset(off uint64) slog.Attr {
	return slog.Uint64(KeyOffset, off)
}

// Count returns a slog.Attr for byte count requested
func Count(c uint32) slog.Attr {
	return slog.Any(KeyCount, c)
}

// BytesRead returns a slog.Attr for actual bytes read
func BytesRead(n int) slog.Attr {
	return slog.Int(KeyBytesRead, n)
}

// BytesWritten returns a slog.Attr for actual bytes written
func BytesWritten(n int) slog.Attr {
	return slog.Int(KeyBytesWritten, n)
}

// EOF returns a slog.Attr for end-of-file indicator
func EOF(eof bool) slog.Attr {
	return slog.Bool(KeyEOF, eof)
}

// Stable returns a slog.Attr for write durability level
func Stable(s int) slog.Attr {
	return slog.Int(KeyStable, s)
}

// ----------------------------------------------------------------------------
// Client Identification
// ----------------------------------------------------------------------------

// ClientIP returns a slog.Attr for client IP address
func ClientIP(addr string) slog.Attr {
	return slog.String(KeyClientIP, addr)
}

// ClientPort returns a slog.Attr for client source port
func ClientPort(port int) slog.Attr {
	return slog.Int(KeyClientPort, port)
}

// ClientHost returns a slog.Attr for client hostname
func ClientHost(host string) slog.Attr {
	return slog.String(KeyClientHost, host)
}

// UID returns a slog.Attr for user ID
func UID(uid uint32) slog.Attr {
	return slog.Any(KeyUID, uid)
}

// GID returns a slog.Attr for group ID
func GID(gid uint32) slog.Attr {
	return slog.Any(KeyGID, gid)
}

// Username returns a slog.Attr for username
func Username(name string) slog.Attr {
	return slog.String(KeyUsername, name)
}

// Domain returns a slog.Attr for domain name
func Domain(name string) slog.Attr {
	return slog.String(KeyDomain, name)
}

// Auth returns a slog.Attr for authentication method/flavor
func Auth(flavor uint32) slog.Attr {
	return slog.Any(KeyAuth, flavor)
}

// AuthStr returns a slog.Attr for authentication method as string
func AuthStr(method string) slog.Attr {
	return slog.String(KeyAuth, method)
}

// ----------------------------------------------------------------------------
// Session & Connection
// ----------------------------------------------------------------------------

// SessionID returns a slog.Attr for session identifier
func SessionID(id string) slog.Attr {
	return slog.String(KeySessionID, id)
}

// ConnectionID returns a slog.Attr for connection identifier
func ConnectionID(id string) slog.Attr {
	return slog.String(KeyConnectionID, id)
}

// RequestID returns a slog.Attr for protocol-specific request ID
func RequestID(id uint32) slog.Attr {
	return slog.Any(KeyRequestID, id)
}

// RequestIDStr returns a slog.Attr for request ID as string
func RequestIDStr(id string) slog.Attr {
	return slog.String(KeyRequestID, id)
}

// ----------------------------------------------------------------------------
// Operation Metadata
// ----------------------------------------------------------------------------

// DurationMs returns a slog.Attr for duration in milliseconds
func DurationMs(ms float64) slog.Attr {
	return slog.Float64(KeyDurationMs, ms)
}

// Err returns a slog.Attr for an error
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String(KeyError, err.Error())
}

// ErrorCode returns a slog.Attr for numeric error code
func ErrorCode(code int) slog.Attr {
	return slog.Int(KeyErrorCode, code)
}

// Source returns a slog.Attr for data source
func Source(src string) slog.Attr {
	return slog.String(KeySource, src)
}

// Operation returns a slog.Attr for sub-operation type
func Operation(op string) slog.Attr {
	return slog.String(KeyOperation, op)
}

// ----------------------------------------------------------------------------
// Storage Backend (Content Store)
// ----------------------------------------------------------------------------

// PayloadID returns a slog.Attr for content identifier
func PayloadID(id string) slog.Attr {
	return slog.String(KeyPayloadID, id)
}

// StoreName returns a slog.Attr for named store identifier
func StoreName(name string) slog.Attr {
	return slog.String(KeyStoreName, name)
}

// StoreType returns a slog.Attr for store type
func StoreType(t string) slog.Attr {
	return slog.String(KeyStoreType, t)
}

// Bucket returns a slog.Attr for cloud bucket name
func Bucket(name string) slog.Attr {
	return slog.String(KeyBucket, name)
}

// Container returns a slog.Attr for Azure container name
func Container(name string) slog.Attr {
	return slog.String(KeyContainer, name)
}

// Key returns a slog.Attr for object key in cloud storage
func Key(k string) slog.Attr {
	return slog.String(KeyKey, k)
}

// Region returns a slog.Attr for cloud region
func Region(r string) slog.Attr {
	return slog.String(KeyRegion, r)
}

// Attempt returns a slog.Attr for retry attempt number
func Attempt(n int) slog.Attr {
	return slog.Int(KeyAttempt, n)
}

// MaxRetries returns a slog.Attr for maximum retry attempts
func MaxRetries(n int) slog.Attr {
	return slog.Int(KeyMaxRetries, n)
}

// ----------------------------------------------------------------------------
// Metadata Store
// ----------------------------------------------------------------------------

// MetadataStore returns a slog.Attr for metadata store name
func MetadataStore(name string) slog.Attr {
	return slog.String(KeyMetadataStore, name)
}

// InodeID returns a slog.Attr for inode identifier
func InodeID(id uint64) slog.Attr {
	return slog.Uint64(KeyInodeID, id)
}

// ----------------------------------------------------------------------------
// Cache Layer
// ----------------------------------------------------------------------------

// CacheHit returns a slog.Attr for cache hit indicator
func CacheHit(hit bool) slog.Attr {
	return slog.Bool(KeyCacheHit, hit)
}

// CacheState returns a slog.Attr for cache state
func CacheState(state string) slog.Attr {
	return slog.String(KeyCacheState, state)
}

// CacheSize returns a slog.Attr for current cache size
func CacheSize(size int64) slog.Attr {
	return slog.Int64(KeyCacheSize, size)
}

// CacheCapacity returns a slog.Attr for maximum cache capacity
func CacheCapacity(capacity int64) slog.Attr {
	return slog.Int64(KeyCacheCapacity, capacity)
}

// Evicted returns a slog.Attr for number of entries evicted
func Evicted(n int) slog.Attr {
	return slog.Int(KeyEvicted, n)
}

// ----------------------------------------------------------------------------
// Directory Operations
// ----------------------------------------------------------------------------

// Entries returns a slog.Attr for number of directory entries
func Entries(n int) slog.Attr {
	return slog.Int(KeyEntries, n)
}

// CookieEnd returns a slog.Attr for continuation cookie/marker
func CookieEnd(cookie uint64) slog.Attr {
	return slog.Uint64(KeyCookieEnd, cookie)
}

// Pattern returns a slog.Attr for search/filter pattern
func Pattern(p string) slog.Attr {
	return slog.String(KeyPattern, p)
}

// MaxEntries returns a slog.Attr for maximum entries requested
func MaxEntries(n int) slog.Attr {
	return slog.Int(KeyMaxEntries, n)
}

// ----------------------------------------------------------------------------
// Link Operations
// ----------------------------------------------------------------------------

// LinkTarget returns a slog.Attr for symbolic link target path
func LinkTarget(target string) slog.Attr {
	return slog.String(KeyLinkTarget, target)
}

// LinkCount returns a slog.Attr for hard link count
func LinkCount(count uint32) slog.Attr {
	return slog.Any(KeyLinkCount, count)
}

// ----------------------------------------------------------------------------
// Locking
// ----------------------------------------------------------------------------

// LockType returns a slog.Attr for lock type
func LockType(t string) slog.Attr {
	return slog.String(KeyLockType, t)
}

// LockOffset returns a slog.Attr for lock range start
func LockOffset(off uint64) slog.Attr {
	return slog.Uint64(KeyLockOffset, off)
}

// LockLength returns a slog.Attr for lock range length
func LockLength(len uint64) slog.Attr {
	return slog.Uint64(KeyLockLength, len)
}

// LockOwner returns a slog.Attr for lock owner identifier
func LockOwner(owner string) slog.Attr {
	return slog.String(KeyLockOwner, owner)
}
