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

	switch call.Program {
	case rpc.ProgramNFS:
		replyData, err = c.handleNFSProcedure(call.Procedure, procedureData)
	case rpc.ProgramMount:
		replyData, err = c.handleMountProcedure(call.Procedure, procedureData)
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

func (c *conn) handleNFSProcedure(procedure uint32, data []byte) ([]byte, error) {
	repo := c.server.repository
	contentRepo := c.server.content
	handler := c.server.nfsHandler

	switch procedure {
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
		// case NFSProcReadLink:
		// 	return handler.ReadLink(repo, data)
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
		// case NFSProcMkdir:
		// 	return handler.Mkdir(repo, data)
		// case NFSProcSymlink:
		// 	return handler.Symlink(repo, data)
		// case NFSProcMknod:
		// 	return handler.MkNod(repo, data)
		// case NFSProcRemove:
		// 	return handler.Remove(repo, data)
		// case NFSProcRmdir:
		// 	return handler.RmDir(repo, data)
		// case NFSProcRename:
		// 	return handler.Rename(repo, data)
		// case NFSProcLink:
		// 	return handler.Link(repo, data)
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
				return handler.FsInfo(repo, req)
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

	// case NFSProcCommit:
	// 	return handler.Commit(repo, data)
	default:
		logger.Debug("Unknown NFS procedure: %d", procedure)
		return []byte{}, nil
	}
}

func (c *conn) handleMountProcedure(procedure uint32, data []byte) ([]byte, error) {
	repo := c.server.repository
	handler := c.server.mountHandler

	switch procedure {
	case mount.MountProcNull:
		return handler.MountNull(repo)
	case mount.MountProcMnt:
		return handleRequest(
			data,
			mount.DecodeMountRequest,
			func(req *mount.MountRequest) (*mount.MountResponse, error) {
				return handler.Mount(repo, req)
			},
			mount.MountErrIO,
			func(status uint32) *mount.MountResponse {
				return &mount.MountResponse{Status: status}
			},
		)
	case mount.MountProcUmnt:
		return handleRequest(
			data,
			mount.DecodeUmountRequest,
			func(req *mount.UmountRequest) (*mount.UmountResponse, error) {
				return handler.Umnt(repo, req)
			},
			mount.MountErrIO,
			func(status uint32) *mount.UmountResponse {
				return &mount.UmountResponse{}
			},
		)
	default:
		logger.Debug("Unknown Mount procedure: %d", procedure)
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
