package callback

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ProcessGrantedCallback sends NLM_GRANTED callback to a waiter and handles failures.
//
// This function:
// 1. Checks if the waiter was cancelled while processing
// 2. Builds the NLM_GRANTED callback arguments
// 3. Sends the callback to the client
// 4. If callback fails, releases the lock immediately (per CONTEXT.md locked decision)
//
// Per CONTEXT.md locked decision:
//   - Release lock immediately if NLM_GRANTED callback fails (no hold period)
//   - This prevents orphaned grants when clients become unreachable
//
// Parameters:
//   - ctx: Context for cancellation
//   - waiter: The pending lock request that was granted
//   - lm: Lock manager to release the lock if callback fails
//   - metrics: Optional metrics collector (may be nil)
//
// Returns:
//   - true if callback succeeded
//   - false if callback failed (lock was released) or waiter was cancelled
func ProcessGrantedCallback(
	ctx context.Context,
	waiter *blocking.Waiter,
	lm *lock.Manager,
	metrics *Metrics,
) bool {
	// Check if cancelled while we were processing
	if waiter.IsCancelled() {
		logger.Debug("Skipping callback for cancelled waiter",
			"owner", waiter.Lock.Owner.OwnerID)
		if metrics != nil {
			metrics.RecordCallback("cancelled")
		}
		return false
	}

	// Build NLM_GRANTED args
	args := &types.NLM4GrantedArgs{
		Cookie:    waiter.Cookie,
		Exclusive: waiter.Exclusive,
		Lock: types.NLM4Lock{
			CallerName: waiter.CallerName,
			FH:         waiter.FileHandle,
			OH:         waiter.OH,
			Svid:       waiter.Svid,
			Offset:     waiter.Lock.Offset,
			Length:     waiter.Lock.Length,
		},
	}

	// Record callback timing
	start := time.Now()

	// Send callback
	err := SendGrantedCallback(ctx, waiter.CallbackAddr, waiter.CallbackProg,
		waiter.CallbackVers, args)

	duration := time.Since(start)

	if metrics != nil {
		metrics.ObserveCallbackDuration(duration)
	}

	if err != nil {
		logger.Warn("NLM_GRANTED callback failed, releasing lock",
			"error", err,
			"addr", waiter.CallbackAddr,
			"owner", waiter.Lock.Owner.OwnerID,
			"duration", duration)

		// Per CONTEXT.md locked decision: release lock immediately if callback fails
		handleKey := string(waiter.Lock.FileHandle)
		_ = lm.RemoveUnifiedLock(handleKey, waiter.Lock.Owner,
			waiter.Lock.Offset, waiter.Lock.Length)

		if metrics != nil {
			metrics.RecordCallback("failed")
		}
		return false
	}

	logger.Debug("NLM_GRANTED callback succeeded",
		"addr", waiter.CallbackAddr,
		"owner", waiter.Lock.Owner.OwnerID,
		"duration", duration)

	if metrics != nil {
		metrics.RecordCallback("success")
	}
	return true
}

// Metrics interface for callback operations.
// This is a subset of the full NLM metrics.
type Metrics struct {
	recordCallback          func(result string)
	observeCallbackDuration func(duration time.Duration)
}

// NewCallbackMetrics creates a Metrics struct with the given functions.
func NewCallbackMetrics(
	recordCallback func(result string),
	observeCallbackDuration func(duration time.Duration),
) *Metrics {
	return &Metrics{
		recordCallback:          recordCallback,
		observeCallbackDuration: observeCallbackDuration,
	}
}

// RecordCallback records a callback result (success/failed/cancelled).
func (m *Metrics) RecordCallback(result string) {
	if m != nil && m.recordCallback != nil {
		m.recordCallback(result)
	}
}

// ObserveCallbackDuration records callback duration.
func (m *Metrics) ObserveCallbackDuration(duration time.Duration) {
	if m != nil && m.observeCallbackDuration != nil {
		m.observeCallbackDuration(duration)
	}
}
