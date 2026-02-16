// Package handlers implements NFSv4 COMPOUND operation dispatch and individual
// operation handlers. It follows the same patterns as v3/handlers but uses
// the COMPOUND model where multiple operations are bundled in a single RPC call.
package handlers

import (
	"bytes"
	"io"
	"sync"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/identity"
)

// OpHandler is the type signature for individual NFSv4 operation handlers.
// Each handler receives the mutable compound context and an io.Reader
// positioned at the operation's arguments. It returns a CompoundResult
// with the operation's status and XDR-encoded response data.
type OpHandler func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult

// Handler is the concrete implementation for NFSv4 protocol handlers.
// It processes COMPOUND RPCs by dispatching operations through
// the opDispatchTable.
type Handler struct {
	// Registry provides access to all stores and shares.
	Registry *runtime.Runtime

	// PseudoFS is the pseudo-filesystem for virtual namespace navigation.
	PseudoFS *pseudofs.PseudoFS

	// StateManager is the central NFSv4 state coordinator for client
	// identity, open state, and lease tracking.
	StateManager *state.StateManager

	// KerberosEnabled indicates whether RPCSEC_GSS (Kerberos) authentication
	// is available. When true, SECINFO responses include krb5/krb5i/krb5p
	// pseudo-flavors in addition to AUTH_SYS and AUTH_NONE.
	KerberosEnabled bool

	// IdentityMapper is an optional identity resolver for FATTR4_OWNER
	// and FATTR4_OWNER_GROUP encoding. When non-nil, UIDs/GIDs are
	// reverse-resolved to user@domain format. When nil, numeric format is used.
	IdentityMapper identity.IdentityMapper

	// blockedOpsMu protects blockedOps from concurrent read/write access.
	blockedOpsMu sync.RWMutex

	// blockedOps is a set of operation numbers blocked at the adapter level.
	// Populated from NFSAdapterSettings.BlockedOperations.
	// If an operation is in this set, COMPOUND returns NFS4ERR_NOTSUPP for it.
	blockedOps map[uint32]bool

	// opDispatchTable maps operation numbers to handler functions.
	opDispatchTable map[uint32]OpHandler
}

// NewHandler creates a new NFSv4 handler with the given runtime, pseudo-fs,
// and state manager. If stateManager is nil, a default one is created with
// a 90-second lease duration (suitable for testing).
//
// All operation handlers are initialized to return NFS4ERR_NOTSUPP except
// for OP_ILLEGAL which returns NFS4ERR_OP_ILLEGAL.
func NewHandler(registry *runtime.Runtime, pfs *pseudofs.PseudoFS, stateManager ...*state.StateManager) *Handler {
	var sm *state.StateManager
	if len(stateManager) > 0 && stateManager[0] != nil {
		sm = stateManager[0]
	} else {
		sm = state.NewStateManager(state.DefaultLeaseDuration)
	}

	h := &Handler{
		Registry:        registry,
		PseudoFS:        pfs,
		StateManager:    sm,
		opDispatchTable: make(map[uint32]OpHandler),
	}

	// Register all implemented operation handlers.
	// Filehandle management
	h.opDispatchTable[types.OP_PUTFH] = h.handlePutFH
	h.opDispatchTable[types.OP_PUTROOTFH] = h.handlePutRootFH
	h.opDispatchTable[types.OP_PUTPUBFH] = h.handlePutPubFH
	h.opDispatchTable[types.OP_GETFH] = h.handleGetFH
	h.opDispatchTable[types.OP_SAVEFH] = h.handleSaveFH
	h.opDispatchTable[types.OP_RESTOREFH] = h.handleRestoreFH

	// Pseudo-fs traversal and attributes
	h.opDispatchTable[types.OP_LOOKUP] = h.handleLookup
	h.opDispatchTable[types.OP_LOOKUPP] = h.handleLookupP
	h.opDispatchTable[types.OP_GETATTR] = h.handleGetAttr
	h.opDispatchTable[types.OP_READDIR] = h.handleReadDir
	h.opDispatchTable[types.OP_ACCESS] = h.handleAccess

	// Symlink operations
	h.opDispatchTable[types.OP_READLINK] = h.handleReadLink

	// Create/Remove operations
	h.opDispatchTable[types.OP_CREATE] = h.handleCreate
	h.opDispatchTable[types.OP_REMOVE] = h.handleRemove

	// Link and rename operations
	h.opDispatchTable[types.OP_LINK] = h.handleLink
	h.opDispatchTable[types.OP_RENAME] = h.handleRename

	// Attribute operations
	h.opDispatchTable[types.OP_SETATTR] = h.handleSetAttr

	// Protocol operations
	h.opDispatchTable[types.OP_ILLEGAL] = h.handleIllegal

	// Stateful I/O operations (Phase 7 placeholders, Phase 9 proper state)
	h.opDispatchTable[types.OP_OPEN] = h.handleOpen
	h.opDispatchTable[types.OP_OPEN_CONFIRM] = h.handleOpenConfirm
	h.opDispatchTable[types.OP_CLOSE] = h.handleClose

	// Data I/O operations
	h.opDispatchTable[types.OP_READ] = h.handleRead
	h.opDispatchTable[types.OP_WRITE] = h.handleWrite
	h.opDispatchTable[types.OP_COMMIT] = h.handleCommit

	// Conditional verification
	h.opDispatchTable[types.OP_VERIFY] = h.handleVerify
	h.opDispatchTable[types.OP_NVERIFY] = h.handleNVerify

	// Security negotiation
	h.opDispatchTable[types.OP_SECINFO] = h.handleSecInfo

	// Client setup stubs (Phase 9 implements proper state management)
	h.opDispatchTable[types.OP_SETCLIENTID] = h.handleSetClientID
	h.opDispatchTable[types.OP_SETCLIENTID_CONFIRM] = h.handleSetClientIDConfirm
	h.opDispatchTable[types.OP_RENEW] = h.handleRenew

	// Lock operations (Plan 10-01, 10-02)
	h.opDispatchTable[types.OP_LOCK] = h.handleLock
	h.opDispatchTable[types.OP_LOCKT] = h.handleLockT
	h.opDispatchTable[types.OP_LOCKU] = h.handleLockU

	// Delegation operations (Plan 11-01, 11-03)
	h.opDispatchTable[types.OP_DELEGRETURN] = h.handleDelegReturn
	h.opDispatchTable[types.OP_DELEGPURGE] = h.handleDelegPurge

	// Stub operations (deferred to later phases)
	h.opDispatchTable[types.OP_OPENATTR] = h.handleOpenAttr
	h.opDispatchTable[types.OP_OPEN_DOWNGRADE] = h.handleOpenDowngrade
	h.opDispatchTable[types.OP_RELEASE_LOCKOWNER] = h.handleReleaseLockOwner

	// Grace period lifecycle (Plan 09-04):
	// On server startup (if previous clients exist):
	//   handler.StateManager.StartGracePeriod(previousClientIDs)
	// On graceful shutdown:
	//   snapshots := handler.StateManager.SaveClientState()
	//   // serialize snapshots to disk
	//   handler.StateManager.Shutdown()

	return h
}

// handleIllegal handles the OP_ILLEGAL operation.
// Per RFC 7530, this returns NFS4ERR_OP_ILLEGAL.
func (h *Handler) handleIllegal(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	return &types.CompoundResult{
		Status: types.NFS4ERR_OP_ILLEGAL,
		OpCode: types.OP_ILLEGAL,
		Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
	}
}

// notSuppHandler creates a CompoundResult for an unsupported operation.
// The result includes the operation's opcode and NFS4ERR_NOTSUPP status.
func notSuppHandler(opCode uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: types.NFS4ERR_NOTSUPP,
		OpCode: opCode,
		Data:   encodeStatusOnly(types.NFS4ERR_NOTSUPP),
	}
}

// SetBlockedOps replaces the adapter-level blocked operations set.
// Called when live settings change (via SettingsWatcher) to refresh the
// operations that COMPOUND should reject with NFS4ERR_NOTSUPP.
//
// The opNames slice uses the human-readable operation names returned by
// types.OpName (e.g., "READ", "WRITE", "DELEGPURGE"). Unrecognised names
// are silently ignored so that stale DB entries don't crash the server.
func (h *Handler) SetBlockedOps(opNames []string) {
	blocked := make(map[uint32]bool, len(opNames))
	for _, name := range opNames {
		if opNum, ok := types.OpNameToNum(name); ok {
			blocked[opNum] = true
		}
	}
	h.blockedOpsMu.Lock()
	h.blockedOps = blocked
	h.blockedOpsMu.Unlock()
}

// IsOperationBlocked returns true if the given operation number is blocked
// at the adapter level. Per-share blocked operations are checked separately
// in the COMPOUND dispatcher using the CompoundContext after PUTFH resolves.
func (h *Handler) IsOperationBlocked(opNum uint32) bool {
	h.blockedOpsMu.RLock()
	blocked := h.blockedOps
	h.blockedOpsMu.RUnlock()
	if blocked == nil {
		return false
	}
	return blocked[opNum]
}

// encodeStatusOnly XDR-encodes a status-only response (just the nfsstat4).
// Many NFSv4 operation error responses consist of only the status code.
func encodeStatusOnly(status uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, status)
	return buf.Bytes()
}
