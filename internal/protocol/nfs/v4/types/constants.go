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
// NFSv4.1 Operation Numbers (nfs_opnum4)
// ============================================================================
//
// Per RFC 8881 Section 18 (NFSv4.1 operations).
// 19 new operations added in NFSv4.1, ops 40-58.

const (
	OP_BACKCHANNEL_CTL      = 40
	OP_BIND_CONN_TO_SESSION = 41
	OP_EXCHANGE_ID          = 42
	OP_CREATE_SESSION       = 43
	OP_DESTROY_SESSION      = 44
	OP_FREE_STATEID         = 45
	OP_GET_DIR_DELEGATION   = 46
	OP_GETDEVICEINFO        = 47
	OP_GETDEVICELIST        = 48
	OP_LAYOUTCOMMIT         = 49
	OP_LAYOUTGET            = 50
	OP_LAYOUTRETURN         = 51
	OP_SECINFO_NO_NAME      = 52
	OP_SEQUENCE             = 53
	OP_SET_SSV              = 54
	OP_TEST_STATEID         = 55
	OP_WANT_DELEGATION      = 56
	OP_DESTROY_CLIENTID     = 57
	OP_RECLAIM_COMPLETE     = 58
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
	NFS4ERR_BADHANDLE           = 10001 // Illegal NFS file handle
	NFS4ERR_BAD_COOKIE          = 10003 // READDIR cookie is stale
	NFS4ERR_NOTSUPP             = 10004 // Operation not supported
	NFS4ERR_TOOSMALL            = 10005 // Buffer/response too small
	NFS4ERR_SERVERFAULT         = 10006 // Internal server error
	NFS4ERR_BADTYPE             = 10007 // Bad type for CREATE
	NFS4ERR_DELAY               = 10008 // Retry later (replaces JUKEBOX)
	NFS4ERR_SAME                = 10009 // VERIFY: attributes match
	NFS4ERR_DENIED              = 10010 // Lock denied
	NFS4ERR_EXPIRED             = 10011 // Lease/state expired
	NFS4ERR_LOCKED              = 10012 // File is locked
	NFS4ERR_GRACE               = 10013 // Grace period active
	NFS4ERR_FHEXPIRED           = 10014 // Volatile handle expired
	NFS4ERR_SHARE_DENIED        = 10015 // OPEN share mode conflict
	NFS4ERR_WRONGSEC            = 10016 // Wrong security flavor
	NFS4ERR_CLID_INUSE          = 10017 // Client ID in use
	NFS4ERR_RESOURCE            = 10018 // Server resource limit
	NFS4ERR_MOVED               = 10019 // Filesystem moved
	NFS4ERR_NOFILEHANDLE        = 10020 // No current filehandle
	NFS4ERR_MINOR_VERS_MISMATCH = 10021 // Minor version not supported
	NFS4ERR_STALE_CLIENTID      = 10022 // Client ID is stale
	NFS4ERR_STALE_STATEID       = 10023 // State ID is stale
	NFS4ERR_OLD_STATEID         = 10024 // State ID is outdated
	NFS4ERR_BAD_STATEID         = 10025 // Invalid state ID
	NFS4ERR_BAD_SEQID           = 10026 // Sequence ID mismatch
	NFS4ERR_NOT_SAME            = 10027 // NVERIFY: attributes differ
	NFS4ERR_LOCK_RANGE          = 10028 // Lock range not supported
	NFS4ERR_SYMLINK             = 10029 // Unexpected symlink
	NFS4ERR_RESTOREFH           = 10030 // No saved FH for RESTOREFH
	NFS4ERR_LEASE_MOVED         = 10031 // Lease moved to other server
	NFS4ERR_ATTRNOTSUPP         = 10032 // Attribute not supported
	NFS4ERR_NO_GRACE            = 10033 // No grace period available
	NFS4ERR_RECLAIM_BAD         = 10034 // Reclaim failed
	NFS4ERR_RECLAIM_CONFLICT    = 10035 // Reclaim conflict
	NFS4ERR_BADXDR              = 10036 // Malformed XDR data
	NFS4ERR_LOCKS_HELD          = 10037 // Cannot close, locks held
	NFS4ERR_OPENMODE            = 10038 // Wrong open mode for op
	NFS4ERR_BADOWNER            = 10039 // Invalid owner string
	NFS4ERR_BADCHAR             = 10040 // Invalid UTF-8 character
	NFS4ERR_BADNAME             = 10041 // Invalid filename
	NFS4ERR_BAD_RANGE           = 10042 // Invalid byte range
	NFS4ERR_LOCK_NOTSUPP        = 10043 // Lock type not supported
	NFS4ERR_OP_ILLEGAL          = 10044 // Unknown/illegal operation
	NFS4ERR_DEADLOCK            = 10045 // Lock deadlock detected
	NFS4ERR_FILE_OPEN           = 10046 // File is open
	NFS4ERR_ADMIN_REVOKED       = 10047 // Admin revoked access
	NFS4ERR_CB_PATH_DOWN        = 10048 // Callback path down
)

// --- NFSv4.1 Error Codes (RFC 8881 Section 15) ---

const (
	NFS4ERR_BADIOMODE                 = 10049 // Bad I/O mode for layout
	NFS4ERR_BADLAYOUT                 = 10050 // Bad layout specification
	NFS4ERR_BAD_SESSION_DIGEST        = 10051 // Bad session digest
	NFS4ERR_BADSESSION                = 10052 // Invalid session ID
	NFS4ERR_BADSLOT                   = 10053 // Invalid slot ID
	NFS4ERR_COMPLETE_ALREADY          = 10054 // Reclaim already completed
	NFS4ERR_CONN_NOT_BOUND_TO_SESSION = 10055 // Connection not bound to session
	NFS4ERR_DELEG_ALREADY_WANTED      = 10056 // Delegation already wanted
	NFS4ERR_BACK_CHAN_BUSY            = 10057 // Backchannel busy
	NFS4ERR_LAYOUTTRYLATER            = 10058 // Layout try again later
	NFS4ERR_LAYOUTUNAVAILABLE         = 10059 // Layout unavailable
	NFS4ERR_NOMATCHING_LAYOUT         = 10060 // No matching layout
	NFS4ERR_RECALLCONFLICT            = 10061 // Recall conflict
	NFS4ERR_UNKNOWN_LAYOUTTYPE        = 10062 // Unknown layout type
	NFS4ERR_SEQ_MISORDERED            = 10063 // Sequence misordered
	NFS4ERR_SEQUENCE_POS              = 10064 // SEQUENCE not first operation
	NFS4ERR_REQ_TOO_BIG               = 10065 // Request too big for session
	NFS4ERR_REP_TOO_BIG               = 10066 // Reply too big for session
	NFS4ERR_REP_TOO_BIG_TO_CACHE      = 10067 // Reply too big to cache
	NFS4ERR_RETRY_UNCACHED_REP        = 10068 // Retry uncached reply
	NFS4ERR_UNSAFE_COMPOUND           = 10069 // Unsafe compound request
	NFS4ERR_TOO_MANY_OPS              = 10070 // Too many operations
	NFS4ERR_OP_NOT_IN_SESSION         = 10071 // Op not in session
	NFS4ERR_HASH_ALG_UNSUPP           = 10072 // Hash algorithm unsupported
	// 10073 intentionally skipped (no error code assigned)
	NFS4ERR_CLIENTID_BUSY    = 10074 // Client ID busy
	NFS4ERR_PNFS_IO_HOLE     = 10075 // pNFS I/O hole
	NFS4ERR_SEQ_FALSE_RETRY  = 10076 // Sequence false retry
	NFS4ERR_BAD_HIGH_SLOT    = 10077 // Bad highest slot
	NFS4ERR_DEADSESSION      = 10078 // Dead session
	NFS4ERR_ENCR_ALG_UNSUPP  = 10079 // Encryption algorithm unsupported
	NFS4ERR_PNFS_NO_LAYOUT   = 10080 // pNFS no layout
	NFS4ERR_NOT_ONLY_OP      = 10081 // Not the only operation
	NFS4ERR_WRONG_CRED       = 10082 // Wrong credentials
	NFS4ERR_WRONG_TYPE       = 10083 // Wrong type
	NFS4ERR_DIRDELEG_UNAVAIL = 10084 // Directory delegation unavailable
	NFS4ERR_REJECT_DELEG     = 10085 // Reject delegation
	NFS4ERR_RETURNCONFLICT   = 10086 // Return conflict
	NFS4ERR_DELEG_REVOKED    = 10087 // Delegation revoked
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

// --- NFSv4.1 Callback Operation Numbers (RFC 8881 Section 20) ---

const (
	CB_LAYOUTRECALL         uint32 = 5
	CB_NOTIFY               uint32 = 6
	CB_PUSH_DELEG           uint32 = 7
	CB_RECALL_ANY           uint32 = 8
	CB_RECALLABLE_OBJ_AVAIL uint32 = 9
	CB_RECALL_SLOT          uint32 = 10
	CB_SEQUENCE             uint32 = 11
	CB_WANTS_CANCELLED      uint32 = 12
	CB_NOTIFY_LOCK          uint32 = 13
	CB_NOTIFY_DEVICEID      uint32 = 14
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

// ============================================================================
// NFSv4.1 Minor Version
// ============================================================================

const (
	// NFS4_MINOR_VERSION_1 indicates NFSv4.1 per RFC 8881.
	NFS4_MINOR_VERSION_1 = 1
)

// ============================================================================
// NFSv4.1 Session Constants
// ============================================================================

const (
	// NFS4_SESSIONID_SIZE is the size of a session identifier (16 bytes).
	// Per RFC 8881 Section 2.10.3.
	NFS4_SESSIONID_SIZE = 16
)

// ============================================================================
// NFSv4.1 EXCHANGE_ID Flags (RFC 8881 Section 18.35)
// ============================================================================

const (
	EXCHGID4_FLAG_SUPP_MOVED_REFER    = 0x00000001
	EXCHGID4_FLAG_SUPP_MOVED_MIGR     = 0x00000002
	EXCHGID4_FLAG_SUPP_FENCE_OPS      = 0x00000004
	EXCHGID4_FLAG_BIND_PRINC_STATEID  = 0x00000100
	EXCHGID4_FLAG_USE_NON_PNFS        = 0x00010000
	EXCHGID4_FLAG_USE_PNFS_MDS        = 0x00020000
	EXCHGID4_FLAG_USE_PNFS_DS         = 0x00040000
	EXCHGID4_FLAG_UPD_CONFIRMED_REC_A = 0x40000000
	EXCHGID4_FLAG_CONFIRMED_R         = 0x80000000
	EXCHGID4_FLAG_MASK_A              = 0x40070103
	EXCHGID4_FLAG_MASK_R              = 0x80070103
)

// ============================================================================
// NFSv4.1 CREATE_SESSION Flags (RFC 8881 Section 18.36)
// ============================================================================

const (
	CREATE_SESSION4_FLAG_PERSIST        = 0x00000001
	CREATE_SESSION4_FLAG_CONN_BACK_CHAN = 0x00000002
	CREATE_SESSION4_FLAG_CONN_RDMA      = 0x00000004
)

// ============================================================================
// NFSv4.1 State Protection (RFC 8881 Section 18.35)
// ============================================================================

const (
	SP4_NONE      = 0
	SP4_MACH_CRED = 1
	SP4_SSV       = 2
)

// ============================================================================
// NFSv4.1 Channel Direction (RFC 8881 Section 18.36/18.34)
// ============================================================================

const (
	CDFC4_FORE         = 0x1
	CDFC4_BACK         = 0x2
	CDFC4_FORE_OR_BOTH = 0x3
	CDFC4_BACK_OR_BOTH = 0x7

	CDFS4_FORE = 0x1
	CDFS4_BACK = 0x2
	CDFS4_BOTH = 0x3
)

// ============================================================================
// NFSv4.1 SEQUENCE Status Flags (RFC 8881 Section 18.46)
// ============================================================================

const (
	SEQ4_STATUS_CB_PATH_DOWN               = 0x00000001
	SEQ4_STATUS_CB_GSS_CONTEXTS_EXPIRING   = 0x00000002
	SEQ4_STATUS_CB_GSS_CONTEXTS_EXPIRED    = 0x00000004
	SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED  = 0x00000008
	SEQ4_STATUS_EXPIRED_SOME_STATE_REVOKED = 0x00000010
	SEQ4_STATUS_ADMIN_STATE_REVOKED        = 0x00000020
	SEQ4_STATUS_RECALLABLE_STATE_REVOKED   = 0x00000040
	SEQ4_STATUS_LEASE_MOVED                = 0x00000080
	SEQ4_STATUS_RESTART_RECLAIM_NEEDED     = 0x00000100
	SEQ4_STATUS_CB_PATH_DOWN_SESSION       = 0x00000200
	SEQ4_STATUS_BACKCHANNEL_FAULT          = 0x00000400
	SEQ4_STATUS_DEVID_CHANGED              = 0x00000800
	SEQ4_STATUS_DEVID_DELETED              = 0x00001000
)

// ============================================================================
// NFSv4.1 Layout Types (RFC 8881 Section 3.3.13)
// ============================================================================

const (
	LAYOUT4_NFSV4_1_FILES = 1
	LAYOUT4_OSD2_OBJECTS  = 2
	LAYOUT4_BLOCK_VOLUME  = 3
)

// ============================================================================
// NFSv4.1 SECINFO_NO_NAME Style (RFC 8881 Section 18.45)
// ============================================================================

const (
	SECINFO_STYLE4_CURRENT_FH = 0
	SECINFO_STYLE4_PARENT     = 1
)

// ============================================================================
// NFSv4.1 Notification Types (RFC 8881 Section 20.4)
// ============================================================================

const (
	NOTIFY4_CHANGE_CHILD_ATTRS     = 0
	NOTIFY4_CHANGE_DIR_ATTRS       = 1
	NOTIFY4_REMOVE_ENTRY           = 2
	NOTIFY4_ADD_ENTRY              = 3
	NOTIFY4_RENAME_ENTRY           = 4
	NOTIFY4_CHANGE_COOKIE_VERIFIER = 5
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
	// --- NFSv4.1 Operations ---
	case OP_BACKCHANNEL_CTL:
		return "BACKCHANNEL_CTL"
	case OP_BIND_CONN_TO_SESSION:
		return "BIND_CONN_TO_SESSION"
	case OP_EXCHANGE_ID:
		return "EXCHANGE_ID"
	case OP_CREATE_SESSION:
		return "CREATE_SESSION"
	case OP_DESTROY_SESSION:
		return "DESTROY_SESSION"
	case OP_FREE_STATEID:
		return "FREE_STATEID"
	case OP_GET_DIR_DELEGATION:
		return "GET_DIR_DELEGATION"
	case OP_GETDEVICEINFO:
		return "GETDEVICEINFO"
	case OP_GETDEVICELIST:
		return "GETDEVICELIST"
	case OP_LAYOUTCOMMIT:
		return "LAYOUTCOMMIT"
	case OP_LAYOUTGET:
		return "LAYOUTGET"
	case OP_LAYOUTRETURN:
		return "LAYOUTRETURN"
	case OP_SECINFO_NO_NAME:
		return "SECINFO_NO_NAME"
	case OP_SEQUENCE:
		return "SEQUENCE"
	case OP_SET_SSV:
		return "SET_SSV"
	case OP_TEST_STATEID:
		return "TEST_STATEID"
	case OP_WANT_DELEGATION:
		return "WANT_DELEGATION"
	case OP_DESTROY_CLIENTID:
		return "DESTROY_CLIENTID"
	case OP_RECLAIM_COMPLETE:
		return "RECLAIM_COMPLETE"
	default:
		return "UNKNOWN"
	}
}

// CbOpName returns a human-readable name for an NFSv4 callback operation number.
func CbOpName(op uint32) string {
	switch op {
	case OP_CB_GETATTR:
		return "CB_GETATTR"
	case OP_CB_RECALL:
		return "CB_RECALL"
	// --- NFSv4.1 Callback Operations ---
	case CB_LAYOUTRECALL:
		return "CB_LAYOUTRECALL"
	case CB_NOTIFY:
		return "CB_NOTIFY"
	case CB_PUSH_DELEG:
		return "CB_PUSH_DELEG"
	case CB_RECALL_ANY:
		return "CB_RECALL_ANY"
	case CB_RECALLABLE_OBJ_AVAIL:
		return "CB_RECALLABLE_OBJ_AVAIL"
	case CB_RECALL_SLOT:
		return "CB_RECALL_SLOT"
	case CB_SEQUENCE:
		return "CB_SEQUENCE"
	case CB_WANTS_CANCELLED:
		return "CB_WANTS_CANCELLED"
	case CB_NOTIFY_LOCK:
		return "CB_NOTIFY_LOCK"
	case CB_NOTIFY_DEVICEID:
		return "CB_NOTIFY_DEVICEID"
	default:
		return "CB_UNKNOWN"
	}
}

// opNameToNum maps human-readable operation names to their numeric constants.
// Populated by init() from the OpName reverse mapping.
var opNameToNum map[string]uint32

func init() {
	opNameToNum = map[string]uint32{
		"ACCESS":              OP_ACCESS,
		"CLOSE":               OP_CLOSE,
		"COMMIT":              OP_COMMIT,
		"CREATE":              OP_CREATE,
		"DELEGPURGE":          OP_DELEGPURGE,
		"DELEGRETURN":         OP_DELEGRETURN,
		"GETATTR":             OP_GETATTR,
		"GETFH":               OP_GETFH,
		"LINK":                OP_LINK,
		"LOCK":                OP_LOCK,
		"LOCKT":               OP_LOCKT,
		"LOCKU":               OP_LOCKU,
		"LOOKUP":              OP_LOOKUP,
		"LOOKUPP":             OP_LOOKUPP,
		"NVERIFY":             OP_NVERIFY,
		"OPEN":                OP_OPEN,
		"OPENATTR":            OP_OPENATTR,
		"OPEN_CONFIRM":        OP_OPEN_CONFIRM,
		"OPEN_DOWNGRADE":      OP_OPEN_DOWNGRADE,
		"PUTFH":               OP_PUTFH,
		"PUTPUBFH":            OP_PUTPUBFH,
		"PUTROOTFH":           OP_PUTROOTFH,
		"READ":                OP_READ,
		"READDIR":             OP_READDIR,
		"READLINK":            OP_READLINK,
		"REMOVE":              OP_REMOVE,
		"RENAME":              OP_RENAME,
		"RENEW":               OP_RENEW,
		"RESTOREFH":           OP_RESTOREFH,
		"SAVEFH":              OP_SAVEFH,
		"SECINFO":             OP_SECINFO,
		"SETATTR":             OP_SETATTR,
		"SETCLIENTID":         OP_SETCLIENTID,
		"SETCLIENTID_CONFIRM": OP_SETCLIENTID_CONFIRM,
		"VERIFY":              OP_VERIFY,
		"WRITE":               OP_WRITE,
		"RELEASE_LOCKOWNER":   OP_RELEASE_LOCKOWNER,

		// --- NFSv4.1 Operations ---
		"BACKCHANNEL_CTL":      OP_BACKCHANNEL_CTL,
		"BIND_CONN_TO_SESSION": OP_BIND_CONN_TO_SESSION,
		"EXCHANGE_ID":          OP_EXCHANGE_ID,
		"CREATE_SESSION":       OP_CREATE_SESSION,
		"DESTROY_SESSION":      OP_DESTROY_SESSION,
		"FREE_STATEID":         OP_FREE_STATEID,
		"GET_DIR_DELEGATION":   OP_GET_DIR_DELEGATION,
		"GETDEVICEINFO":        OP_GETDEVICEINFO,
		"GETDEVICELIST":        OP_GETDEVICELIST,
		"LAYOUTCOMMIT":         OP_LAYOUTCOMMIT,
		"LAYOUTGET":            OP_LAYOUTGET,
		"LAYOUTRETURN":         OP_LAYOUTRETURN,
		"SECINFO_NO_NAME":      OP_SECINFO_NO_NAME,
		"SEQUENCE":             OP_SEQUENCE,
		"SET_SSV":              OP_SET_SSV,
		"TEST_STATEID":         OP_TEST_STATEID,
		"WANT_DELEGATION":      OP_WANT_DELEGATION,
		"DESTROY_CLIENTID":     OP_DESTROY_CLIENTID,
		"RECLAIM_COMPLETE":     OP_RECLAIM_COMPLETE,
	}
}

// OpNameToNum converts a human-readable operation name to its numeric constant.
// Returns the operation number and true if found, or (0, false) if the name is
// not recognised. Used by Handler.SetBlockedOps to translate string-based
// blocklists from the control plane into numeric lookup tables.
func OpNameToNum(name string) (uint32, bool) {
	num, ok := opNameToNum[name]
	return num, ok
}
