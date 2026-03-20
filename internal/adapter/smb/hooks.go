package smb

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// DispatchHook is called before or after handler execution for a specific command.
// It receives the connection info, the command being dispatched, and the raw SMB2
// message bytes (header + body) for that message.
//
// Hooks are used for cross-cutting concerns that need access to raw wire bytes,
// such as the preauth integrity hash chain computation for SMB 3.1.1.
type DispatchHook func(connInfo *ConnInfo, command types.Command, rawMessage []byte)

var (
	// beforeHooks are invoked before the handler processes the request.
	// Key: command code, Value: slice of hooks to run in order.
	beforeHooks map[types.Command][]DispatchHook

	// afterHooks are invoked after the handler has produced a response.
	// Key: command code, Value: slice of hooks to run in order.
	afterHooks map[types.Command][]DispatchHook
)

func init() {
	beforeHooks = make(map[types.Command][]DispatchHook)
	afterHooks = make(map[types.Command][]DispatchHook)

	// Register preauth integrity hash hooks for NEGOTIATE and SESSION_SETUP.
	//
	// NEGOTIATE messages update the connection-level preauth hash.
	// SESSION_SETUP messages update per-session preauth hashes (PreauthSessionTable).
	// Per [MS-SMB2] 3.3.5.5: each session maintains its own preauth hash chain
	// initialized from the connection hash after NEGOTIATE.
	registerBeforeHook(types.CommandNegotiate, preauthHashBeforeHook)
	registerAfterHook(types.CommandNegotiate, preauthHashAfterHook)

	registerBeforeHook(types.CommandSessionSetup, sessionPreauthBeforeHook)
	registerAfterHook(types.CommandSessionSetup, sessionPreauthAfterHook)
}

// registerBeforeHook appends a hook to run before handler execution for the given command.
// Must only be called during init().
func registerBeforeHook(cmd types.Command, hook DispatchHook) {
	beforeHooks[cmd] = append(beforeHooks[cmd], hook)
}

// registerAfterHook appends a hook to run after handler execution for the given command.
// Must only be called during init().
func registerAfterHook(cmd types.Command, hook DispatchHook) {
	afterHooks[cmd] = append(afterHooks[cmd], hook)
}

// RunBeforeHooks runs all before-hooks registered for the given command.
func RunBeforeHooks(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	for _, hook := range beforeHooks[cmd] {
		hook(connInfo, cmd, rawMessage)
	}
}

// RunAfterHooks runs all after-hooks registered for the given command.
func RunAfterHooks(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	for _, hook := range afterHooks[cmd] {
		hook(connInfo, cmd, rawMessage)
	}
}

// preauthHashBeforeHook updates the connection-level preauth integrity hash
// with NEGOTIATE request bytes.
//
// Per [MS-SMB2] 3.3.5.4: H(i) = SHA-512(H(i-1) || Message(i))
// We always update here because the dialect is not yet known at NEGOTIATE time.
// If the final negotiated dialect is not 3.1.1, the hash value is simply unused.
func preauthHashBeforeHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Connection preauth hash updated with request",
		"command", cmd.String(),
		"messageLen", len(rawMessage))
}

// preauthHashAfterHook updates the connection-level preauth integrity hash
// with NEGOTIATE response bytes. Only updates if dialect is 3.1.1.
func preauthHashAfterHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	if connInfo.CryptoState.GetDialect() != types.Dialect0311 {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Connection preauth hash updated with response",
		"command", cmd.String(),
		"messageLen", len(rawMessage))
}

// sessionPreauthBeforeHook handles per-session preauth hash tracking for
// SESSION_SETUP requests.
//
// Per [MS-SMB2] 3.3.5.5: SESSION_SETUP messages update per-session hashes
// (PreauthSessionTable), NOT the connection-level hash.
//
// Three cases:
//  1. SessionID == 0 (new session): Stash raw bytes for handler to consume
//     after allocating the session ID via InitSessionPreauthHash.
//  2. SessionID != 0, per-session hash exists (continuation): Update
//     the per-session hash directly.
//  3. SessionID != 0, per-session hash deleted (re-auth): Stash raw bytes
//     for handler to consume via InitSessionPreauthHash.
//
// We always stash the request. For case 2, we ALSO update the existing hash.
// The stash is harmless in case 2 — it won't be consumed since the handler
// calls completeNTLMAuth (not InitSessionPreauthHash) for continuations.
func sessionPreauthBeforeHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}

	// Always stash the raw request for potential consumption by
	// InitSessionPreauthHash (covers new session and re-auth cases).
	connInfo.CryptoState.StashPendingSessionSetup(rawMessage)

	// For non-zero SessionID, also update the per-session hash if the entry
	// exists (continuation case). This is a no-op if the entry was deleted
	// after original auth (re-auth case — handler will re-init).
	sessionID := extractSessionIDFromRaw(rawMessage)
	if sessionID != 0 {
		connInfo.CryptoState.UpdateSessionPreauthHash(sessionID, rawMessage)
	}

	logger.Debug("SESSION_SETUP preauth before-hook",
		"sessionID", sessionID,
		"messageLen", len(rawMessage))
}

// sessionPreauthAfterHook updates the per-session preauth hash with
// SESSION_SETUP response bytes. Only runs for SMB 3.1.1.
func sessionPreauthAfterHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	if connInfo.CryptoState.GetDialect() != types.Dialect0311 {
		return
	}

	// Parse SessionID from the response header
	sessionID := extractSessionIDFromRaw(rawMessage)
	if sessionID == 0 {
		return
	}

	connInfo.CryptoState.UpdateSessionPreauthHash(sessionID, rawMessage)
	logger.Debug("Per-session preauth hash updated with response",
		"sessionID", sessionID,
		"messageLen", len(rawMessage))
}

// extractSessionIDFromRaw extracts the SessionID from raw SMB2 message bytes.
// SessionID is at offset 40 in the SMB2 header (8 bytes, little-endian).
func extractSessionIDFromRaw(rawMessage []byte) uint64 {
	if len(rawMessage) < 48 {
		return 0
	}
	return binary.LittleEndian.Uint64(rawMessage[40:48])
}
