package callback

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ProcessGrantedCallback sends NLM_GRANTED callback to a waiter and handles failures.
//
// Steps:
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
//
// Returns:
//   - true if callback succeeded
//   - false if callback failed (lock was released) or waiter was cancelled
func ProcessGrantedCallback(
	ctx context.Context,
	waiter *blocking.Waiter,
	lm *lock.Manager,
) bool {
	// Read the waiter once, under its mutex. The queue can cancel it or (on a
	// retransmit) leave it to be removed at any point after GetWaiters handed
	// out the pointer, so every field below comes from this one snapshot rather
	// than a fresh read per use.
	w := waiter.Snapshot()

	// Check if cancelled while we were processing
	if w.Cancelled {
		logger.Debug("Skipping callback for cancelled waiter",
			"owner", w.Lock.Owner.OwnerID)
		return false
	}

	// Resolve the client's NLM callback address. The host is the transport
	// source recorded at lock time; the port is looked up NOW from the client's
	// portmapper so we dial the lockd port the client actually registered rather
	// than a stale or hardcoded port. If resolution fails the grant cannot be
	// delivered: release the lock (same as a failed callback) so it is not
	// orphaned, and re-drive remaining waiters via the normal release hook.
	addr, err := callbackAddrResolver(ctx, w.CallbackHost, w.CallbackVers)
	if err != nil {
		logger.Warn("NLM_GRANTED: cannot resolve client callback address, releasing lock",
			"host", w.CallbackHost,
			"owner", w.Lock.Owner.OwnerID,
			"error", err)
		handleKey := string(w.Lock.FileHandle)
		_ = lm.RemoveUnifiedLock(handleKey, w.Lock.Owner,
			w.Lock.Offset, w.Lock.Length)
		return false
	}

	// Build NLM_GRANTED args
	args := &types.NLM4GrantedArgs{
		Cookie:    w.Cookie,
		Exclusive: w.Exclusive,
		Lock: types.NLM4Lock{
			CallerName: w.CallerName,
			FH:         w.FileHandle,
			OH:         w.OH,
			Svid:       w.Svid,
			Offset:     w.Lock.Offset,
			Length:     w.Lock.Length,
		},
	}

	// Record callback timing
	start := time.Now()

	// Send callback
	err = SendGrantedCallback(ctx, addr, w.CallbackProg,
		w.CallbackVers, args)

	duration := time.Since(start)

	if err != nil {
		logger.Warn("NLM_GRANTED callback failed, releasing lock",
			"error", err,
			"addr", addr,
			"owner", w.Lock.Owner.OwnerID,
			"duration", duration)

		// Per CONTEXT.md locked decision: release lock immediately if callback fails
		handleKey := string(w.Lock.FileHandle)
		_ = lm.RemoveUnifiedLock(handleKey, w.Lock.Owner,
			w.Lock.Offset, w.Lock.Length)

		return false
	}

	logger.Debug("NLM_GRANTED callback succeeded",
		"addr", addr,
		"owner", w.Lock.Owner.OwnerID,
		"duration", duration)

	return true
}

// callbackAddrResolver resolves a client source host to a dialable NLM callback
// address. It is a package var so tests can stand in a deterministic resolver
// (the real one performs a portmap network round-trip). Production always uses
// resolveCallbackAddr.
var callbackAddrResolver = resolveCallbackAddr

// SetCallbackAddrResolver overrides how a client source host is resolved to a
// dialable NLM callback address. It returns a restore func. Intended for tests
// that need a deterministic callback target without a portmap round-trip.
func SetCallbackAddrResolver(fn func(ctx context.Context, host string, vers uint32) (string, error)) (restore func()) {
	prev := callbackAddrResolver
	callbackAddrResolver = fn
	return func() { callbackAddrResolver = prev }
}

// resolveCallbackAddr turns a client source host into a dialable NLM callback
// "host:port" address by querying the client's portmapper for its registered
// NLM (lockd) port. The host is always the request's transport source, never a
// client-supplied wire field, so the callback can only ever target the host
// that issued the lock. The portmap GETPORT must use the version the client
// negotiated: a v1/v3 client registers lockd under (100021, v1/v3), not v4, so
// querying the wrong version returns port 0 and the blocking-lock grant is lost.
func resolveCallbackAddr(ctx context.Context, host string, vers uint32) (string, error) {
	port, err := ResolveNLMCallbackPort(ctx, host, types.ProgramNLM, vers)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}
