// Package v3 provides NFS version 3 (program 100003) dispatch: the procedure
// table, the twenty-two procedure thunks that adapt each wire call to a
// handler in v3/handlers, and the generic decode/handle/encode seam they
// share.
//
// It mirrors the layout of the sibling programs — mount, nlm, nsm and
// portmap — each of which owns its dispatch next to its handlers rather than
// in the RPC record layer above them.
package v3
