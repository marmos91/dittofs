// register_v40.go — NFSv4.0 (RFC 7530) operation handler registration.
package handlers

import "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"

// registerV40Ops registers every implemented NFSv4.0 operation (op numbers
// 3-39) into v40DispatchTable. These are reachable from v4.0, v4.1 and v4.2
// COMPOUNDs (the later minor versions are supersets).
func (h *Handler) registerV40Ops() {
	// Filehandle management
	h.v40DispatchTable[types.OP_PUTFH] = h.handlePutFH
	h.v40DispatchTable[types.OP_PUTROOTFH] = h.handlePutRootFH
	h.v40DispatchTable[types.OP_PUTPUBFH] = h.handlePutPubFH
	h.v40DispatchTable[types.OP_GETFH] = h.handleGetFH
	h.v40DispatchTable[types.OP_SAVEFH] = h.handleSaveFH
	h.v40DispatchTable[types.OP_RESTOREFH] = h.handleRestoreFH

	// Pseudo-fs traversal and attributes
	h.v40DispatchTable[types.OP_LOOKUP] = h.handleLookup
	h.v40DispatchTable[types.OP_LOOKUPP] = h.handleLookupP
	h.v40DispatchTable[types.OP_GETATTR] = h.handleGetAttr
	h.v40DispatchTable[types.OP_READDIR] = h.handleReadDir
	h.v40DispatchTable[types.OP_ACCESS] = h.handleAccess

	// Symlink operations
	h.v40DispatchTable[types.OP_READLINK] = h.handleReadLink

	// Create/Remove operations
	h.v40DispatchTable[types.OP_CREATE] = h.handleCreate
	h.v40DispatchTable[types.OP_REMOVE] = h.handleRemove

	// Link and rename operations
	h.v40DispatchTable[types.OP_LINK] = h.handleLink
	h.v40DispatchTable[types.OP_RENAME] = h.handleRename

	// Attribute operations
	h.v40DispatchTable[types.OP_SETATTR] = h.handleSetAttr

	// Protocol operations
	h.v40DispatchTable[types.OP_ILLEGAL] = h.handleIllegal

	// Stateful I/O operations
	h.v40DispatchTable[types.OP_OPEN] = h.handleOpen
	h.v40DispatchTable[types.OP_OPEN_CONFIRM] = h.handleOpenConfirm
	h.v40DispatchTable[types.OP_CLOSE] = h.handleClose

	// Data I/O operations
	h.v40DispatchTable[types.OP_READ] = h.handleRead
	h.v40DispatchTable[types.OP_WRITE] = h.handleWrite
	h.v40DispatchTable[types.OP_COMMIT] = h.handleCommit

	// Conditional verification
	h.v40DispatchTable[types.OP_VERIFY] = h.handleVerify
	h.v40DispatchTable[types.OP_NVERIFY] = h.handleNVerify

	// Security negotiation
	h.v40DispatchTable[types.OP_SECINFO] = h.handleSecInfo

	// Client setup operations
	h.v40DispatchTable[types.OP_SETCLIENTID] = h.handleSetClientID
	h.v40DispatchTable[types.OP_SETCLIENTID_CONFIRM] = h.handleSetClientIDConfirm
	h.v40DispatchTable[types.OP_RENEW] = h.handleRenew

	// Lock operations (, 10-02)
	h.v40DispatchTable[types.OP_LOCK] = h.handleLock
	h.v40DispatchTable[types.OP_LOCKT] = h.handleLockT
	h.v40DispatchTable[types.OP_LOCKU] = h.handleLockU

	// Delegation operations (, 11-03)
	h.v40DispatchTable[types.OP_DELEGRETURN] = h.handleDelegReturn
	h.v40DispatchTable[types.OP_DELEGPURGE] = h.handleDelegPurge

	// Stub operations (deferred to later phases)
	h.v40DispatchTable[types.OP_OPENATTR] = h.handleOpenAttr
	h.v40DispatchTable[types.OP_OPEN_DOWNGRADE] = h.handleOpenDowngrade
	h.v40DispatchTable[types.OP_RELEASE_LOCKOWNER] = h.handleReleaseLockOwner

	// Grace period lifecycle :
	// On server startup (if previous clients exist):
	//   handler.StateManager.StartGracePeriod(previousClientIDs)
	// On graceful shutdown:
	//   snapshots := handler.StateManager.SaveClientState()
	//   // serialize snapshots to disk
	//   handler.StateManager.Shutdown()
}
