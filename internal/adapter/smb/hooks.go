package smb

import (
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

	// Register preauth integrity hash hooks for NEGOTIATE.
	// Per [MS-SMB2] 3.3.5.4: the preauth integrity hash chain is updated
	// with the raw bytes of every NEGOTIATE and SESSION_SETUP message.
	//
	// Before-hook: always updates the hash with the request bytes.
	// The dialect is not yet known at request time, so we always hash.
	// If the negotiated dialect is not 3.1.1, the hash is simply unused.
	RegisterBeforeHook(types.CommandNegotiate, preauthHashBeforeHook)

	// After-hook: updates the hash with the response bytes, but only if
	// the negotiated dialect is 3.1.1 (checked via CryptoState.Dialect).
	RegisterAfterHook(types.CommandNegotiate, preauthHashAfterHook)
}

// RegisterBeforeHook appends a hook to run before handler execution for the given command.
func RegisterBeforeHook(cmd types.Command, hook DispatchHook) {
	beforeHooks[cmd] = append(beforeHooks[cmd], hook)
}

// RegisterAfterHook appends a hook to run after handler execution for the given command.
func RegisterAfterHook(cmd types.Command, hook DispatchHook) {
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

// preauthHashBeforeHook updates the preauth integrity hash with the NEGOTIATE
// request bytes. Called before the handler processes the request.
//
// Per [MS-SMB2] 3.3.5.4: H(i) = SHA-512(H(i-1) || Message(i))
// We always update here because the dialect is not yet known. If the final
// negotiated dialect is not 3.1.1, the hash value is simply unused.
func preauthHashBeforeHook(connInfo *ConnInfo, _ types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Preauth hash updated with NEGOTIATE request",
		"messageLen", len(rawMessage))
}

// preauthHashAfterHook updates the preauth integrity hash with the NEGOTIATE
// response bytes. Only updates if the negotiated dialect is 3.1.1.
//
// Per [MS-SMB2] 3.3.5.4: The response hash is computed only when 3.1.1 is selected.
func preauthHashAfterHook(connInfo *ConnInfo, _ types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	// Only update the hash for 3.1.1 — the handler sets Dialect on CryptoState
	// after selecting the negotiated dialect.
	if connInfo.CryptoState.GetDialect() != types.Dialect0311 {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Preauth hash updated with NEGOTIATE response",
		"messageLen", len(rawMessage))
}
