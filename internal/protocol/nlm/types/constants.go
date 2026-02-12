// Package types provides NLM (Network Lock Manager) protocol types and constants.
//
// NLM is the POSIX file locking protocol used by NFS clients to coordinate
// advisory locks across the network. This implementation follows the NLM v4
// specification from the Open Group.
//
// Key characteristics of NLM v4:
//   - Uses 64-bit offsets and lengths (unlike NLM v1-3 which used 32-bit)
//   - Supports both exclusive (write) and shared (read) locks
//   - Provides blocking lock requests with callback notification
//   - Includes grace period handling for server recovery
//
// NLM works in conjunction with NSM (Network Status Monitor) for lock reclaim
// after server crashes.
//
// References:
//   - Open Group NLM Protocol Specification
//   - RFC 1813 Section 4 (NFS Locking)
package types

// ============================================================================
// NLM RPC Program and Version
// ============================================================================

const (
	// ProgramNLM is the NLM RPC program number.
	// Per Open Group specification, NLM uses program number 100021.
	ProgramNLM uint32 = 100021

	// NLMVersion4 is the NLM protocol version implementing 64-bit offsets.
	// Version 4 is required for NFS v3 compatibility.
	NLMVersion4 uint32 = 4
)

// ============================================================================
// NLM v4 Procedure Numbers
// ============================================================================
//
// NLM defines both synchronous and asynchronous versions of most procedures.
// Per CONTEXT.md, we implement ONLY synchronous procedures initially.
// Asynchronous procedures (_MSG and _RES variants) may be added later.

const (
	// NLMProcNull is the NULL procedure for connection testing (ping).
	// No authentication required, always succeeds.
	NLMProcNull uint32 = 0

	// NLMProcTest checks if a lock can be granted without actually acquiring it.
	// Returns lock holder info if the lock would conflict.
	NLMProcTest uint32 = 1

	// NLMProcLock acquires an advisory lock on a byte range.
	// Can be blocking (wait for lock) or non-blocking (fail immediately).
	NLMProcLock uint32 = 2

	// NLMProcCancel cancels a pending (blocked) lock request.
	// Used when client times out waiting for a lock.
	NLMProcCancel uint32 = 3

	// NLMProcUnlock releases a previously acquired lock.
	NLMProcUnlock uint32 = 4

	// NLMProcGranted is a callback from server to client notifying that
	// a previously blocked lock has been granted.
	// Used in the callback direction (server → client).
	NLMProcGranted uint32 = 5

	// NLMProcTestMsg is the async version of TEST (client → server).
	// Response is sent via TEST_RES callback.
	NLMProcTestMsg uint32 = 6

	// NLMProcLockMsg is the async version of LOCK (client → server).
	// Response is sent via LOCK_RES callback.
	NLMProcLockMsg uint32 = 7

	// NLMProcCancelMsg is the async version of CANCEL (client → server).
	// Response is sent via CANCEL_RES callback.
	NLMProcCancelMsg uint32 = 8

	// NLMProcUnlockMsg is the async version of UNLOCK (client → server).
	// Response is sent via UNLOCK_RES callback.
	NLMProcUnlockMsg uint32 = 9

	// NLMProcGrantedMsg is the async version of GRANTED (server → client callback).
	// Response is sent via GRANTED_RES callback.
	NLMProcGrantedMsg uint32 = 10

	// NLMProcTestRes is the TEST response callback (server → client).
	NLMProcTestRes uint32 = 11

	// NLMProcLockRes is the LOCK response callback (server → client).
	NLMProcLockRes uint32 = 12

	// NLMProcCancelRes is the CANCEL response callback (server → client).
	NLMProcCancelRes uint32 = 13

	// NLMProcUnlockRes is the UNLOCK response callback (server → client).
	NLMProcUnlockRes uint32 = 14

	// NLMProcGrantedRes is the GRANTED response callback (client → server).
	// Client acknowledges receipt of GRANTED notification.
	NLMProcGrantedRes uint32 = 15

	// NLMProcShare and NLMProcUnshare are DOS-style share mode locks.
	// These are rarely used with modern NFS clients.
	NLMProcShare   uint32 = 20
	NLMProcUnshare uint32 = 21

	// NLMProcNMLock is a non-monitored lock (no NSM tracking).
	// Used for locks that don't need server crash recovery.
	NLMProcNMLock uint32 = 22

	// NLMProcFreeAll releases all locks held by a client.
	// Called by NSM during client recovery.
	NLMProcFreeAll uint32 = 23
)

// ============================================================================
// NLM v4 Status Codes (nlm4_stats)
// ============================================================================
//
// Per Open Group NLM specification, these are the possible status codes
// returned from NLM procedures.

const (
	// NLM4Granted indicates the lock was successfully granted.
	NLM4Granted uint32 = 0

	// NLM4Denied indicates the lock was denied due to a conflict with
	// an existing lock held by another client.
	NLM4Denied uint32 = 1

	// NLM4DeniedNoLocks indicates the server has run out of lock resources.
	// The client should retry later.
	NLM4DeniedNoLocks uint32 = 2

	// NLM4Blocked indicates the lock request has been queued and the server
	// will call back via GRANTED when the lock becomes available.
	// Only returned when Block=true in the request.
	NLM4Blocked uint32 = 3

	// NLM4DeniedGrace indicates the server is in a grace period after restart.
	// Only reclaim requests are allowed during grace period.
	// The client should retry after the grace period ends.
	NLM4DeniedGrace uint32 = 4

	// NLM4Deadlock indicates the lock would cause a deadlock condition.
	// The server detected a cycle in the wait-for graph.
	NLM4Deadlock uint32 = 5

	// NLM4ROFS indicates the file is on a read-only filesystem.
	// Write locks cannot be granted.
	NLM4ROFS uint32 = 6

	// NLM4StaleFH indicates the file handle is no longer valid.
	// The file may have been deleted or the server restarted.
	NLM4StaleFH uint32 = 7

	// NLM4FBIG indicates the lock range is too large.
	// The offset or length exceeds server limits.
	NLM4FBIG uint32 = 8

	// NLM4Failed is a general failure code for unspecified errors.
	NLM4Failed uint32 = 9
)

// StatusString returns a human-readable string for an NLM status code.
func StatusString(status uint32) string {
	switch status {
	case NLM4Granted:
		return "NLM4_GRANTED"
	case NLM4Denied:
		return "NLM4_DENIED"
	case NLM4DeniedNoLocks:
		return "NLM4_DENIED_NOLOCKS"
	case NLM4Blocked:
		return "NLM4_BLOCKED"
	case NLM4DeniedGrace:
		return "NLM4_DENIED_GRACE_PERIOD"
	case NLM4Deadlock:
		return "NLM4_DEADLCK"
	case NLM4ROFS:
		return "NLM4_ROFS"
	case NLM4StaleFH:
		return "NLM4_STALE_FH"
	case NLM4FBIG:
		return "NLM4_FBIG"
	case NLM4Failed:
		return "NLM4_FAILED"
	default:
		return "NLM4_UNKNOWN"
	}
}

// ProcedureName returns a human-readable name for an NLM procedure number.
func ProcedureName(proc uint32) string {
	switch proc {
	case NLMProcNull:
		return "NULL"
	case NLMProcTest:
		return "TEST"
	case NLMProcLock:
		return "LOCK"
	case NLMProcCancel:
		return "CANCEL"
	case NLMProcUnlock:
		return "UNLOCK"
	case NLMProcGranted:
		return "GRANTED"
	case NLMProcTestMsg:
		return "TEST_MSG"
	case NLMProcLockMsg:
		return "LOCK_MSG"
	case NLMProcCancelMsg:
		return "CANCEL_MSG"
	case NLMProcUnlockMsg:
		return "UNLOCK_MSG"
	case NLMProcGrantedMsg:
		return "GRANTED_MSG"
	case NLMProcTestRes:
		return "TEST_RES"
	case NLMProcLockRes:
		return "LOCK_RES"
	case NLMProcCancelRes:
		return "CANCEL_RES"
	case NLMProcUnlockRes:
		return "UNLOCK_RES"
	case NLMProcGrantedRes:
		return "GRANTED_RES"
	case NLMProcShare:
		return "SHARE"
	case NLMProcUnshare:
		return "UNSHARE"
	case NLMProcNMLock:
		return "NM_LOCK"
	case NLMProcFreeAll:
		return "FREE_ALL"
	default:
		return "UNKNOWN"
	}
}
