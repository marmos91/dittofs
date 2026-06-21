// register_v42.go — NFSv4.2 (RFC 8276) extended-attribute operation registration.
package handlers

import "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"

// registerV42Ops registers the four RFC 8276 extended-attribute operations
// (op numbers 72-75) into v42DispatchTable, kept separate from v4.0/v4.1 so the
// minor-version boundary is explicit. dispatchOne gates these to minorversion 2:
// a v4.0/v4.1 COMPOUND carrying one of these opcodes gets NFS4ERR_NOTSUPP.
func (h *Handler) registerV42Ops() {
	h.v42DispatchTable[types.OP_GETXATTR] = h.handleGetXattr
	h.v42DispatchTable[types.OP_SETXATTR] = h.handleSetXattr
	h.v42DispatchTable[types.OP_LISTXATTRS] = h.handleListXattrs
	h.v42DispatchTable[types.OP_REMOVEXATTR] = h.handleRemoveXattr
}
