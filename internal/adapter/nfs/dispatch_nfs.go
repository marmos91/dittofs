package nfs

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// NFS Dispatch Table Initialization
// ============================================================================

func initNFSDispatchTable() {
	NfsDispatchTable = map[uint32]*nfsProcedure{
		types.NFSProcNull: {
			Name:      "NULL",
			Handler:   handleNFSNull,
			NeedsAuth: false,
		},
		types.NFSProcGetAttr: {
			Name:      "GETATTR",
			Handler:   handleNFSGetAttr,
			NeedsAuth: false,
		},
		types.NFSProcSetAttr: {
			Name:      "SETATTR",
			Handler:   handleNFSSetAttr,
			NeedsAuth: true,
		},
		types.NFSProcLookup: {
			Name:      "LOOKUP",
			Handler:   handleNFSLookup,
			NeedsAuth: true,
		},
		types.NFSProcAccess: {
			Name:      "ACCESS",
			Handler:   handleNFSAccess,
			NeedsAuth: true,
		},
		types.NFSProcReadLink: {
			Name:      "READLINK",
			Handler:   handleNFSReadLink,
			NeedsAuth: true,
		},
		types.NFSProcRead: {
			Name:      "READ",
			Handler:   handleNFSRead,
			NeedsAuth: true,
		},
		types.NFSProcWrite: {
			Name:      "WRITE",
			Handler:   handleNFSWrite,
			NeedsAuth: true,
		},
		types.NFSProcCreate: {
			Name:      "CREATE",
			Handler:   handleNFSCreate,
			NeedsAuth: true,
		},
		types.NFSProcMkdir: {
			Name:      "MKDIR",
			Handler:   handleNFSMkdir,
			NeedsAuth: true,
		},
		types.NFSProcSymlink: {
			Name:      "SYMLINK",
			Handler:   handleNFSSymlink,
			NeedsAuth: true,
		},
		types.NFSProcMknod: {
			Name:      "MKNOD",
			Handler:   handleNFSMknod,
			NeedsAuth: false,
		},
		types.NFSProcRemove: {
			Name:      "REMOVE",
			Handler:   handleNFSRemove,
			NeedsAuth: true,
		},
		types.NFSProcRmdir: {
			Name:      "RMDIR",
			Handler:   handleNFSRmdir,
			NeedsAuth: true,
		},
		types.NFSProcRename: {
			Name:      "RENAME",
			Handler:   handleNFSRename,
			NeedsAuth: true,
		},
		types.NFSProcLink: {
			Name:      "LINK",
			Handler:   handleNFSLink,
			NeedsAuth: true,
		},
		types.NFSProcReadDir: {
			Name:      "READDIR",
			Handler:   handleNFSReadDir,
			NeedsAuth: true,
		},
		types.NFSProcReadDirPlus: {
			Name:      "READDIRPLUS",
			Handler:   handleNFSReadDirPlus,
			NeedsAuth: true,
		},
		types.NFSProcFsStat: {
			Name:      "FSSTAT",
			Handler:   handleNFSFsStat,
			NeedsAuth: false,
		},
		types.NFSProcFsInfo: {
			Name:      "FSINFO",
			Handler:   handleNFSFsInfo,
			NeedsAuth: false,
		},
		types.NFSProcPathConf: {
			Name:      "PATHCONF",
			Handler:   handleNFSPathConf,
			NeedsAuth: false,
		},
		types.NFSProcCommit: {
			Name:      "COMMIT",
			Handler:   handleNFSCommit,
			NeedsAuth: true,
		},
	}
}

// ============================================================================
// NFS Procedure Handlers
// ============================================================================
//
// Each handler function below follows the same pattern:
//  1. Create a procedure-specific context from the AuthContext
//  2. Propagate the Go context for cancellation support
//  3. Call the handler via handleRequest helper
//  4. Return the encoded response
//
// The context propagation ensures that all procedure handlers can:
//  - Check for cancellation before expensive operations
//  - Respond to server shutdown signals
//  - Implement request timeouts
//  - Clean up resources properly on cancellation

func handleNFSNull(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeNullRequest,
		func(req *nfs.NullRequest) (*nfs.NullResponse, error) {
			return handler.Null(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.NullResponse {
			return &nfs.NullResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSGetAttr(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeGetAttrRequest,
		func(req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error) {
			return handler.GetAttr(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.GetAttrResponse {
			return &nfs.GetAttrResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSSetAttr(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeSetAttrRequest,
		func(req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error) {
			return handler.SetAttr(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.SetAttrResponse {
			return &nfs.SetAttrResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSLookup(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeLookupRequest,
		func(req *nfs.LookupRequest) (*nfs.LookupResponse, error) {
			return handler.Lookup(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.LookupResponse {
			return &nfs.LookupResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSAccess(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeAccessRequest,
		func(req *nfs.AccessRequest) (*nfs.AccessResponse, error) {
			return handler.Access(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.AccessResponse {
			return &nfs.AccessResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadLink(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeReadLinkRequest,
		func(req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error) {
			return handler.ReadLink(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.ReadLinkResponse {
			return &nfs.ReadLinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRead(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	// Custom dispatch (not using generic handleRequest) to capture
	// resp.Count for metrics without re-decoding the request.

	// Decode request
	req, err := nfs.DecodeReadRequest(data)
	if err != nil {
		logger.Debug("Error decoding READ request", "error", err)
		errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encoded, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
	}

	// Call handler
	resp, err := handler.Read(ctx, req)
	if err != nil {
		logger.Debug("READ handler error", "error", err)
		errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encoded, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
	}

	// Extract status and byte count before encoding
	status := resp.GetStatus()
	bytesRead := uint64(resp.Count)

	// Encode response
	encoded, err := resp.Encode()

	// Release pooled resources after encoding
	resp.Release()

	if err != nil {
		logger.Debug("Error encoding READ response", "error", err)
		errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encodedErr, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encodedErr, NFSStatus: types.NFS3ErrIO}, err
	}

	result := &HandlerResult{Data: encoded, NFSStatus: status}
	if status == types.NFS3OK {
		result.BytesRead = bytesRead
	}
	return result, nil
}

func handleNFSWrite(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	// Custom dispatch (not using generic handleRequest) to capture
	// resp.Count for metrics without re-decoding the request.

	// Decode request
	req, err := nfs.DecodeWriteRequest(data)
	if err != nil {
		logger.Debug("Error decoding WRITE request", "error", err)
		errResp := &nfs.WriteResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encoded, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
	}

	// Call handler
	resp, err := handler.Write(ctx, req)
	if err != nil {
		logger.Debug("WRITE handler error", "error", err)
		errResp := &nfs.WriteResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encoded, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
	}

	// Extract status and byte count before encoding
	status := resp.GetStatus()
	bytesWritten := uint64(resp.Count)

	// Encode response
	encoded, err := resp.Encode()
	if err != nil {
		logger.Debug("Error encoding WRITE response", "error", err)
		errResp := &nfs.WriteResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
		encodedErr, encErr := errResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, NFSStatus: types.NFS3ErrIO}, encErr
		}
		return &HandlerResult{Data: encodedErr, NFSStatus: types.NFS3ErrIO}, err
	}

	result := &HandlerResult{Data: encoded, NFSStatus: status}
	if status == types.NFS3OK {
		result.BytesWritten = bytesWritten
	}
	return result, nil
}

func handleNFSCreate(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeCreateRequest,
		func(req *nfs.CreateRequest) (*nfs.CreateResponse, error) {
			return handler.Create(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CreateResponse {
			return &nfs.CreateResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSMkdir(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeMkdirRequest,
		func(req *nfs.MkdirRequest) (*nfs.MkdirResponse, error) {
			return handler.Mkdir(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MkdirResponse {
			return &nfs.MkdirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSSymlink(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeSymlinkRequest,
		func(req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error) {
			return handler.Symlink(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.SymlinkResponse {
			return &nfs.SymlinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSMknod(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeMknodRequest,
		func(req *nfs.MknodRequest) (*nfs.MknodResponse, error) {
			return handler.Mknod(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MknodResponse {
			return &nfs.MknodResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRemove(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeRemoveRequest,
		func(req *nfs.RemoveRequest) (*nfs.RemoveResponse, error) {
			return handler.Remove(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RemoveResponse {
			return &nfs.RemoveResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRmdir(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeRmdirRequest,
		func(req *nfs.RmdirRequest) (*nfs.RmdirResponse, error) {
			return handler.Rmdir(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RmdirResponse {
			return &nfs.RmdirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRename(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeRenameRequest,
		func(req *nfs.RenameRequest) (*nfs.RenameResponse, error) {
			return handler.Rename(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RenameResponse {
			return &nfs.RenameResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSLink(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeLinkRequest,
		func(req *nfs.LinkRequest) (*nfs.LinkResponse, error) {
			return handler.Link(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.LinkResponse {
			return &nfs.LinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadDir(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeReadDirRequest,
		func(req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error) {
			return handler.ReadDir(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.ReadDirResponse {
			return &nfs.ReadDirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadDirPlus(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeReadDirPlusRequest,
		func(req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error) {
			return handler.ReadDirPlus(ctx, req)
		},
		types.NFS3ErrAccess,
		func(status uint32) *nfs.ReadDirPlusResponse {
			return &nfs.ReadDirPlusResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSFsStat(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeFsStatRequest,
		func(req *nfs.FsStatRequest) (*nfs.FsStatResponse, error) {
			return handler.FsStat(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsStatResponse {
			return &nfs.FsStatResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSFsInfo(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeFsInfoRequest,
		func(req *nfs.FsInfoRequest) (*nfs.FsInfoResponse, error) {
			return handler.FsInfo(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsInfoResponse {
			return &nfs.FsInfoResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSPathConf(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodePathConfRequest,
		func(req *nfs.PathConfRequest) (*nfs.PathConfResponse, error) {
			return handler.PathConf(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.PathConfResponse {
			return &nfs.PathConfResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSCommit(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		nfs.DecodeCommitRequest,
		func(req *nfs.CommitRequest) (*nfs.CommitResponse, error) {
			return handler.Commit(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CommitResponse {
			return &nfs.CommitResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}
