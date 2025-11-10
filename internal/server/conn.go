package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/mount"
	"github.com/marmos91/dittofs/internal/protocol/nfs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
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

// serve handles all RPC requests for this connection.
// It implements panic recovery to prevent a single misbehaving connection
// from crashing the entire server.
//
// The connection is automatically closed when:
// - The context is cancelled (server shutdown)
// - An idle timeout occurs
// - A read or write timeout occurs
// - An unrecoverable error occurs
// - The client closes the connection
func (c *conn) serve(ctx context.Context) {
	defer func() {
		// Panic recovery - prevents a single connection from crashing the server
		if r := recover(); r != nil {
			logger.Error("Panic in connection handler from %s: %v",
				c.conn.RemoteAddr().String(), r)
		}
		c.conn.Close()
	}()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New connection from %s", clientAddr)

	// Set initial idle timeout
	if c.server.config.IdleTimeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.IdleTimeout)); err != nil {
			logger.Warn("Failed to set deadline for %s: %v", clientAddr, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Debug("Connection from %s closed due to context cancellation", clientAddr)
			return
		case <-c.server.shutdown:
			logger.Debug("Connection from %s closed due to server shutdown", clientAddr)
			return
		default:
		}

		err := c.handleRequest()
		if err != nil {
			if err == io.EOF {
				logger.Debug("Connection from %s closed by client", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("Connection from %s timed out: %v", clientAddr, err)
			} else {
				logger.Debug("Error handling request from %s: %v", clientAddr, err)
			}
			return
		}

		// Reset idle timeout after successful request
		if c.server.config.IdleTimeout > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.IdleTimeout)); err != nil {
				logger.Warn("Failed to reset deadline for %s: %v", clientAddr, err)
			}
		}
	}
}

func (c *conn) handleRequest() error {
	// Apply read timeout if configured
	if c.server.config.ReadTimeout > 0 {
		deadline := time.Now().Add(c.server.config.ReadTimeout)
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read fragment header
	header, err := c.readFragmentHeader()
	if err != nil {
		return err
	}

	// Validate fragment size to prevent memory exhaustion
	const maxFragmentSize = 1 << 20 // 1MB - NFS messages are typically much smaller
	if header.Length > maxFragmentSize {
		logger.Warn("Fragment size %d exceeds maximum %d from %s",
			header.Length, maxFragmentSize, c.conn.RemoteAddr().String())
		return fmt.Errorf("fragment too large: %d bytes", header.Length)
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

// ============================================================================
// Authentication Extraction Helper
// ============================================================================

// extractUnixAuth extracts Unix authentication credentials from an RPC call.
// It returns the UID, GID, and supplementary GIDs if AUTH_UNIX is used.
// If AUTH_UNIX is not used or parsing fails, it returns nil values.
//
// This helper centralizes authentication extraction logic and ensures
// consistent error logging across all procedures.
//
// Parameters:
//   - call: The RPC call message containing authentication data
//   - procedure: Name of the procedure (for logging purposes)
//
// Returns:
//   - uid: Pointer to user ID (nil if not AUTH_UNIX or parsing failed)
//   - gid: Pointer to group ID (nil if not AUTH_UNIX or parsing failed)
//   - gids: Slice of supplementary group IDs (nil if not AUTH_UNIX or parsing failed)
func extractUnixAuth(call *rpc.RPCCallMessage, procedure string) (*uint32, *uint32, []uint32) {
	authFlavor := call.GetAuthFlavor()

	// If not Unix auth, return nil values (this is expected for AUTH_NULL)
	if authFlavor != rpc.AuthUnix {
		return nil, nil, nil
	}

	// Get auth body
	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("%s: AUTH_UNIX specified but auth body is empty", procedure)
		return nil, nil, nil
	}

	// Parse Unix auth credentials
	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		// Log the parsing failure - this is unexpected and may indicate
		// a protocol issue or malicious client
		logger.Warn("%s: Failed to parse AUTH_UNIX credentials: %v", procedure, err)
		return nil, nil, nil
	}

	// Log successful auth parsing at debug level
	logger.Debug("%s: Parsed Unix auth: uid=%d gid=%d ngids=%d",
		procedure, unixAuth.UID, unixAuth.GID, len(unixAuth.GIDs))

	return &unixAuth.UID, &unixAuth.GID, unixAuth.GIDs
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

	// Extract auth flavor for all procedures
	authFlavor := call.GetAuthFlavor()

	switch call.Procedure {
	case types.NFSProcNull:
		uid, gid, gids := extractUnixAuth(call, "READDIRPLUS")
		nullCtx := &nfs.NullContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}

		return handleRequest(
			data,
			nfs.DecodeNullRequest,
			func(req *nfs.NullRequest) (*nfs.NullResponse, error) {
				return handler.Null(repo, req, nullCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.NullResponse {
				return &nfs.NullResponse{}
			},
		)

	case types.NFSProcGetAttr:
		getAttrCtx := &nfs.GetAttrContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
		}
		return handleRequest(
			data,
			nfs.DecodeGetAttrRequest,
			func(req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error) {
				return handler.GetAttr(repo, req, getAttrCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.GetAttrResponse {
				return &nfs.GetAttrResponse{Status: status}
			},
		)

	case types.NFSProcSetAttr:
		uid, gid, gids := extractUnixAuth(call, "SETATTR")
		setAttrCtx := &nfs.SetAttrContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeSetAttrRequest,
			func(req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error) {
				return handler.SetAttr(repo, req, setAttrCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.SetAttrResponse {
				return &nfs.SetAttrResponse{Status: status}
			},
		)

	case types.NFSProcLookup:
		uid, gid, gids := extractUnixAuth(call, "LOOKUP")
		lookupCtx := &nfs.LookupContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeLookupRequest,
			func(req *nfs.LookupRequest) (*nfs.LookupResponse, error) {
				return handler.Lookup(repo, req, lookupCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.LookupResponse {
				return &nfs.LookupResponse{Status: status}
			},
		)

	case types.NFSProcAccess:
		uid, gid, gids := extractUnixAuth(call, "ACCESS")
		accessCtx := &nfs.AccessContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeAccessRequest,
			func(req *nfs.AccessRequest) (*nfs.AccessResponse, error) {
				return handler.Access(repo, req, accessCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.AccessResponse {
				return &nfs.AccessResponse{Status: status}
			},
		)

	case types.NFSProcReadLink:
		uid, gid, gids := extractUnixAuth(call, "READLINK")
		readLinkCtx := &nfs.ReadLinkContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeReadLinkRequest,
			func(req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error) {
				return handler.ReadLink(repo, req, readLinkCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.ReadLinkResponse {
				return &nfs.ReadLinkResponse{Status: status}
			},
		)

	case types.NFSProcRead:
		uid, gid, gids := extractUnixAuth(call, "READ")
		readCtx := &nfs.ReadContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeReadRequest,
			func(req *nfs.ReadRequest) (*nfs.ReadResponse, error) {
				return handler.Read(contentRepo, repo, req, readCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.ReadResponse {
				return &nfs.ReadResponse{Status: status}
			},
		)

	case types.NFSProcWrite:
		uid, gid, gids := extractUnixAuth(call, "WRITE")
		writeCtx := &nfs.WriteContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeWriteRequest,
			func(req *nfs.WriteRequest) (*nfs.WriteResponse, error) {
				return handler.Write(contentRepo, repo, req, writeCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.WriteResponse {
				return &nfs.WriteResponse{Status: status}
			},
		)

	case types.NFSProcCreate:
		uid, gid, _ := extractUnixAuth(call, "CREATE")
		createCtx := &nfs.CreateContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
		}
		return handleRequest(
			data,
			nfs.DecodeCreateRequest,
			func(req *nfs.CreateRequest) (*nfs.CreateResponse, error) {
				return handler.Create(contentRepo, repo, req, createCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.CreateResponse {
				return &nfs.CreateResponse{Status: status}
			},
		)

	case types.NFSProcMkdir:
		uid, gid, gids := extractUnixAuth(call, "MKDIR")
		mkdirContext := &nfs.MkdirContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeMkdirRequest,
			func(req *nfs.MkdirRequest) (*nfs.MkdirResponse, error) {
				return handler.Mkdir(repo, req, mkdirContext)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.MkdirResponse {
				return &nfs.MkdirResponse{Status: status}
			},
		)

	case types.NFSProcSymlink:
		uid, gid, gids := extractUnixAuth(call, "SYMLINK")
		symlinkCtx := &nfs.SymlinkContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeSymlinkRequest,
			func(req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error) {
				return handler.Symlink(repo, req, symlinkCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.SymlinkResponse {
				return &nfs.SymlinkResponse{Status: status}
			},
		)

	case types.NFSProcMknod:
		mkNodCtx := &nfs.MknodContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
		}
		return handleRequest(
			data,
			nfs.DecodeMknodRequest,
			func(req *nfs.MknodRequest) (*nfs.MknodResponse, error) {
				return handler.Mknod(repo, req, mkNodCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.MknodResponse {
				return &nfs.MknodResponse{Status: status}
			},
		)

	case types.NFSProcRemove:
		uid, gid, gids := extractUnixAuth(call, "REMOVE")
		removeContext := &nfs.RemoveContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeRemoveRequest,
			func(req *nfs.RemoveRequest) (*nfs.RemoveResponse, error) {
				return handler.Remove(repo, req, removeContext)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.RemoveResponse {
				return &nfs.RemoveResponse{Status: status}
			},
		)

	case types.NFSProcRmdir:
		uid, gid, gids := extractUnixAuth(call, "RMDIR")
		rmDirCtx := &nfs.RmdirContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeRmdirRequest,
			func(req *nfs.RmdirRequest) (*nfs.RmdirResponse, error) {
				return handler.Rmdir(repo, req, rmDirCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.RmdirResponse {
				return &nfs.RmdirResponse{Status: status}
			},
		)

	case types.NFSProcRename:
		uid, gid, gids := extractUnixAuth(call, "RENAME")
		renameCtx := &nfs.RenameContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeRenameRequest,
			func(req *nfs.RenameRequest) (*nfs.RenameResponse, error) {
				return handler.Rename(repo, req, renameCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.RenameResponse {
				return &nfs.RenameResponse{Status: status}
			},
		)

	case types.NFSProcLink:
		uid, gid, gids := extractUnixAuth(call, "LINK")
		linkCtx := &nfs.LinkContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeLinkRequest,
			func(req *nfs.LinkRequest) (*nfs.LinkResponse, error) {
				return handler.Link(repo, req, linkCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.LinkResponse {
				return &nfs.LinkResponse{Status: status}
			},
		)

	case types.NFSProcReadDir:
		uid, gid, gids := extractUnixAuth(call, "READDIR")
		readDirCtx := &nfs.ReadDirContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeReadDirRequest,
			func(req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error) {
				return handler.ReadDir(repo, req, readDirCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.ReadDirResponse {
				return &nfs.ReadDirResponse{Status: status}
			},
		)

	case types.NFSProcReadDirPlus:
		uid, gid, gids := extractUnixAuth(call, "READDIRPLUS")
		readDirPlusCtx := &nfs.ReadDirPlusContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}
		return handleRequest(
			data,
			nfs.DecodeReadDirPlusRequest,
			func(req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error) {
				return handler.ReadDirPlus(repo, req, readDirPlusCtx)
			},
			types.NFS3ErrAcces,
			func(status uint32) *nfs.ReadDirPlusResponse {
				return &nfs.ReadDirPlusResponse{Status: status}
			},
		)

	case types.NFSProcFsStat:
		ctx := &nfs.FsStatContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
		}
		return handleRequest(
			data,
			nfs.DecodeFsStatRequest,
			func(req *nfs.FsStatRequest) (*nfs.FsStatResponse, error) {
				return handler.FsStat(repo, req, ctx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.FsStatResponse {
				return &nfs.FsStatResponse{Status: status}
			},
		)

	case types.NFSProcFsInfo:
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
			types.NFS3ErrIO,
			func(status uint32) *nfs.FsInfoResponse {
				return &nfs.FsInfoResponse{Status: status}
			},
		)

	case types.NFSProcPathConf:
		pathConfCtx := &nfs.PathConfContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
		}
		return handleRequest(
			data,
			nfs.DecodePathConfRequest,
			func(req *nfs.PathConfRequest) (*nfs.PathConfResponse, error) {
				return handler.PathConf(repo, req, pathConfCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.PathConfResponse {
				return &nfs.PathConfResponse{Status: status}
			},
		)

	case types.NFSProcCommit:
		uid, gid, gids := extractUnixAuth(call, "COMMIT")
		commitCtx := &nfs.CommitContext{
			ClientAddr: c.conn.RemoteAddr().String(),
			AuthFlavor: authFlavor,
			UID:        uid,
			GID:        gid,
			GIDs:       gids,
		}

		return handleRequest(
			data,
			nfs.DecodeCommitRequest,
			func(req *nfs.CommitRequest) (*nfs.CommitResponse, error) {
				return handler.Commit(repo, req, commitCtx)
			},
			types.NFS3ErrIO,
			func(status uint32) *nfs.CommitResponse {
				return &nfs.CommitResponse{Status: status}
			},
		)

	default:
		logger.Debug("Unknown NFS procedure: %d", call.Procedure)
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
					logger.Warn("MOUNT: Failed to parse Unix auth credentials: %v", err)
					// Don't fail the mount, just proceed without detailed auth info
				} else {
					unixAuth = parsedAuth
					logger.Debug("MOUNT: Parsed Unix auth: %s", parsedAuth.String())
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
	if c.server.config.WriteTimeout > 0 {
		deadline := time.Now().Add(c.server.config.WriteTimeout)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	reply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		return fmt.Errorf("make reply: %w", err)
	}

	_, err = c.conn.Write(reply)
	if err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	logger.Debug("Sent reply for XID=0x%x (%d bytes)", xid, len(reply))
	return nil
}
