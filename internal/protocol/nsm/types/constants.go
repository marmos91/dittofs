// Package types provides NSM (Network Status Monitor) protocol types and constants.
//
// NSM is the crash recovery protocol used by NLM (Network Lock Manager) clients
// to detect server crashes and reclaim locks. This implementation follows the
// NSM v1 specification from the Open Group.
//
// Key characteristics of NSM:
//   - Monitors hosts and detects crashes via state counter changes
//   - Odd state = host is up, even state = host went down
//   - SM_MON registers for crash notifications
//   - SM_NOTIFY is sent to registered monitors when a host restarts
//
// NSM works in conjunction with NLM for lock recovery after server crashes.
//
// References:
//   - Open Group NSM Protocol Specification
//   - RFC 1813 Section 4 (NFS Locking)
package types

// ============================================================================
// NSM RPC Program and Version
// ============================================================================

const (
	// ProgramNSM is the NSM RPC program number.
	// Per Open Group specification, NSM uses program number 100024.
	ProgramNSM uint32 = 100024

	// SMVersion1 is the only NSM protocol version.
	SMVersion1 uint32 = 1
)

// ============================================================================
// NSM Procedure Numbers
// ============================================================================
//
// NSM defines a small set of procedures for monitoring host status.

const (
	// SMProcNull is the NULL procedure for connection testing (ping).
	// No authentication required, always succeeds.
	SMProcNull uint32 = 0

	// SMProcStat queries the current status (state counter) of a host.
	// Returns the current state number without establishing monitoring.
	SMProcStat uint32 = 1

	// SMProcMon registers the caller to receive notifications when a
	// monitored host's state changes (i.e., when it crashes and restarts).
	SMProcMon uint32 = 2

	// SMProcUnmon unregisters monitoring for a single host.
	// The caller will no longer receive notifications for that host.
	SMProcUnmon uint32 = 3

	// SMProcUnmonAll unregisters monitoring for all hosts.
	// Used during client shutdown to clean up all monitoring registrations.
	SMProcUnmonAll uint32 = 4

	// SMProcSimuCrash simulates a crash for testing purposes.
	// Increments the local state counter and sends notifications.
	// Should be disabled in production deployments.
	SMProcSimuCrash uint32 = 5

	// SMProcNotify is sent to registered monitors when a host restarts.
	// Contains the new state number of the restarted host.
	SMProcNotify uint32 = 6
)

// ============================================================================
// NSM Result Codes (sm_res)
// ============================================================================

const (
	// StatSucc indicates the monitoring operation succeeded.
	// For SM_MON: monitoring was established.
	// For SM_UNMON: monitoring was removed.
	StatSucc uint32 = 0

	// StatFail indicates the monitoring operation failed.
	// For SM_MON: unable to establish monitoring (resource exhaustion, etc.).
	StatFail uint32 = 1
)

// ============================================================================
// NSM String Length Limits
// ============================================================================

const (
	// SMMaxStrLen is the maximum length for NSM strings (hostnames).
	// Per Open Group specification, SM_MAXSTRLEN = 1024.
	SMMaxStrLen = 1024
)

// ProcedureName returns a human-readable name for an NSM procedure number.
func ProcedureName(proc uint32) string {
	switch proc {
	case SMProcNull:
		return "NULL"
	case SMProcStat:
		return "STAT"
	case SMProcMon:
		return "MON"
	case SMProcUnmon:
		return "UNMON"
	case SMProcUnmonAll:
		return "UNMON_ALL"
	case SMProcSimuCrash:
		return "SIMU_CRASH"
	case SMProcNotify:
		return "NOTIFY"
	default:
		return "UNKNOWN"
	}
}

// ResultString returns a human-readable string for an NSM result code.
func ResultString(result uint32) string {
	switch result {
	case StatSucc:
		return "STAT_SUCC"
	case StatFail:
		return "STAT_FAIL"
	default:
		return "UNKNOWN"
	}
}
