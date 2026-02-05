package nlm

// ============================================================================
// NLM v4 XDR Types
// ============================================================================
//
// These types match the Open Group NLM v4 specification.
// All offsets and lengths are 64-bit (uint64) for large file support.

// NLM4Lock represents a lock identifier in NLM v4.
//
// Per Open Group specification:
//
//	struct nlm4_lock {
//	    string   caller_name<LM_MAXSTRLEN>;  // Client hostname
//	    netobj   fh;                         // File handle
//	    netobj   oh;                         // Owner handle (unique per lock owner)
//	    int      svid;                       // Server ID (process ID or unique ID)
//	    uint64   l_offset;                   // Start of lock range
//	    uint64   l_len;                      // Length of lock range (0 = to EOF)
//	};
//
// The combination of (caller_name, oh, svid) uniquely identifies a lock owner.
// This allows multiple processes on the same client to hold different locks.
type NLM4Lock struct {
	// CallerName is the hostname of the client holding the lock.
	// Used for crash recovery via NSM (Network Status Monitor).
	// Max length: 1024 bytes (LM_MAXSTRLEN).
	CallerName string

	// FH is the NFS file handle identifying the locked file.
	// This is an opaque byte array from the NFS layer.
	FH []byte

	// OH is the owner handle - a unique identifier for the lock owner.
	// Typically encodes process information on the client side.
	// Used to distinguish different lock owners on the same client.
	OH []byte

	// Svid is the server ID, typically the process ID of the lock holder.
	// Combined with CallerName and OH to uniquely identify the owner.
	Svid int32

	// Offset is the starting byte offset of the locked range.
	// NLM v4 uses 64-bit offsets for large file support.
	Offset uint64

	// Length is the number of bytes in the locked range.
	// Length of 0 means "lock to end of file" (entire remaining file).
	Length uint64
}

// NLM4Holder describes the holder of a conflicting lock.
//
// Per Open Group specification:
//
//	struct nlm4_holder {
//	    bool     exclusive;   // true = write lock, false = read lock
//	    int      svid;        // Server ID of holder
//	    netobj   oh;          // Owner handle of holder
//	    uint64   l_offset;    // Start of conflicting range
//	    uint64   l_len;       // Length of conflicting range
//	};
//
// Returned in NLM4TestRes when a TEST request finds a conflicting lock.
type NLM4Holder struct {
	// Exclusive indicates whether the conflicting lock is exclusive (write).
	// true = exclusive/write lock, false = shared/read lock.
	Exclusive bool

	// Svid is the server ID of the lock holder.
	Svid int32

	// OH is the owner handle of the lock holder.
	OH []byte

	// Offset is the starting byte offset of the conflicting lock.
	Offset uint64

	// Length is the length of the conflicting lock range.
	// 0 means the lock extends to end of file.
	Length uint64
}

// ============================================================================
// NLM v4 Request Arguments
// ============================================================================

// NLM4LockArgs contains arguments for NLM_LOCK procedure.
//
// Per Open Group specification:
//
//	struct nlm4_lockargs {
//	    netobj   cookie;      // Opaque identifier for async correlation
//	    bool     block;       // true = wait for lock, false = fail immediately
//	    bool     exclusive;   // true = write lock, false = read lock
//	    nlm4_lock alock;      // Lock to acquire
//	    bool     reclaim;     // true = reclaiming after server restart
//	    int      state;       // NSM state for crash recovery
//	};
type NLM4LockArgs struct {
	// Cookie is an opaque value used to correlate async requests/responses.
	// The server echoes this back in the response unchanged.
	Cookie []byte

	// Block indicates whether to block waiting for the lock.
	// If true and lock conflicts, server queues request and calls back via GRANTED.
	// If false and lock conflicts, server returns NLM4_DENIED immediately.
	Block bool

	// Exclusive indicates the lock type.
	// true = exclusive (write) lock - conflicts with all other locks on the range.
	// false = shared (read) lock - only conflicts with exclusive locks.
	Exclusive bool

	// Lock contains the lock parameters (file, owner, range).
	Lock NLM4Lock

	// Reclaim indicates this is a lock reclaim during grace period.
	// After server restart, clients have a grace period to reclaim their locks.
	// Reclaim requests bypass normal conflict checking.
	Reclaim bool

	// State is the NSM state counter for crash recovery correlation.
	// Used to detect client crashes and clean up stale locks.
	State int32
}

// NLM4UnlockArgs contains arguments for NLM_UNLOCK procedure.
//
// Per Open Group specification:
//
//	struct nlm4_unlockargs {
//	    netobj   cookie;      // Opaque identifier for async correlation
//	    nlm4_lock alock;      // Lock to release
//	};
type NLM4UnlockArgs struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Lock identifies the lock to release.
	// The lock owner (caller_name, oh, svid) and range must match exactly.
	Lock NLM4Lock
}

// NLM4TestArgs contains arguments for NLM_TEST procedure.
//
// Per Open Group specification:
//
//	struct nlm4_testargs {
//	    netobj   cookie;      // Opaque identifier for async correlation
//	    bool     exclusive;   // true = test for write lock, false = test for read lock
//	    nlm4_lock alock;      // Lock to test
//	};
//
// TEST checks if a lock could be granted without actually acquiring it.
// Used for F_GETLK fcntl() calls.
type NLM4TestArgs struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Exclusive indicates the lock type to test for.
	// true = would an exclusive lock succeed?
	// false = would a shared lock succeed?
	Exclusive bool

	// Lock contains the lock parameters to test.
	Lock NLM4Lock
}

// NLM4CancelArgs contains arguments for NLM_CANCEL procedure.
//
// Per Open Group specification:
//
//	struct nlm4_cancargs {
//	    netobj   cookie;      // Opaque identifier for async correlation
//	    bool     block;       // Must match the blocked request being cancelled
//	    bool     exclusive;   // Must match the blocked request being cancelled
//	    nlm4_lock alock;      // Lock request to cancel
//	};
//
// CANCEL removes a pending (blocked) lock request from the server's wait queue.
type NLM4CancelArgs struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Block must match the Block value from the original LOCK request.
	Block bool

	// Exclusive must match the Exclusive value from the original LOCK request.
	Exclusive bool

	// Lock identifies the pending lock request to cancel.
	Lock NLM4Lock
}

// NLM4GrantedArgs contains arguments for NLM_GRANTED callback.
//
// Per Open Group specification:
//
//	struct nlm4_testargs {
//	    netobj   cookie;      // Opaque identifier for async correlation
//	    bool     exclusive;   // Type of lock granted
//	    nlm4_lock alock;      // Lock that was granted
//	};
//
// GRANTED is a callback from server to client notifying that a previously
// blocked lock request has now been granted.
type NLM4GrantedArgs struct {
	// Cookie is an opaque value for correlation.
	Cookie []byte

	// Exclusive indicates the type of lock that was granted.
	Exclusive bool

	// Lock contains the granted lock details.
	Lock NLM4Lock
}

// NLM4FreeAllArgs contains arguments for NLM_FREE_ALL procedure.
//
// Per Open Group specification:
//
//	struct nlm_notify {
//	    string   name<LM_MAXSTRLEN>;  // Client hostname
//	    int      state;               // NSM state counter
//	};
//
// FREE_ALL releases all locks held by a client. Called by NSM when it
// detects that a client has crashed and restarted.
type NLM4FreeAllArgs struct {
	// Name is the hostname of the client whose locks should be freed.
	Name string

	// State is the new NSM state counter after the client restarted.
	State int32
}

// ============================================================================
// NLM v4 Response Structures
// ============================================================================

// NLM4Res is the common response structure for most NLM procedures.
//
// Per Open Group specification:
//
//	struct nlm4_res {
//	    netobj   cookie;      // Echoed from request
//	    nlm4_stat stat;       // Result status
//	};
//
// Used by: NLM_LOCK, NLM_UNLOCK, NLM_CANCEL, NLM_GRANTED
type NLM4Res struct {
	// Cookie is echoed from the request for async correlation.
	Cookie []byte

	// Status is the result of the operation.
	// See NLM4Granted, NLM4Denied, etc. constants.
	Status uint32
}

// NLM4TestRes is the response structure for NLM_TEST procedure.
//
// Per Open Group specification:
//
//	union nlm4_testrply switch (nlm4_stat stat) {
//	    case NLM4_DENIED:
//	        nlm4_holder holder;   // Info about conflicting lock
//	    default:
//	        void;
//	};
//
//	struct nlm4_testres {
//	    netobj   cookie;          // Echoed from request
//	    nlm4_testrply test_stat;  // Result
//	};
//
// If status is NLM4_DENIED, Holder contains info about the conflicting lock.
// If status is NLM4_GRANTED, Holder is nil (no conflict).
type NLM4TestRes struct {
	// Cookie is echoed from the request for async correlation.
	Cookie []byte

	// Status is the result of the test.
	// NLM4Granted means the lock would succeed.
	// NLM4Denied means there's a conflict (see Holder).
	Status uint32

	// Holder contains information about the conflicting lock.
	// Only populated when Status is NLM4Denied.
	// nil when Status is NLM4Granted (no conflict).
	Holder *NLM4Holder
}

// NLM4ShareArgs contains arguments for NLM_SHARE/NLM_UNSHARE procedures.
//
// Per Open Group specification (DOS share modes):
//
//	struct nlm4_share {
//	    string   caller_name<LM_MAXSTRLEN>;
//	    netobj   fh;
//	    netobj   oh;
//	    fsh4_mode mode;           // Share access mode
//	    fsh4_access access;       // Share deny mode
//	};
//
//	struct nlm4_shareargs {
//	    netobj   cookie;
//	    nlm4_share share;
//	    bool     reclaim;
//	};
//
// Note: Share modes are rarely used with modern NFS clients.
type NLM4ShareArgs struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// CallerName is the hostname of the client.
	CallerName string

	// FH is the NFS file handle.
	FH []byte

	// OH is the owner handle.
	OH []byte

	// Mode is the share access mode (read, write, read-write).
	Mode uint32

	// Access is the share deny mode (deny none, deny read, deny write, deny both).
	Access uint32

	// Reclaim indicates this is a share reclaim during grace period.
	Reclaim bool
}

// NLM4ShareRes is the response structure for NLM_SHARE/NLM_UNSHARE.
//
// Per Open Group specification:
//
//	struct nlm4_shareres {
//	    netobj   cookie;
//	    nlm4_stat stat;
//	    int      sequence;        // Sequence number for state tracking
//	};
type NLM4ShareRes struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	Status uint32

	// Sequence is a monotonically increasing counter for state tracking.
	Sequence int32
}

// ============================================================================
// Share Mode Constants (for DOS-style share locks)
// ============================================================================

const (
	// Share access modes (fsh4_mode)
	FSH4ModeRead      uint32 = 0 // Read access
	FSH4ModeWrite     uint32 = 1 // Write access
	FSH4ModeReadWrite uint32 = 2 // Read and write access

	// Share deny modes (fsh4_access)
	FSH4DenyNone  uint32 = 0 // Deny no access
	FSH4DenyRead  uint32 = 1 // Deny read access
	FSH4DenyWrite uint32 = 2 // Deny write access
	FSH4DenyBoth  uint32 = 3 // Deny all access
)

// ============================================================================
// Wire Format Constants
// ============================================================================

const (
	// LM_MAXSTRLEN is the maximum length for NLM strings (caller_name, etc.)
	LMMaxStrLen = 1024

	// Maximum length for opaque data (file handles, owner handles, cookies)
	MaxOpaqueLen = 1024
)
