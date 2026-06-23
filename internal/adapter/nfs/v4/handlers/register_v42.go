// register_v42.go — NFSv4.2 (RFC 8276) extended-attribute operation registration.
package handlers

import "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"

// registerV42Ops registers the NFSv4.2 operations into v42DispatchTable, kept
// separate from v4.0/v4.1 so the minor-version boundary is explicit:
//   - the four RFC 8276 extended-attribute operations (op numbers 72-75), and
//   - the RFC 7862 sparse-file cluster SEEK / READ_PLUS / DEALLOCATE / ALLOCATE,
//     which share one hole-tracking foundation (pkg/block hole map).
//
// dispatchOne gates these to minorversion 2: a v4.0/v4.1 COMPOUND carrying one
// of these opcodes gets NFS4ERR_NOTSUPP.
func (h *Handler) registerV42Ops() {
	// RFC 7862 sparse-file operations.
	h.v42DispatchTable[types.OP_ALLOCATE] = h.handleAllocate
	h.v42DispatchTable[types.OP_DEALLOCATE] = h.handleDeallocate
	h.v42DispatchTable[types.OP_SEEK] = h.handleSeek
	h.v42DispatchTable[types.OP_READ_PLUS] = h.handleReadPlus

	// RFC 8276 extended-attribute operations.
	h.v42DispatchTable[types.OP_GETXATTR] = h.handleGetXattr
	h.v42DispatchTable[types.OP_SETXATTR] = h.handleSetXattr
	h.v42DispatchTable[types.OP_LISTXATTRS] = h.handleListXattrs
	h.v42DispatchTable[types.OP_REMOVEXATTR] = h.handleRemoveXattr
}
