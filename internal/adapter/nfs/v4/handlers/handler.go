// Package handlers implements NFSv4 COMPOUND operation dispatch and individual
// operation handlers. It follows the same patterns as v3/handlers but uses
// the COMPOUND model where multiple operations are bundled in a single RPC call.
package handlers

import (
	"io"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	v41handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/v41/handlers"
	"github.com/marmos91/dittofs/pkg/adapter/nfs/identity"
)

// V40OpHandler is the type signature for individual NFSv4 operation handlers.
// Each handler receives the mutable compound context and an io.Reader
// positioned at the operation's arguments. It returns a CompoundResult
// with the operation's status and XDR-encoded response data.
type V40OpHandler func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult

// V41OpHandler is the type signature for NFSv4.1 operation handlers.
// Unlike v4.0 V40OpHandler, it receives a V41RequestContext with session/slot info
// populated by SEQUENCE processing. For stub handlers, the context is nil.
type V41OpHandler func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult

// V42OpHandler is the type signature for NFSv4.2 operation handlers (RFC 8276
// extended-attribute ops). v4.2 ops run inside the v4.1 session machinery
// (SEQUENCE is processed by the COMPOUND loop before dispatch), but the xattr
// ops themselves are session-state-agnostic, so — like V40OpHandler — the
// signature omits the V41RequestContext. The distinct named type keeps the
// v4.2 op set visibly separate from v4.0/v4.1.
type V42OpHandler func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult

// Handler is the concrete implementation for NFSv4 protocol handlers.
// It processes COMPOUND RPCs by dispatching operations through
// the v40DispatchTable (v4.0) and v41DispatchTable (v4.1).
type Handler struct {
	// Registry provides access to all stores and shares.
	Registry nfsRuntime

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

	// v40DispatchTable maps v4.0 operation numbers to handler functions.
	v40DispatchTable map[uint32]V40OpHandler

	// v41DispatchTable maps v4.1 operation numbers (40-58) to handler functions.
	// v4.0 operations (3-39) are accessible from v4.1 compounds via fallback
	// to v40DispatchTable.
	v41DispatchTable map[uint32]V41OpHandler

	// v42DispatchTable maps v4.2 operation numbers (RFC 8276 xattr ops 72-75) to
	// handler functions, kept SEPARATE from v41DispatchTable so the minor-version
	// boundary is explicit: an op in this table is v4.2-only and is gated to
	// minorversion 2 in dispatchOne. v4.0/v4.1 ops remain reachable from a v4.2
	// COMPOUND via the v41/v40 tables (v4.2 is a superset).
	v42DispatchTable map[uint32]V42OpHandler

	// v41Deps holds shared dependencies for v4.1 operation handlers.
	v41Deps *v41handlers.Deps

	// minMinorVersion is the minimum accepted NFSv4 minor version (default 0).
	// Compounds with minorversion < minMinorVersion get NFS4ERR_MINOR_VERS_MISMATCH.
	minMinorVersion uint32

	// maxMinorVersion is the maximum accepted NFSv4 minor version (default 2).
	// Compounds with minorversion > maxMinorVersion get NFS4ERR_MINOR_VERS_MISMATCH.
	// v4.2 support is limited to the RFC 8276 extended-attribute operations.
	maxMinorVersion uint32

	// xattrBackend resolves named (extended) attribute operations for the four
	// RFC 8276 xattr handlers. It is satisfied structurally by *metadata.Service.
	// Depending on this interface (rather than concrete Service methods) keeps
	// PR2 decoupled from the unified xattr resolver landing in PR1 (#1285).
	xattrBackend XattrBackend
}

// NewHandler creates a new NFSv4 handler with the given runtime, pseudo-fs,
// and state manager. If stateManager is nil, a default one is created with
// a 90-second lease duration (suitable for testing).
//
// All operation handlers are initialized to return NFS4ERR_NOTSUPP except
// for OP_ILLEGAL which returns NFS4ERR_OP_ILLEGAL.
func NewHandler(registry nfsRuntime, pfs *pseudofs.PseudoFS, stateManager ...*state.StateManager) *Handler {
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
		v40DispatchTable: make(map[uint32]V40OpHandler),
		v41DispatchTable: make(map[uint32]V41OpHandler),
		v42DispatchTable: make(map[uint32]V42OpHandler),
		maxMinorVersion:  2, // default: accept v4.0, v4.1, and v4.2 (RFC 8276 xattr ops)
	}

	// Initialize v4.1 handler dependencies
	v41d := &v41handlers.Deps{
		StateManager: sm,
	}
	h.v41Deps = v41d

	// Register operation handlers grouped by NFS minor version. Each
	// registerV4xOps method lives in its own register_v4x.go file so the
	// version boundary is explicit: RFC 7530 (v4.0), RFC 8881 (v4.1),
	// RFC 8276 xattr ops (v4.2).
	h.registerV40Ops()
	h.registerV41Ops()
	h.registerV42Ops()

	return h
}

// SetXattrBackend overrides the extended-attribute backend used by the RFC 8276
// xattr handlers. When unset, the handlers resolve the backend from the
// registry's metadata service (see xattrBackendForHandler). Primarily used by
// tests to inject a fake backend.
func (h *Handler) SetXattrBackend(b XattrBackend) {
	h.xattrBackend = b
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
//
// This consults the pre-parsed blockedOps set, which is populated by
// SetBlockedOps from the SettingsWatcher in applyNFSSettings (on startup and
// on each settings-change event, with hot-reload support). Doing a single map
// lookup here keeps the hot COMPOUND dispatch path free of the per-op JSON
// unmarshal + linear name scan that reading live settings would incur — see
// the NFSv3 isOperationBlocked path, which is structured the same way.
func (h *Handler) IsOperationBlocked(opNum uint32) bool {
	h.blockedOpsMu.RLock()
	blocked := h.blockedOps[opNum]
	h.blockedOpsMu.RUnlock()
	return blocked
}

// SetMinorVersionRange sets the accepted minor version range for COMPOUND requests.
// Compounds with minorversion outside [min, max] get NFS4ERR_MINOR_VERS_MISMATCH.
// Default: min=0, max=2 (v4.0, v4.1, and v4.2 xattr ops enabled).
func (h *Handler) SetMinorVersionRange(min, max uint32) {
	h.minMinorVersion = min
	h.maxMinorVersion = max
}

// encodeStatusOnly XDR-encodes a status-only response (just the nfsstat4).
// Delegates to v41handlers.EncodeStatusOnly to avoid duplicating the encoding logic.
func encodeStatusOnly(status uint32) []byte {
	return v41handlers.EncodeStatusOnly(status)
}
