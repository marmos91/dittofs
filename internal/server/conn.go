package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/mount"
	"github.com/marmos91/dittofs/internal/protocol/nfs"
	"github.com/marmos91/dittofs/internal/protocol/rpc"
)

type conn struct {
	server *NFSServer
	conn   net.Conn
}

type fragmentHeader struct {
	IsLast bool
	Length uint32
}

func (c *conn) serve(ctx context.Context) {
	defer c.conn.Close()
	logger.Debug("New connection from %s", c.conn.RemoteAddr().String())

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := c.handleRequest(); err != nil {
				if err != io.EOF {
					logger.Debug("Error handling request: %v", err)
				}
				return
			}
		}
	}
}

func (c *conn) handleRequest() error {
	// Read fragment header
	header, err := c.readFragmentHeader()
	if err != nil {
		return err
	}

	// Read RPC message
	message, err := c.readRPCMessage(header.Length)
	if err != nil {
		return fmt.Errorf("read RPC message: %w", err)
	}

	// Parse RPC call
	call, err := rpc.ReadCall(message)
	if err != nil {
		logger.Debug("Error parsing RPC call: %v", err)
		return nil
	}

	logger.Debug("RPC Call: XID=0x%x Program=%d Version=%d Procedure=%d",
		call.XID, call.Program, call.Version, call.Procedure)

	// Extract procedure data
	procedureData, err := rpc.ReadData(message, call)
	if err != nil {
		return fmt.Errorf("extract procedure data: %w", err)
	}

	// Handle the call
	return c.handleRPCCall(call, procedureData)
}

func (c *conn) readFragmentHeader() (*fragmentHeader, error) {
	var buf [4]byte
	_, err := io.ReadFull(c.conn, buf[:])
	if err != nil {
		return nil, err
	}

	header := binary.BigEndian.Uint32(buf[:])
	return &fragmentHeader{
		IsLast: (header & 0x80000000) != 0,
		Length: header & 0x7FFFFFFF,
	}, nil
}

func (c *conn) readRPCMessage(length uint32) ([]byte, error) {
	message := make([]byte, length)
	_, err := io.ReadFull(c.conn, message)
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	return message, nil
}

func (c *conn) handleRPCCall(call *rpc.RPCCallMessage, procedureData []byte) error {
	var replyData []byte
	var err error

	logger.Debug("RPC Call Details: Program=%d Version=%d Procedure=%d",
		call.Program, call.Version, call.Procedure)

	switch call.Program {
	case rpc.ProgramNFS:
		replyData, err = c.handleNFSProcedure(call, procedureData)
	case rpc.ProgramMount:
		replyData, err = c.handleMountProcedure(call, procedureData)
	default:
		logger.Debug("Unknown program: %d", call.Program)
		return nil
	}

	if err != nil {
		logger.Debug("Handler error: %v", err)
		return fmt.Errorf("handle program %d: %w", call.Program, err)
	}

	return c.sendReply(call.XID, replyData)
}

func (c *conn) handleNFSProcedure(call *rpc.RPCCallMessage, data []byte) ([]byte, error) {
	repo := c.server.repository
	contentRepo := c.server.content
	handler := c.server.nfsHandler

	// Note: NFS procedures also receive auth info through the RPC call
	// We may want to pass this context to NFS handlers for permission checks
	// For now, we'll extract it but not use it in most operations
	authFlavor := call.GetAuthFlavor()

	if authFlavor == rpc.AuthUnix {
		authBody := call.GetAuthBody()
		if len(authBody) > 0 {
			if unixAuth, err := rpc.ParseUnixAuth(authBody); err == nil {
				logger.Debug("NFS call with Unix auth: uid=%d gid=%d", unixAuth.UID, unixAuth.GID)
			}
		}
	}

	switch call.Procedure {
	case nfs.NFSProcNull:
		return handler.Null(repo)
	case nfs.NFSProcGetAttr:
		return handleRequest(
			data,
			nfs.DecodeGetAttrRequest,
			func(req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error) {
				return handler.GetAttr(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.GetAttrResponse {
				return &nfs.GetAttrResponse{Status: status}
			},
		)
	case nfs.NFSProcSetAttr:
		return handleRequest(
			data,
			nfs.DecodeSetAttrRequest,
			func(req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error) {
				return handler.SetAttr(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.SetAttrResponse {
				return &nfs.SetAttrResponse{Status: status}
			},
		)
	case nfs.NFSProcLookup:
		return handleRequest(
			data,
			nfs.DecodeLookupRequest,
			func(req *nfs.LookupRequest) (*nfs.LookupResponse, error) {
				return handler.Lookup(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.LookupResponse {
				return &nfs.LookupResponse{Status: status}
			},
		)
	case nfs.NFSProcAccess:
		return handleRequest(
			data,
			nfs.DecodeAccessRequest,
			func(req *nfs.AccessRequest) (*nfs.AccessResponse, error) {
				return handler.Access(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.AccessResponse {
				return &nfs.AccessResponse{Status: status}
			},
		)
	case nfs.NFSProcReadLink:
		return handleRequest(
			data,
			nfs.DecodeReadLinkRequest,
			func(req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error) {
				return handler.ReadLink(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.ReadLinkResponse {
				return &nfs.ReadLinkResponse{Status: status}
			},
		)
	case nfs.NFSProcRead:
		return handleRequest(
			data,
			nfs.DecodeReadRequest,
			func(req *nfs.ReadRequest) (*nfs.ReadResponse, error) {
				return handler.Read(contentRepo, repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.ReadResponse {
				return &nfs.ReadResponse{Status: status}
			},
		)
	case nfs.NFSProcWrite:
		return handleRequest(
			data,
			nfs.DecodeWriteRequest,
			func(req *nfs.WriteRequest) (*nfs.WriteResponse, error) {
				return handler.Write(contentRepo, repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.WriteResponse {
				return &nfs.WriteResponse{Status: status}
			},
		)
	case nfs.NFSProcCreate:
		return handleRequest(
			data,
			nfs.DecodeCreateRequest,
			func(req *nfs.CreateRequest) (*nfs.CreateResponse, error) {
				return handler.Create(contentRepo, repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.CreateResponse {
				return &nfs.CreateResponse{Status: status}
			},
		)
	case nfs.NFSProcMkdir:
		return handleRequest(
			data,
			nfs.DecodeMkdirRequest,
			func(req *nfs.MkdirRequest) (*nfs.MkdirResponse, error) {
				return handler.Mkdir(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.MkdirResponse {
				return &nfs.MkdirResponse{Status: status}
			},
		)
	case nfs.NFSProcSymlink:
		return handleRequest(
			data,
			nfs.DecodeSymlinkRequest,
			func(req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error) {
				return handler.Symlink(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.SymlinkResponse {
				return &nfs.SymlinkResponse{Status: status}
			},
		)
	case nfs.NFSProcMknod:
		return handleRequest(
			data,
			nfs.DecodeMknodRequest,
			func(req *nfs.MknodRequest) (*nfs.MknodResponse, error) {
				return handler.Mknod(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.MknodResponse {
				return &nfs.MknodResponse{Status: status}
			},
		)
	case nfs.NFSProcRemove:
		return handleRequest(
			data,
			nfs.DecodeRemoveRequest,
			func(req *nfs.RemoveRequest) (*nfs.RemoveResponse, error) {
				return handler.Remove(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.RemoveResponse {
				return &nfs.RemoveResponse{Status: status}
			},
		)
	case nfs.NFSProcRmdir:
		return handleRequest(
			data,
			nfs.DecodeRmdirRequest,
			func(req *nfs.RmdirRequest) (*nfs.RmdirResponse, error) {
				return handler.Rmdir(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.RmdirResponse {
				return &nfs.RmdirResponse{Status: status}
			},
		)
	case nfs.NFSProcRename:
		return handleRequest(
			data,
			nfs.DecodeRenameRequest,
			func(req *nfs.RenameRequest) (*nfs.RenameResponse, error) {
				return handler.Rename(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.RenameResponse {
				return &nfs.RenameResponse{Status: status}
			},
		)
	case nfs.NFSProcLink:
		return handleRequest(
			data,
			nfs.DecodeLinkRequest,
			func(req *nfs.LinkRequest) (*nfs.LinkResponse, error) {
				return handler.Link(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.LinkResponse {
				return &nfs.LinkResponse{Status: status}
			},
		)
	case nfs.NFSProcReadDir:
		return handleRequest(
			data,
			nfs.DecodeReadDirRequest,
			func(req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error) {
				return handler.ReadDir(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.ReadDirResponse {
				return &nfs.ReadDirResponse{Status: status}
			},
		)
	case nfs.NFSProcReadDirPlus:
		return handleRequest(
			data,
			nfs.DecodeReadDirPlusRequest,
			func(req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error) {
				return handler.ReadDirPlus(repo, req)
			},
			nfs.NFS3ErrAcces,
			func(status uint32) *nfs.ReadDirPlusResponse {
				return &nfs.ReadDirPlusResponse{Status: status}
			},
		)
	case nfs.NFSProcFsStat:
		return handleRequest(
			data,
			nfs.DecodeFsStatRequest,
			func(req *nfs.FsStatRequest) (*nfs.FsStatResponse, error) {
				return handler.FsStat(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.FsStatResponse {
				return &nfs.FsStatResponse{Status: status}
			},
		)
	case nfs.NFSProcFsInfo:
		return handleRequest(
			data,
			nfs.DecodeFsInfoRequest,
			func(req *nfs.FsInfoRequest) (*nfs.FsInfoResponse, error) {
				ctx := &nfs.FsInfoContext{
					ClientAddr: c.conn.RemoteAddr().String(),
					AuthFlavor: authFlavor,
				}
				return handler.FsInfo(repo, req, ctx)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.FsInfoResponse {
				return &nfs.FsInfoResponse{Status: status}
			},
		)

	case nfs.NFSProcPathConf:
		return handleRequest(
			data,
			nfs.DecodePathConfRequest,
			func(req *nfs.PathConfRequest) (*nfs.PathConfResponse, error) {
				return handler.PathConf(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.PathConfResponse {
				return &nfs.PathConfResponse{Status: status}
			},
		)

	case nfs.NFSProcCommit:
		return handleRequest(
			data,
			nfs.DecodeCommitRequest,
			func(req *nfs.CommitRequest) (*nfs.CommitResponse, error) {
				return handler.Commit(repo, req)
			},
			nfs.NFS3ErrIO,
			func(status uint32) *nfs.CommitResponse {
				return &nfs.CommitResponse{Status: status}
			},
		)
	default:
		logger.Debug("Unknown NFS procedure: %d", call)
		return []byte{}, nil
	}
}

func (c *conn) handleMountProcedure(call *rpc.RPCCallMessage, data []byte) ([]byte, error) {
	repo := c.server.repository
	handler := c.server.mountHandler

	switch call.Procedure {
	case mount.MountProcNull:
		return handler.MountNull(repo)

	case mount.MountProcMnt:
		// Extract authentication from RPC call
		authFlavor := call.GetAuthFlavor()

		// Parse Unix credentials if present
		var unixAuth *rpc.UnixAuth
		if authFlavor == rpc.AuthUnix {
			authBody := call.GetAuthBody()
			if len(authBody) > 0 {
				parsedAuth, err := rpc.ParseUnixAuth(authBody)
				if err != nil {
					logger.Warn("Failed to parse Unix auth credentials: %v", err)
					// Don't fail the mount, just proceed without detailed auth info
				} else {
					unixAuth = parsedAuth
					logger.Debug("Parsed Unix auth: %s", parsedAuth.String())
				}
			}
		}

		// Create mount context with client information and auth
		ctx := &mount.MountContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UnixAuth:   unixAuth,
		}

		return handleRequest(
			data,
			mount.DecodeMountRequest,
			func(req *mount.MountRequest) (*mount.MountResponse, error) {
				return handler.Mount(repo, req, ctx)
			},
			mount.MountErrIO,
			func(status uint32) *mount.MountResponse {
				return &mount.MountResponse{Status: status}
			},
		)
	case mount.MountProcExport:

		return handleRequest(
			data,
			mount.DecodeExportRequest,
			func(req *mount.ExportRequest) (*mount.ExportResponse, error) {
				return handler.Export(repo, req)
			},
			mount.MountErrIO,
			func(status uint32) *mount.ExportResponse {
				// EXPORT doesn't use status codes, but we need this for the generic handler
				return &mount.ExportResponse{Entries: []mount.ExportEntry{}}
			},
		)
	case mount.MountProcDump:
		ctx := &mount.DumpContext{
			ClientAddr: c.conn.RemoteAddr().String(),
		}
		return handleRequest(
			data,
			mount.DecodeDumpRequest,
			func(req *mount.DumpRequest) (*mount.DumpResponse, error) {
				return handler.Dump(repo, req, ctx)
			},
			mount.MountErrIO,
			func(status uint32) *mount.DumpResponse {
				// DUMP doesn't use status codes, but we need this for the generic handler
				// In practice, errors are returned as Go errors, not mount status codes
				return &mount.DumpResponse{Entries: []mount.DumpEntry{}}
			},
		)
	case mount.MountProcUmnt:
		ctx := &mount.UmountContext{
			ClientAddr: c.conn.RemoteAddr().String(),
		}
		return handleRequest(
			data,
			mount.DecodeUmountRequest,
			func(req *mount.UmountRequest) (*mount.UmountResponse, error) {
				return handler.Umnt(repo, req, ctx)
			},
			mount.MountErrIO,
			func(status uint32) *mount.UmountResponse {
				return &mount.UmountResponse{}
			},
		)

	case mount.MountProcUmntAll:
		ctx := &mount.UmountAllContext{
			ClientAddr: c.conn.RemoteAddr().String(),
		}

		return handleRequest(
			data,
			mount.DecodeUmountAllRequest,
			func(req *mount.UmountAllRequest) (*mount.UmountAllResponse, error) {
				return handler.UmntAll(repo, req, ctx)
			},
			mount.MountErrIO,
			func(status uint32) *mount.UmountAllResponse {
				return &mount.UmountAllResponse{}
			},
		)

	default:
		logger.Debug("Unknown Mount procedure: %d", call.Procedure)
		return []byte{}, nil
	}
}

func (c *conn) sendReply(xid uint32, data []byte) error {
	reply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		return fmt.Errorf("make reply: %w", err)
	}

	_, err = c.conn.Write(reply)
	if err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	logger.Debug("Sent reply for XID=0x%x", xid)
	return nil
}
