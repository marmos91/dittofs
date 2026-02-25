// Package handlers provides cross-protocol integration helpers for NLM handlers.
//
// This file contains helpers for building NLM responses when locks conflict
// with SMB leases or byte-range locks from other protocols.
package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// buildDeniedResponseFromSMBLease creates an NLM LOCK response for denial due to SMB lease.
//
// When an NLM LOCK request conflicts with an SMB lease, we return NLM4_DENIED
// with holder information translated from the SMB lease. This allows NFS clients
// to see that an SMB client holds a conflicting lease.
//
// Parameters:
//   - cookie: The cookie from the original LOCK request (echoed back)
//   - lease: The conflicting SMB lease (must have Lease != nil)
//
// Returns:
//   - *LockResponse: Response with NLM4_DENIED status and holder info
func buildDeniedResponseFromSMBLease(cookie []byte, lease *lock.UnifiedLock) *LockResponse {
	// Translate SMB lease to NLM holder format
	holderInfo := lock.TranslateToNLMHolder(lease)

	// Build the response
	// Note: LockResponse currently only has Cookie and Status
	// The holder info would be in an extended response type (NLM4_DENIEDargs)
	// For now, we just return NLM4Denied - holder info is logged for debugging
	logger.Info("NLM LOCK denied by SMB lease",
		"caller_name", holderInfo.CallerName,
		"exclusive", holderInfo.Exclusive,
		"lease_key", holderInfo.OH)

	return &LockResponse{
		Cookie: cookie,
		Status: types.NLM4Denied,
	}
}

// buildDeniedResponseFromByteRangeLock creates an NLM LOCK response for denial due to byte-range lock.
//
// Parameters:
//   - cookie: The cookie from the original LOCK request (echoed back)
//   - conflict: The conflicting byte-range lock
//
// Returns:
//   - *LockResponse: Response with NLM4_DENIED status
func buildDeniedResponseFromByteRangeLock(cookie []byte, conflict *lock.UnifiedLock) *LockResponse {
	// Translate to NLM holder format for logging
	holderInfo := lock.TranslateByteRangeLockToNLMHolder(conflict)

	logger.Debug("NLM LOCK denied by byte-range lock",
		"caller_name", holderInfo.CallerName,
		"offset", holderInfo.Offset,
		"length", holderInfo.Length,
		"exclusive", holderInfo.Exclusive)

	return &LockResponse{
		Cookie: cookie,
		Status: types.NLM4Denied,
	}
}
