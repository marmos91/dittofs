package server

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/mount"
	"github.com/marmos91/dittofs/internal/protocol/nfs"
)

type rpcRequest interface {
	*nfs.GetAttrRequest |
		*nfs.FsStatRequest |
		*nfs.NullRequest |
		*nfs.FsInfoRequest |
		*nfs.PathConfRequest |
		*nfs.ReadDirRequest |
		*nfs.LookupRequest |
		*nfs.AccessRequest |
		*nfs.ReadDirPlusRequest |
		*nfs.ReadRequest |
		*nfs.SetAttrRequest |
		*nfs.WriteRequest |
		*nfs.CreateRequest |
		*nfs.RenameRequest |
		*nfs.RemoveRequest |
		*nfs.MkdirRequest |
		*nfs.RmdirRequest |
		*nfs.LinkRequest |
		*nfs.SymlinkRequest |
		*nfs.ReadLinkRequest |
		*nfs.MknodRequest |
		*nfs.CommitRequest |
		*mount.MountRequest |
		*mount.NullRequest |
		*mount.UmountRequest |
		*mount.DumpRequest |
		*mount.UmountAllRequest |
		*mount.ExportRequest
}

type rpcResponse interface {
	*nfs.GetAttrResponse |
		*nfs.FsStatResponse |
		*nfs.NullResponse |
		*nfs.FsInfoResponse |
		*nfs.PathConfResponse |
		*nfs.ReadDirResponse |
		*nfs.LookupResponse |
		*nfs.AccessResponse |
		*nfs.ReadDirPlusResponse |
		*nfs.ReadResponse |
		*nfs.SetAttrResponse |
		*nfs.WriteResponse |
		*nfs.CreateResponse |
		*nfs.RenameResponse |
		*nfs.RemoveResponse |
		*nfs.MkdirResponse |
		*nfs.RmdirResponse |
		*nfs.LinkResponse |
		*nfs.SymlinkResponse |
		*nfs.ReadLinkResponse |
		*nfs.MknodResponse |
		*nfs.CommitResponse |
		*mount.MountResponse |
		*mount.NullResponse |
		*mount.UmountResponse |
		*mount.DumpResponse |
		*mount.UmountAllResponse |
		*mount.ExportResponse
	Encode() ([]byte, error)
}

func handleRequest[Req rpcRequest, Resp rpcResponse](
	data []byte,
	decode func([]byte) (Req, error),
	handle func(Req) (Resp, error),
	errorStatus uint32,
	makeErrorResp func(uint32) Resp,
) ([]byte, error) {
	// Decode request
	req, err := decode(data)
	if err != nil {
		logger.Debug("Error decoding request: %v", err)
		errorResp := makeErrorResp(errorStatus)
		return errorResp.Encode()
	}

	// Call handler
	resp, err := handle(req)
	if err != nil {
		logger.Debug("Handler error: %v", err)
		errorResp := makeErrorResp(errorStatus)
		return errorResp.Encode()
	}

	// Encode response
	encoded, err := resp.Encode()
	if err != nil {
		logger.Debug("Error encoding response: %v", err)
		errorResp := makeErrorResp(errorStatus)
		return errorResp.Encode()
	}

	return encoded, nil
}
