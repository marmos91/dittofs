// Package handlers implements NFSv4 COMPOUND operation dispatch and individual
// operation handlers. It follows the same patterns as v3/handlers but uses
// the COMPOUND model where multiple operations are bundled in a single RPC call.
package handlers

import (
	"encoding/binary"
	"io"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	v41handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/v41/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/adapter/nfs/identity"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// OpHandler is the type signature for individual NFSv4 operation handlers.
// Each handler receives the mutable compound context and an io.Reader
// positioned at the operation's arguments. It returns a CompoundResult
// with the operation's status and XDR-encoded response data.
type OpHandler func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult

// V41OpHandler is the type signature for NFSv4.1 operation handlers.
// Unlike v4.0 OpHandler, it receives a V41RequestContext with session/slot info
// populated by SEQUENCE processing. For stub handlers, the context is nil.
type V41OpHandler func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult

// Handler is the concrete implementation for NFSv4 protocol handlers.
// It processes COMPOUND RPCs by dispatching operations through
// the opDispatchTable (v4.0) and v41DispatchTable (v4.1).
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

	// opDispatchTable maps v4.0 operation numbers to handler functions.
	opDispatchTable map[uint32]OpHandler

	// v41DispatchTable maps v4.1 operation numbers (40-58) to handler functions.
	// v4.0 operations (3-39) are accessible from v4.1 compounds via fallback
	// to opDispatchTable.
	v41DispatchTable map[uint32]V41OpHandler

	// sequenceMetrics holds Prometheus metrics for SEQUENCE operation tracking.
	// May be nil; SequenceMetrics methods are nil-safe so callers can invoke
	// them without additional nil checks.
	v41Deps *v41handlers.Deps

	sequenceMetrics *state.SequenceMetrics

	// connectionMetrics holds Prometheus metrics for connection binding tracking.
	// May be nil; ConnectionMetrics methods are nil-safe so callers can invoke
	// them without additional nil checks.
	connectionMetrics *state.ConnectionMetrics

	// backchannelMetrics holds Prometheus metrics for backchannel callback tracking.
	// May be nil; BackchannelMetrics methods are nil-safe so callers can invoke
	// them without additional nil checks.
	backchannelMetrics *state.BackchannelMetrics

	// minMinorVersion is the minimum accepted NFSv4 minor version (default 0).
	// Compounds with minorversion < minMinorVersion get NFS4ERR_MINOR_VERS_MISMATCH.
	minMinorVersion uint32

	// maxMinorVersion is the maximum accepted NFSv4 minor version (default 1).
	// Compounds with minorversion > maxMinorVersion get NFS4ERR_MINOR_VERS_MISMATCH.
	maxMinorVersion uint32
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
		Registry:         registry,
		PseudoFS:         pfs,
		StateManager:     sm,
		opDispatchTable:  make(map[uint32]OpHandler),
		v41DispatchTable: make(map[uint32]V41OpHandler),
		maxMinorVersion:  1, // default: accept v4.0 and v4.1
	}

	// Initialize v4.1 handler dependencies
	v41d := &v41handlers.Deps{
		StateManager:    sm,
		SequenceMetrics: nil, // set later via SetSequenceMetrics
	}
	h.v41Deps = v41d

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

	// ============================================================================
	// NFSv4.1 dispatch table (stub handlers for all 19 v4.1 operations)
	// ============================================================================
	//
	// Each stub decodes its operation's XDR args (to prevent stream desync)
	// and returns NFS4ERR_NOTSUPP. Real handlers replace stubs in Phases 17-24.

	// BACKCHANNEL_CTL: update callback program and security params (RFC 8881 Section 18.33)
	h.v41DispatchTable[types.OP_BACKCHANNEL_CTL] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleBackchannelCtl(h.v41Deps, ctx, v41ctx, reader)
	}
	// BIND_CONN_TO_SESSION: connection binding (RFC 8881 Section 18.34)
	h.v41DispatchTable[types.OP_BIND_CONN_TO_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleBindConnToSession(h.v41Deps, ctx, v41ctx, reader)
	}
	// EXCHANGE_ID: client identity registration (RFC 8881 Section 18.35)
	h.v41DispatchTable[types.OP_EXCHANGE_ID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleExchangeID(h.v41Deps, ctx, v41ctx, reader)
	}
	// CREATE_SESSION: session lifecycle (RFC 8881 Section 18.36)
	h.v41DispatchTable[types.OP_CREATE_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleCreateSession(h.v41Deps, ctx, v41ctx, reader)
	}
	// DESTROY_SESSION: session teardown (RFC 8881 Section 18.37)
	h.v41DispatchTable[types.OP_DESTROY_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleDestroySession(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_FREE_STATEID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleFreeStateid(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_GET_DIR_DELEGATION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleGetDirDelegation(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_GETDEVICEINFO] = v41StubHandler(types.OP_GETDEVICEINFO, func(r io.Reader) error {
		var args types.GetDeviceInfoArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_GETDEVICELIST] = v41StubHandler(types.OP_GETDEVICELIST, func(r io.Reader) error {
		var args types.GetDeviceListArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTCOMMIT] = v41StubHandler(types.OP_LAYOUTCOMMIT, func(r io.Reader) error {
		var args types.LayoutCommitArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTGET] = v41StubHandler(types.OP_LAYOUTGET, func(r io.Reader) error {
		var args types.LayoutGetArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTRETURN] = v41StubHandler(types.OP_LAYOUTRETURN, func(r io.Reader) error {
		var args types.LayoutReturnArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_SECINFO_NO_NAME] = v41StubHandler(types.OP_SECINFO_NO_NAME, func(r io.Reader) error {
		var args types.SecinfoNoNameArgs
		return args.Decode(r)
	})
	// SEQUENCE at position > 0 returns NFS4ERR_SEQUENCE_POS per RFC 8881.
	// SEQUENCE at position 0 is handled specially in dispatchV41 before the op loop.
	h.v41DispatchTable[types.OP_SEQUENCE] = func(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		var args types.SequenceArgs
		if err := args.Decode(reader); err != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_SEQUENCE,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}
		return &types.CompoundResult{
			Status: types.NFS4ERR_SEQUENCE_POS,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(types.NFS4ERR_SEQUENCE_POS),
		}
	}
	h.v41DispatchTable[types.OP_SET_SSV] = v41StubHandler(types.OP_SET_SSV, func(r io.Reader) error {
		var args types.SetSsvArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_TEST_STATEID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleTestStateid(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_WANT_DELEGATION] = v41StubHandler(types.OP_WANT_DELEGATION, func(r io.Reader) error {
		var args types.WantDelegationArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_DESTROY_CLIENTID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleDestroyClientID(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_RECLAIM_COMPLETE] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleReclaimComplete(h.v41Deps, ctx, v41ctx, reader)
	}

	return h
}

// v41StubHandler creates a v4.1 stub handler that decodes args and returns NOTSUPP.
// The decoder parameter consumes the operation's XDR args from the reader to
// prevent stream desync (the CRITICAL invariant for COMPOUND processing).
func v41StubHandler(opCode uint32, decoder func(io.Reader) error) V41OpHandler {
	return func(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		if err := decoder(reader); err != nil {
			logger.Debug("NFSv4.1 stub decode error",
				"op", types.OpName(opCode), "error", err, "client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: opCode,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}
		logger.Debug("NFSv4.1 operation not yet implemented",
			"op", types.OpName(opCode), "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_NOTSUPP,
			OpCode: opCode,
			Data:   encodeStatusOnly(types.NFS4ERR_NOTSUPP),
		}
	}
}

// handleIllegal handles the OP_ILLEGAL operation (RFC 7530 Section 16.14).
// Returns NFS4ERR_OP_ILLEGAL for unknown opcodes outside valid operation ranges.
// No delegation; returns error immediately with no store access.
// No side effects; terminates the compound per stop-on-error semantics.
// Errors: NFS4ERR_OP_ILLEGAL (always).
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
// Reads live settings from the SettingsWatcher when available, falling back
// to the local blockedOps cache (used in tests or before settings load).
func (h *Handler) IsOperationBlocked(opNum uint32) bool {
	if h.Registry != nil {
		settings := h.Registry.GetNFSSettings()
		if settings != nil {
			blockedOps := settings.GetBlockedOperations()
			// Check if this operation is in the blocked list
			for _, name := range blockedOps {
				if num, ok := types.OpNameToNum(name); ok && num == opNum {
					return true
				}
			}
			return false
		}
	}

	// Fall back to local cache (for tests or when settings not available)
	h.blockedOpsMu.RLock()
	blocked := h.blockedOps
	h.blockedOpsMu.RUnlock()
	if blocked == nil {
		return false
	}
	return blocked[opNum]
}

// SetSequenceMetrics sets the Prometheus metrics collector for SEQUENCE operations.
// Must be called before any SEQUENCE operations. Safe to leave nil (no-op metrics).
func (h *Handler) SetSequenceMetrics(m *state.SequenceMetrics) {
	if h.v41Deps != nil {
		h.v41Deps.SequenceMetrics = m
	}
	h.sequenceMetrics = m
}

// SetConnectionMetrics sets the Prometheus metrics collector for connection binding.
// Must be called before any connection binding operations. Safe to leave nil (no-op metrics).
func (h *Handler) SetConnectionMetrics(m *state.ConnectionMetrics) {
	h.connectionMetrics = m
	h.StateManager.SetConnectionMetrics(m)
}

// SetBackchannelMetrics sets the Prometheus metrics collector for backchannel callbacks.
// Must be called before any backchannel operations. Safe to leave nil (no-op metrics).
func (h *Handler) SetBackchannelMetrics(m *state.BackchannelMetrics) {
	h.backchannelMetrics = m
	h.StateManager.SetBackchannelMetrics(m)
}

// SetMinorVersionRange sets the accepted minor version range for COMPOUND requests.
// Compounds with minorversion outside [min, max] get NFS4ERR_MINOR_VERS_MISMATCH.
// Default: min=0, max=1 (both v4.0 and v4.1 enabled).
func (h *Handler) SetMinorVersionRange(min, max uint32) {
	h.minMinorVersion = min
	h.maxMinorVersion = max
}

// encodeStatusOnly XDR-encodes a status-only response (just the nfsstat4).
// Many NFSv4 operation error responses consist of only the status code.
func encodeStatusOnly(status uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, status)
	return b
}
