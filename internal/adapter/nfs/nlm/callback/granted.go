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
	// Check if cancelled while we were processing
	if waiter.IsCancelled() {
		logger.Debug("Skipping callback for cancelled waiter",
			"owner", waiter.Lock.Owner.OwnerID)
		return false
	}

	// Resolve the client's NLM callback address. The host is the transport
	// source recorded at lock time; the port is looked up NOW from the client's
	// portmapper so we dial the lockd port the client actually registered rather
	// than a stale or hardcoded port. If resolution fails the grant cannot be
	// delivered: release the lock (same as a failed callback) so it is not
	// orphaned, and re-drive remaining waiters via the normal release hook.
	addr, err := callbackAddrResolver(ctx, waiter.CallbackHost)
	if err != nil {
		logger.Warn("NLM_GRANTED: cannot resolve client callback address, releasing lock",
			"host", waiter.CallbackHost,
			"owner", waiter.Lock.Owner.OwnerID,
			"error", err)
		handleKey := string(waiter.Lock.FileHandle)
		_ = lm.RemoveUnifiedLock(handleKey, waiter.Lock.Owner,
			waiter.Lock.Offset, waiter.Lock.Length)
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
	err = SendGrantedCallback(ctx, addr, waiter.CallbackProg,
		waiter.CallbackVers, args)

	duration := time.Since(start)

	if err != nil {
		logger.Warn("NLM_GRANTED callback failed, releasing lock",
			"error", err,
			"addr", addr,
			"owner", waiter.Lock.Owner.OwnerID,
			"duration", duration)

		// Per CONTEXT.md locked decision: release lock immediately if callback fails
		handleKey := string(waiter.Lock.FileHandle)
		_ = lm.RemoveUnifiedLock(handleKey, waiter.Lock.Owner,
			waiter.Lock.Offset, waiter.Lock.Length)

		return false
	}

	logger.Debug("NLM_GRANTED callback succeeded",
		"addr", addr,
		"owner", waiter.Lock.Owner.OwnerID,
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
func SetCallbackAddrResolver(fn func(ctx context.Context, host string) (string, error)) (restore func()) {
	prev := callbackAddrResolver
	callbackAddrResolver = fn
	return func() { callbackAddrResolver = prev }
}

// resolveCallbackAddr turns a client source host into a dialable NLM callback
// "host:port" address by querying the client's portmapper for its registered
// NLM (lockd) port. The host is always the request's transport source, never a
// client-supplied wire field, so the callback can only ever target the host
// that issued the lock.
func resolveCallbackAddr(ctx context.Context, host string) (string, error) {
	port, err := ResolveNLMCallbackPort(ctx, host, types.ProgramNLM, types.NLMVersion4)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}
