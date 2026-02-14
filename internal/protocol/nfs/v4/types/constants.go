// Package types defines NFSv4 constants, error codes, and core structures
// per RFC 7530 (NFSv4.0 protocol specification) and RFC 7531 (NFSv4.0 XDR).
//
// This is a foundational package with no runtime behavior -- only data
// definitions used by the COMPOUND dispatcher and operation handlers.
package types

// ============================================================================
// RPC Procedure Numbers (NFSv4)
// ============================================================================

// NFSv4 has only two RPC procedures per RFC 7530 Section 16.
const (
	// NFSPROC4_NULL is the null/ping procedure (procedure 0).
	NFSPROC4_NULL = 0

	// NFSPROC4_COMPOUND is the only real procedure -- bundles multiple
	// operations into a single RPC call (procedure 1).
	NFSPROC4_COMPOUND = 1
)

// ============================================================================
// Protocol Limits
// ============================================================================

const (
	// NFS4_FHSIZE is the maximum file handle size in bytes (RFC 7530).
	// NFSv4 increased this from 64 bytes (NFSv3) to 128 bytes.
	NFS4_FHSIZE = 128

	// NFS4_MINOR_VERSION_0 is the only minor version supported in this phase.
	NFS4_MINOR_VERSION_0 = 0

	// MaxCompoundOps limits the number of operations in a single COMPOUND
	// request to prevent memory exhaustion. Similar to Linux nfsd limits.
	MaxCompoundOps = 128
)

// ============================================================================
// File Handle Expire Type (fh_expire_type4)
// ============================================================================

const (
	// FH4_PERSISTENT indicates file handles are persistent across server restarts.
	FH4_PERSISTENT = 0x00

	// FH4_VOLATILE_ANY indicates file handles may expire at any time.
	FH4_VOLATILE_ANY = 0x01
)

// ============================================================================
// NFSv4 File Type Constants (nfs_ftype4)
// ============================================================================
//
// Per RFC 7530 Section 3.3.13 (enum nfs_ftype4).

const (
	NF4REG       = 1 // Regular file
	NF4DIR       = 2 // Directory
	NF4BLK       = 3 // Block device
	NF4CHR       = 4 // Character device
	NF4LNK       = 5 // Symbolic link
	NF4SOCK      = 6 // Socket
	NF4FIFO      = 7 // Named pipe (FIFO)
	NF4ATTRDIR   = 8 // Attribute directory
	NF4NAMEDATTR = 9 // Named attribute
)

// ============================================================================
// NFSv4 Operation Numbers (nfs_opnum4)
// ============================================================================
//
// Per RFC 7530 Section 16.1 / RFC 7531 (enum nfs_opnum4).
// All 40 NFSv4.0 operations plus OP_ILLEGAL.

const (
	OP_ACCESS              = 3
	OP_CLOSE               = 4
	OP_COMMIT              = 5
	OP_CREATE              = 6
	OP_DELEGPURGE          = 7
	OP_DELEGRETURN         = 8
	OP_GETATTR             = 9
	OP_GETFH               = 10
	OP_LINK                = 11
	OP_LOCK                = 12
	OP_LOCKT               = 13
	OP_LOCKU               = 14
	OP_LOOKUP              = 15
	OP_LOOKUPP             = 16
	OP_NVERIFY             = 17
	OP_OPEN                = 18
	OP_OPENATTR            = 19
	OP_OPEN_CONFIRM        = 20
	OP_OPEN_DOWNGRADE      = 21
	OP_PUTFH               = 22
	OP_PUTPUBFH            = 23
	OP_PUTROOTFH           = 24
	OP_READ                = 25
	OP_READDIR             = 26
	OP_READLINK            = 27
	OP_REMOVE              = 28
	OP_RENAME              = 29
	OP_RENEW               = 30
	OP_RESTOREFH           = 31
	OP_SAVEFH              = 32
	OP_SECINFO             = 33
	OP_SETATTR             = 34
	OP_SETCLIENTID         = 35
	OP_SETCLIENTID_CONFIRM = 36
	OP_VERIFY              = 37
	OP_WRITE               = 38
	OP_RELEASE_LOCKOWNER   = 39

	// OP_ILLEGAL is used for unknown/illegal operations (must return NFS4ERR_OP_ILLEGAL).
	OP_ILLEGAL = 10044
)

// ============================================================================
// NFSv4 Error Codes (nfsstat4)
// ============================================================================
//
// Per RFC 7530 Section 13 (enum nfsstat4).
// Complete set of NFSv4.0 error codes with exact values.

const (
	NFS4_OK = 0 // Success

	// POSIX-derived error codes (same values as NFSv3 equivalents)
	NFS4ERR_PERM        = 1  // Not owner (EPERM)
	NFS4ERR_NOENT       = 2  // No such file or directory (ENOENT)
	NFS4ERR_IO          = 5  // I/O error (EIO)
	NFS4ERR_NXIO        = 6  // No such device or address (ENXIO)
	NFS4ERR_ACCESS      = 13 // Permission denied (EACCES)
	NFS4ERR_EXIST       = 17 // File exists (EEXIST)
	NFS4ERR_XDEV        = 18 // Cross-device link (EXDEV)
	NFS4ERR_NOTDIR      = 20 // Not a directory (ENOTDIR)
	NFS4ERR_ISDIR       = 21 // Is a directory (EISDIR)
	NFS4ERR_INVAL       = 22 // Invalid argument (EINVAL)
	NFS4ERR_FBIG        = 27 // File too large (EFBIG)
	NFS4ERR_NOSPC       = 28 // No space left on device (ENOSPC)
	NFS4ERR_ROFS        = 30 // Read-only filesystem (EROFS)
	NFS4ERR_MLINK       = 31 // Too many links (EMLINK)
	NFS4ERR_NAMETOOLONG = 63 // Filename too long (ENAMETOOLONG)
	NFS4ERR_NOTEMPTY    = 66 // Directory not empty (ENOTEMPTY)
	NFS4ERR_DQUOT       = 69 // Disk quota exceeded (EDQUOT)
	NFS4ERR_STALE       = 70 // Stale file handle (ESTALE)

	// NFSv4-specific error codes (10000+ range)
	NFS4ERR_BADHANDLE            = 10001 // Illegal NFS file handle
	NFS4ERR_BAD_COOKIE           = 10003 // READDIR cookie is stale
	NFS4ERR_NOTSUPP              = 10004 // Operation not supported
	NFS4ERR_TOOSMALL             = 10005 // Buffer/response too small
	NFS4ERR_SERVERFAULT          = 10006 // Internal server error
	NFS4ERR_BADTYPE              = 10007 // Bad type for CREATE
	NFS4ERR_DELAY                = 10008 // Retry later (replaces JUKEBOX)
	NFS4ERR_SAME                 = 10009 // VERIFY: attributes match
	NFS4ERR_DENIED               = 10010 // Lock denied
	NFS4ERR_EXPIRED              = 10011 // Lease/state expired
	NFS4ERR_LOCKED               = 10012 // File is locked
	NFS4ERR_GRACE                = 10013 // Grace period active
	NFS4ERR_FHEXPIRED            = 10014 // Volatile handle expired
	NFS4ERR_SHARE_DENIED         = 10015 // OPEN share mode conflict
	NFS4ERR_WRONGSEC             = 10016 // Wrong security flavor
	NFS4ERR_CLID_INUSE           = 10017 // Client ID in use
	NFS4ERR_RESOURCE             = 10018 // Server resource limit
	NFS4ERR_MOVED                = 10019 // Filesystem moved
	NFS4ERR_NOFILEHANDLE         = 10020 // No current filehandle
	NFS4ERR_MINOR_VERS_MISMATCH  = 10021 // Minor version not supported
	NFS4ERR_STALE_CLIENTID       = 10022 // Client ID is stale
	NFS4ERR_STALE_STATEID        = 10023 // State ID is stale
	NFS4ERR_OLD_STATEID          = 10024 // State ID is outdated
	NFS4ERR_BAD_STATEID          = 10025 // Invalid state ID
	NFS4ERR_BAD_SEQID            = 10026 // Sequence ID mismatch
	NFS4ERR_NOT_SAME             = 10027 // NVERIFY: attributes differ
	NFS4ERR_LOCK_RANGE           = 10028 // Lock range not supported
	NFS4ERR_SYMLINK              = 10029 // Unexpected symlink
	NFS4ERR_RESTOREFH            = 10030 // No saved FH for RESTOREFH
	NFS4ERR_LEASE_MOVED          = 10031 // Lease moved to other server
	NFS4ERR_ATTRNOTSUPP          = 10032 // Attribute not supported
	NFS4ERR_NO_GRACE             = 10033 // No grace period available
	NFS4ERR_RECLAIM_BAD          = 10034 // Reclaim failed
	NFS4ERR_RECLAIM_CONFLICT     = 10035 // Reclaim conflict
	NFS4ERR_BADXDR               = 10036 // Malformed XDR data
	NFS4ERR_LOCKS_HELD           = 10037 // Cannot close, locks held
	NFS4ERR_OPENMODE             = 10038 // Wrong open mode for op
	NFS4ERR_BADOWNER             = 10039 // Invalid owner string
	NFS4ERR_BADCHAR              = 10040 // Invalid UTF-8 character
	NFS4ERR_BADNAME              = 10041 // Invalid filename
	NFS4ERR_BAD_RANGE            = 10042 // Invalid byte range
	NFS4ERR_LOCK_NOTSUPP         = 10043 // Lock type not supported
	NFS4ERR_OP_ILLEGAL           = 10044 // Unknown/illegal operation
	NFS4ERR_DEADLOCK             = 10045 // Lock deadlock detected
	NFS4ERR_FILE_OPEN            = 10046 // File is open
	NFS4ERR_ADMIN_REVOKED        = 10047 // Admin revoked access
	NFS4ERR_CB_PATH_DOWN         = 10048 // Callback path down
)

// ============================================================================
// createtype4 Values for CREATE Operation (RFC 7530 Section 16.4)
// ============================================================================
//
// Note: NF4REG is NOT valid for CREATE -- regular files are created via OPEN.
// The createtype4 values reuse the nfs_ftype4 enum values.

const (
	CREATETYPE4_DIR  = NF4DIR  // Directory
	CREATETYPE4_BLK  = NF4BLK  // Block device
	CREATETYPE4_CHR  = NF4CHR  // Character device
	CREATETYPE4_LNK  = NF4LNK  // Symbolic link
	CREATETYPE4_SOCK = NF4SOCK // Socket
	CREATETYPE4_FIFO = NF4FIFO // Named pipe (FIFO)
)

// ============================================================================
// createmode4 Values (RFC 7530 Section 16.16)
// ============================================================================
//
// Specifies how attributes are set during creation (used by OPEN with CREATE).

const (
	UNCHECKED4 = 0 // Set attrs; if exists, no error
	GUARDED4   = 1 // Set attrs; if exists, error
	EXCLUSIVE4 = 2 // Use verifier for exclusive create
)

// ============================================================================
// OPEN Constants (RFC 7530 Section 16.16)
// ============================================================================

// OPEN share_access/share_deny constants (RFC 7530 Section 16.16.2)
const (
	OPEN4_SHARE_ACCESS_READ  = 0x01
	OPEN4_SHARE_ACCESS_WRITE = 0x02
	OPEN4_SHARE_ACCESS_BOTH  = 0x03

	OPEN4_SHARE_DENY_NONE  = 0x00
	OPEN4_SHARE_DENY_READ  = 0x01
	OPEN4_SHARE_DENY_WRITE = 0x02
	OPEN4_SHARE_DENY_BOTH  = 0x03
)

// OPEN create type
const (
	OPEN4_NOCREATE = 0
	OPEN4_CREATE   = 1
)

// OPEN claim types (RFC 7530 Section 16.16.4)
const (
	CLAIM_NULL          = 0
	CLAIM_PREVIOUS      = 1
	CLAIM_DELEGATE_CUR  = 2
	CLAIM_DELEGATE_PREV = 3
)

// OPEN result flags (rflags)
const (
	OPEN4_RESULT_CONFIRM        = 0x02
	OPEN4_RESULT_LOCKTYPE_POSIX = 0x04
)

// OPEN delegation types
const (
	OPEN_DELEGATE_NONE  = 0
	OPEN_DELEGATE_READ  = 1
	OPEN_DELEGATE_WRITE = 2
)

// ============================================================================
// Lock Type Constants (nfs_lock_type4) per RFC 7530/7531
// ============================================================================
//
// Used by LOCK, LOCKT, and LOCKU operations. The "W" variants are blocking
// hints from the client; the server does NOT block -- it returns NFS4ERR_DENIED.

const (
	READ_LT   = 1 // Shared (read) lock
	WRITE_LT  = 2 // Exclusive (write) lock
	READW_LT  = 3 // Blocking read lock (hint; server does NOT block)
	WRITEW_LT = 4 // Blocking write lock (hint; server does NOT block)
)

// ============================================================================
// Callback Operation Numbers (for CB_COMPOUND)
// ============================================================================
//
// Per RFC 7530 Section 16.4 (CB_COMPOUND operation encoding).

const (
	// OP_CB_GETATTR retrieves attributes from the client (write delegations).
	OP_CB_GETATTR uint32 = 3

	// OP_CB_RECALL recalls a delegation from the client.
	OP_CB_RECALL uint32 = 4
)

// ============================================================================
// Callback Program and Version
// ============================================================================
//
// Per RFC 7530 Section 16.33 (NFS4_CALLBACK program).
// Note: The actual program number is client-specified via SETCLIENTID.

const (
	// NFS4_CALLBACK_VERSION is the callback RPC version.
	NFS4_CALLBACK_VERSION uint32 = 1

	// CB_PROC_NULL is the callback null/ping procedure.
	CB_PROC_NULL uint32 = 0

	// CB_PROC_COMPOUND is the callback compound procedure.
	CB_PROC_COMPOUND uint32 = 1
)

// ============================================================================
// ACE Constants for Delegation Permissions (nfsace4)
// ============================================================================
//
// Per RFC 7530 Section 6.2 (acetype4, acemask4).
// Used in open_read_delegation4 and open_write_delegation4.

const (
	// ACE4_ACCESS_ALLOWED_ACE_TYPE allows the specified access.
	ACE4_ACCESS_ALLOWED_ACE_TYPE uint32 = 0

	// ACE4_GENERIC_READ is the generic read access mask.
	ACE4_GENERIC_READ uint32 = 0x00120081

	// ACE4_GENERIC_WRITE is the generic write access mask.
	ACE4_GENERIC_WRITE uint32 = 0x00160106
)

// ============================================================================
// Space Limit Constants (nfs_space_limit4)
// ============================================================================
//
// Per RFC 7530 Section 16.16 (open_write_delegation4).

const (
	// NFS_LIMIT_SIZE limits by total file size.
	NFS_LIMIT_SIZE uint32 = 1

	// NFS_LIMIT_BLOCKS limits by number of modified blocks.
	NFS_LIMIT_BLOCKS uint32 = 2
)

// ============================================================================
// Write Stability Levels (stable_how4) for NFSv4
// ============================================================================

const (
	UNSTABLE4  = 0
	DATA_SYNC4 = 1
	FILE_SYNC4 = 2
)

// OpName returns a human-readable name for an NFSv4 operation number.
func OpName(op uint32) string {
	switch op {
	case OP_ACCESS:
		return "ACCESS"
	case OP_CLOSE:
		return "CLOSE"
	case OP_COMMIT:
		return "COMMIT"
	case OP_CREATE:
		return "CREATE"
	case OP_DELEGPURGE:
		return "DELEGPURGE"
	case OP_DELEGRETURN:
		return "DELEGRETURN"
	case OP_GETATTR:
		return "GETATTR"
	case OP_GETFH:
		return "GETFH"
	case OP_LINK:
		return "LINK"
	case OP_LOCK:
		return "LOCK"
	case OP_LOCKT:
		return "LOCKT"
	case OP_LOCKU:
		return "LOCKU"
	case OP_LOOKUP:
		return "LOOKUP"
	case OP_LOOKUPP:
		return "LOOKUPP"
	case OP_NVERIFY:
		return "NVERIFY"
	case OP_OPEN:
		return "OPEN"
	case OP_OPENATTR:
		return "OPENATTR"
	case OP_OPEN_CONFIRM:
		return "OPEN_CONFIRM"
	case OP_OPEN_DOWNGRADE:
		return "OPEN_DOWNGRADE"
	case OP_PUTFH:
		return "PUTFH"
	case OP_PUTPUBFH:
		return "PUTPUBFH"
	case OP_PUTROOTFH:
		return "PUTROOTFH"
	case OP_READ:
		return "READ"
	case OP_READDIR:
		return "READDIR"
	case OP_READLINK:
		return "READLINK"
	case OP_REMOVE:
		return "REMOVE"
	case OP_RENAME:
		return "RENAME"
	case OP_RENEW:
		return "RENEW"
	case OP_RESTOREFH:
		return "RESTOREFH"
	case OP_SAVEFH:
		return "SAVEFH"
	case OP_SECINFO:
		return "SECINFO"
	case OP_SETATTR:
		return "SETATTR"
	case OP_SETCLIENTID:
		return "SETCLIENTID"
	case OP_SETCLIENTID_CONFIRM:
		return "SETCLIENTID_CONFIRM"
	case OP_VERIFY:
		return "VERIFY"
	case OP_WRITE:
		return "WRITE"
	case OP_RELEASE_LOCKOWNER:
		return "RELEASE_LOCKOWNER"
	case OP_ILLEGAL:
		return "ILLEGAL"
	default:
		return "UNKNOWN"
	}
}
