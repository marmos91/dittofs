package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Common attribute keys for protocol operations.
// These follow OpenTelemetry semantic conventions where applicable.
// Protocol-agnostic keys use "fs." prefix, protocol-specific use their own prefix.
const (
	// ========================================================================
	// Client attributes (protocol-agnostic)
	// ========================================================================
	AttrClientIP   = "client.ip"
	AttrClientAddr = "client.address"
	AttrClientPort = "client.port"
	AttrClientHost = "client.host"

	// ========================================================================
	// Protocol attributes (protocol-agnostic)
	// ========================================================================
	AttrProtocol   = "protocol.name"    // nfs, smb, webdav, etc.
	AttrOperation  = "fs.operation"     // Generic operation name
	AttrHandle     = "fs.handle"        // File handle (protocol-specific opaque ID)
	AttrShare      = "fs.share"         // Share/export name
	AttrPath       = "fs.path"          // File path
	AttrFilename   = "fs.filename"      // File name (basename)
	AttrOffset     = "fs.offset"        // I/O offset
	AttrCount      = "fs.count"         // Byte count requested
	AttrSize       = "fs.size"          // File size
	AttrType       = "fs.type"          // File type
	AttrMode       = "fs.mode"          // File mode/permissions
	AttrStatus     = "fs.status"        // Operation status code
	AttrStatusMsg  = "fs.status_msg"    // Human-readable status
	AttrEOF        = "fs.eof"           // End of file indicator
	AttrBytesRead  = "fs.bytes_read"    // Actual bytes read
	AttrBytesWrite = "fs.bytes_written" // Actual bytes written

	// ========================================================================
	// RPC attributes (NFS, RPC-based protocols)
	// ========================================================================
	AttrRPCXID      = "rpc.xid"
	AttrRPCProgram  = "rpc.program"
	AttrRPCVersion  = "rpc.version"
	AttrRPCAuthType = "rpc.auth_type"

	// ========================================================================
	// NFS-specific attributes
	// ========================================================================
	AttrNFSProcedure = "nfs.procedure"
	AttrNFSHandle    = "nfs.handle"
	AttrNFSShare     = "nfs.share"
	AttrNFSPath      = "nfs.path"
	AttrNFSFilename  = "nfs.filename"
	AttrNFSOffset    = "nfs.offset"
	AttrNFSCount     = "nfs.count"
	AttrNFSSize      = "nfs.size"
	AttrNFSType      = "nfs.type"
	AttrNFSMode      = "nfs.mode"
	AttrNFSStatus    = "nfs.status"
	AttrNFSEOF       = "nfs.eof"

	// ========================================================================
	// SMB-specific attributes (future)
	// ========================================================================
	AttrSMBCommand   = "smb.command"
	AttrSMBMessageID = "smb.message_id"
	AttrSMBSessionID = "smb.session_id"
	AttrSMBTreeID    = "smb.tree_id"
	AttrSMBFileID    = "smb.file_id"

	// ========================================================================
	// User/Auth attributes (protocol-agnostic)
	// ========================================================================
	AttrUID      = "user.uid"
	AttrGID      = "user.gid"
	AttrUsername = "user.name"
	AttrDomain   = "user.domain"
	AttrAuth     = "auth.method"

	// ========================================================================
	// Cache attributes
	// ========================================================================
	AttrCacheHit    = "cache.hit"
	AttrCacheSource = "cache.source"
	AttrCacheState  = "cache.state"
	AttrCacheSize   = "cache.size"

	// ========================================================================
	// Storage backend attributes
	// ========================================================================
	AttrContentID = "content.id"
	AttrStoreName = "store.name"
	AttrStoreType = "store.type"
	AttrBucket    = "storage.bucket"
	AttrContainer = "storage.container" // Azure Blob
	AttrKey       = "storage.key"
	AttrRegion    = "storage.region"
)

// Span names for operations.
// Format: <protocol>.<operation> for protocol-specific spans
// Format: <component>.<operation> for internal operations
const (
	// ========================================================================
	// NFS protocol spans
	// ========================================================================

	// Root span for NFS request processing
	SpanNFSRequest = "nfs.request"

	// NFS procedures
	SpanNFSNull        = "nfs.NULL"
	SpanNFSGetattr     = "nfs.GETATTR"
	SpanNFSSetattr     = "nfs.SETATTR"
	SpanNFSLookup      = "nfs.LOOKUP"
	SpanNFSAccess      = "nfs.ACCESS"
	SpanNFSReadlink    = "nfs.READLINK"
	SpanNFSRead        = "nfs.READ"
	SpanNFSWrite       = "nfs.WRITE"
	SpanNFSCreate      = "nfs.CREATE"
	SpanNFSMkdir       = "nfs.MKDIR"
	SpanNFSSymlink     = "nfs.SYMLINK"
	SpanNFSMknod       = "nfs.MKNOD"
	SpanNFSRemove      = "nfs.REMOVE"
	SpanNFSRmdir       = "nfs.RMDIR"
	SpanNFSRename      = "nfs.RENAME"
	SpanNFSLink        = "nfs.LINK"
	SpanNFSReaddir     = "nfs.READDIR"
	SpanNFSReaddirplus = "nfs.READDIRPLUS"
	SpanNFSFsstat      = "nfs.FSSTAT"
	SpanNFSFsinfo      = "nfs.FSINFO"
	SpanNFSPathconf    = "nfs.PATHCONF"
	SpanNFSCommit      = "nfs.COMMIT"

	// Mount procedures (NFS mount protocol)
	SpanMountNull    = "mount.NULL"
	SpanMountMnt     = "mount.MNT"
	SpanMountDump    = "mount.DUMP"
	SpanMountUmnt    = "mount.UMNT"
	SpanMountUmntall = "mount.UMNTALL"
	SpanMountExport  = "mount.EXPORT"

	// ========================================================================
	// SMB protocol spans (future)
	// ========================================================================
	SpanSMBRequest    = "smb.request"
	SpanSMBNegotiate  = "smb.NEGOTIATE"
	SpanSMBSessionSet = "smb.SESSION_SETUP"
	SpanSMBTreeConn   = "smb.TREE_CONNECT"
	SpanSMBCreate     = "smb.CREATE"
	SpanSMBClose      = "smb.CLOSE"
	SpanSMBRead       = "smb.READ"
	SpanSMBWrite      = "smb.WRITE"
	SpanSMBQueryDir   = "smb.QUERY_DIRECTORY"
	SpanSMBQueryInfo  = "smb.QUERY_INFO"
	SpanSMBSetInfo    = "smb.SET_INFO"

	// ========================================================================
	// Internal storage operations (protocol-agnostic)
	// ========================================================================
	SpanCacheLookup  = "cache.lookup"
	SpanCacheWrite   = "cache.write"
	SpanCacheFlush   = "cache.flush"
	SpanCacheEvict   = "cache.evict"
	SpanContentRead  = "content.read"
	SpanContentWrite = "content.write"
	SpanContentStat  = "content.stat"
	SpanMetaLookup   = "metadata.lookup"
	SpanMetaUpdate   = "metadata.update"
	SpanMetaCreate   = "metadata.create"
	SpanMetaDelete   = "metadata.delete"
)

// ClientIP returns an attribute for client IP address
func ClientIP(ip string) attribute.KeyValue {
	return attribute.String(AttrClientIP, ip)
}

// ClientAddr returns an attribute for full client address
func ClientAddr(addr string) attribute.KeyValue {
	return attribute.String(AttrClientAddr, addr)
}

// RPCXID returns an attribute for RPC transaction ID
func RPCXID(xid uint32) attribute.KeyValue {
	return attribute.Int64(AttrRPCXID, int64(xid))
}

// NFSProcedure returns an attribute for NFS procedure name
func NFSProcedure(name string) attribute.KeyValue {
	return attribute.String(AttrNFSProcedure, name)
}

// NFSHandle returns an attribute for file handle
func NFSHandle(handle []byte) attribute.KeyValue {
	return attribute.String(AttrNFSHandle, fmt.Sprintf("%x", handle))
}

// NFSHandleHex returns an attribute for file handle already in hex format
func NFSHandleHex(handle string) attribute.KeyValue {
	return attribute.String(AttrNFSHandle, handle)
}

// NFSShare returns an attribute for share name
func NFSShare(share string) attribute.KeyValue {
	return attribute.String(AttrNFSShare, share)
}

// NFSPath returns an attribute for file path
func NFSPath(path string) attribute.KeyValue {
	return attribute.String(AttrNFSPath, path)
}

// NFSFilename returns an attribute for filename
func NFSFilename(name string) attribute.KeyValue {
	return attribute.String(AttrNFSFilename, name)
}

// NFSOffset returns an attribute for file offset
func NFSOffset(offset uint64) attribute.KeyValue {
	return attribute.Int64(AttrNFSOffset, int64(offset))
}

// NFSCount returns an attribute for byte count
func NFSCount(count uint32) attribute.KeyValue {
	return attribute.Int64(AttrNFSCount, int64(count))
}

// NFSSize returns an attribute for file size
func NFSSize(size uint64) attribute.KeyValue {
	return attribute.Int64(AttrNFSSize, int64(size))
}

// NFSType returns an attribute for file type
func NFSType(t int) attribute.KeyValue {
	return attribute.Int(AttrNFSType, t)
}

// NFSMode returns an attribute for file mode
func NFSMode(mode uint32) attribute.KeyValue {
	return attribute.Int64(AttrNFSMode, int64(mode))
}

// NFSStatus returns an attribute for NFS status code
func NFSStatus(status int) attribute.KeyValue {
	return attribute.Int(AttrNFSStatus, status)
}

// NFSEOF returns an attribute for end-of-file indicator
func NFSEOF(eof bool) attribute.KeyValue {
	return attribute.Bool(AttrNFSEOF, eof)
}

// UID returns an attribute for user ID
func UID(uid uint32) attribute.KeyValue {
	return attribute.Int64(AttrUID, int64(uid))
}

// GID returns an attribute for group ID
func GID(gid uint32) attribute.KeyValue {
	return attribute.Int64(AttrGID, int64(gid))
}

// CacheHit returns an attribute for cache hit indicator
func CacheHit(hit bool) attribute.KeyValue {
	return attribute.Bool(AttrCacheHit, hit)
}

// CacheSource returns an attribute for cache source
func CacheSource(source string) attribute.KeyValue {
	return attribute.String(AttrCacheSource, source)
}

// ContentID returns an attribute for content ID
func ContentID(id string) attribute.KeyValue {
	return attribute.String(AttrContentID, id)
}

// Bucket returns an attribute for S3 bucket name
func Bucket(name string) attribute.KeyValue {
	return attribute.String(AttrBucket, name)
}

// StorageKey returns an attribute for S3 object key
func StorageKey(key string) attribute.KeyValue {
	return attribute.String(AttrKey, key)
}

// StartNFSSpan starts a span for an NFS procedure.
// This is a convenience function that sets common attributes.
func StartNFSSpan(ctx context.Context, procedure string, handle []byte, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	allAttrs := []attribute.KeyValue{
		NFSProcedure(procedure),
	}
	if len(handle) > 0 {
		allAttrs = append(allAttrs, NFSHandle(handle))
	}
	allAttrs = append(allAttrs, attrs...)

	return StartSpan(ctx, "nfs."+procedure, trace.WithAttributes(allAttrs...))
}

// StartContentSpan starts a span for a content store operation.
func StartContentSpan(ctx context.Context, operation string, contentID string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	allAttrs := []attribute.KeyValue{
		ContentID(contentID),
	}
	allAttrs = append(allAttrs, attrs...)

	return StartSpan(ctx, "content."+operation, trace.WithAttributes(allAttrs...))
}

// StartCacheSpan starts a span for a cache operation.
func StartCacheSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return StartSpan(ctx, "cache."+operation, trace.WithAttributes(attrs...))
}

// StartMetadataSpan starts a span for a metadata store operation.
func StartMetadataSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return StartSpan(ctx, "metadata."+operation, trace.WithAttributes(attrs...))
}

// ============================================================================
// Protocol-agnostic attribute helpers
// These can be used by any protocol adapter (NFS, SMB, WebDAV, etc.)
// ============================================================================

// Protocol returns an attribute for protocol name
func Protocol(name string) attribute.KeyValue {
	return attribute.String(AttrProtocol, name)
}

// FSOperation returns an attribute for filesystem operation name
func FSOperation(op string) attribute.KeyValue {
	return attribute.String(AttrOperation, op)
}

// FSHandle returns an attribute for file handle (generic)
func FSHandle(handle []byte) attribute.KeyValue {
	return attribute.String(AttrHandle, fmt.Sprintf("%x", handle))
}

// FSHandleHex returns an attribute for file handle already in hex format
func FSHandleHex(handle string) attribute.KeyValue {
	return attribute.String(AttrHandle, handle)
}

// FSShare returns an attribute for share/export name (generic)
func FSShare(share string) attribute.KeyValue {
	return attribute.String(AttrShare, share)
}

// FSPath returns an attribute for file path (generic)
func FSPath(path string) attribute.KeyValue {
	return attribute.String(AttrPath, path)
}

// FSFilename returns an attribute for filename (generic)
func FSFilename(name string) attribute.KeyValue {
	return attribute.String(AttrFilename, name)
}

// FSOffset returns an attribute for file offset (generic)
func FSOffset(offset uint64) attribute.KeyValue {
	return attribute.Int64(AttrOffset, int64(offset))
}

// FSCount returns an attribute for byte count (generic)
func FSCount(count uint32) attribute.KeyValue {
	return attribute.Int64(AttrCount, int64(count))
}

// FSSize returns an attribute for file size (generic)
func FSSize(size uint64) attribute.KeyValue {
	return attribute.Int64(AttrSize, int64(size))
}

// FSStatus returns an attribute for operation status (generic)
func FSStatus(status int) attribute.KeyValue {
	return attribute.Int(AttrStatus, status)
}

// FSStatusMsg returns an attribute for status message (generic)
func FSStatusMsg(msg string) attribute.KeyValue {
	return attribute.String(AttrStatusMsg, msg)
}

// FSEOF returns an attribute for end-of-file indicator (generic)
func FSEOF(eof bool) attribute.KeyValue {
	return attribute.Bool(AttrEOF, eof)
}

// Username returns an attribute for username
func Username(name string) attribute.KeyValue {
	return attribute.String(AttrUsername, name)
}

// Domain returns an attribute for domain name
func Domain(name string) attribute.KeyValue {
	return attribute.String(AttrDomain, name)
}

// AuthMethod returns an attribute for authentication method
func AuthMethod(method string) attribute.KeyValue {
	return attribute.String(AttrAuth, method)
}

// CacheState returns an attribute for cache state
func CacheState(state string) attribute.KeyValue {
	return attribute.String(AttrCacheState, state)
}

// StoreName returns an attribute for store name
func StoreName(name string) attribute.KeyValue {
	return attribute.String(AttrStoreName, name)
}

// StoreType returns an attribute for store type
func StoreType(t string) attribute.KeyValue {
	return attribute.String(AttrStoreType, t)
}

// Container returns an attribute for Azure container name
func Container(name string) attribute.KeyValue {
	return attribute.String(AttrContainer, name)
}

// Region returns an attribute for cloud region
func Region(region string) attribute.KeyValue {
	return attribute.String(AttrRegion, region)
}

// StartProtocolSpan starts a span for a generic protocol operation.
// Use this for new protocol adapters, passing the protocol name and operation.
func StartProtocolSpan(ctx context.Context, protocol, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	allAttrs := []attribute.KeyValue{
		Protocol(protocol),
		FSOperation(operation),
	}
	allAttrs = append(allAttrs, attrs...)

	return StartSpan(ctx, protocol+"."+operation, trace.WithAttributes(allAttrs...))
}
