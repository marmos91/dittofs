// Package smb provides SMB2 protocol dispatch and result types.
package smb

import (
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/internal/protocol/smb/v2/handlers"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// HandlerResult is an alias for the handlers.HandlerResult type
type HandlerResult = handlers.HandlerResult

// CommandHandler is the signature for SMB2 command handlers
type CommandHandler func(
	ctx *handlers.SMBHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	body []byte,
) (*HandlerResult, error)

// Command metadata
type Command struct {
	Name         string
	Handler      CommandHandler
	NeedsSession bool // Requires valid SessionID
	NeedsTree    bool // Requires valid TreeID
}

// DispatchTable maps SMB2 command codes to handlers
var DispatchTable map[types.Command]*Command

func init() {
	DispatchTable = map[types.Command]*Command{
		types.SMB2Negotiate: {
			Name:         "NEGOTIATE",
			Handler:      handleNegotiate,
			NeedsSession: false,
			NeedsTree:    false,
		},
		types.SMB2SessionSetup: {
			Name:         "SESSION_SETUP",
			Handler:      handleSessionSetup,
			NeedsSession: false,
			NeedsTree:    false,
		},
		types.SMB2Logoff: {
			Name:         "LOGOFF",
			Handler:      handleLogoff,
			NeedsSession: true,
			NeedsTree:    false,
		},
		types.SMB2TreeConnect: {
			Name:         "TREE_CONNECT",
			Handler:      handleTreeConnect,
			NeedsSession: true,
			NeedsTree:    false,
		},
		types.SMB2TreeDisconnect: {
			Name:         "TREE_DISCONNECT",
			Handler:      handleTreeDisconnect,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Create: {
			Name:         "CREATE",
			Handler:      handleCreate,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Close: {
			Name:         "CLOSE",
			Handler:      handleClose,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Flush: {
			Name:         "FLUSH",
			Handler:      handleFlush,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Read: {
			Name:         "READ",
			Handler:      handleRead,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Write: {
			Name:         "WRITE",
			Handler:      handleWrite,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2QueryDirectory: {
			Name:         "QUERY_DIRECTORY",
			Handler:      handleQueryDirectory,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2QueryInfo: {
			Name:         "QUERY_INFO",
			Handler:      handleQueryInfo,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2SetInfo: {
			Name:         "SET_INFO",
			Handler:      handleSetInfo,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Echo: {
			Name:         "ECHO",
			Handler:      handleEcho,
			NeedsSession: false,
			NeedsTree:    false,
		},
		types.SMB2Ioctl: {
			Name:         "IOCTL",
			Handler:      handleIoctl,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Cancel: {
			Name:         "CANCEL",
			Handler:      handleCancel,
			NeedsSession: true,
			NeedsTree:    false,
		},
		types.SMB2ChangeNotify: {
			Name:         "CHANGE_NOTIFY",
			Handler:      handleChangeNotify,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2Lock: {
			Name:         "LOCK",
			Handler:      handleLock,
			NeedsSession: true,
			NeedsTree:    true,
		},
		types.SMB2OplockBreak: {
			Name:         "OPLOCK_BREAK",
			Handler:      handleOplockBreak,
			NeedsSession: true,
			NeedsTree:    true,
		},
	}
}

// Dispatch handler wrappers - delegate to handlers package
func handleNegotiate(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.Negotiate(ctx, body)
}

func handleSessionSetup(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.SessionSetup(ctx, body)
}

func handleLogoff(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeLogoffRequest,
		func(req *handlers.LogoffRequest) (*handlers.LogoffResponse, error) {
			return h.Logoff(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.LogoffResponse {
			return &handlers.LogoffResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleTreeConnect(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.TreeConnect(ctx, body)
}

func handleTreeDisconnect(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.TreeDisconnect(ctx, body)
}

func handleCreate(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeCreateRequest,
		func(req *handlers.CreateRequest) (*handlers.CreateResponse, error) {
			return h.Create(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.CreateResponse {
			return &handlers.CreateResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleClose(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeCloseRequest,
		func(req *handlers.CloseRequest) (*handlers.CloseResponse, error) {
			return h.Close(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.CloseResponse {
			return &handlers.CloseResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleFlush(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeFlushRequest,
		func(req *handlers.FlushRequest) (*handlers.FlushResponse, error) {
			return h.Flush(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.FlushResponse {
			return &handlers.FlushResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleRead(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeReadRequest,
		func(req *handlers.ReadRequest) (*handlers.ReadResponse, error) {
			return h.Read(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.ReadResponse {
			return &handlers.ReadResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleWrite(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeWriteRequest,
		func(req *handlers.WriteRequest) (*handlers.WriteResponse, error) {
			return h.Write(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.WriteResponse {
			return &handlers.WriteResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleQueryDirectory(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeQueryDirectoryRequest,
		func(req *handlers.QueryDirectoryRequest) (*handlers.QueryDirectoryResponse, error) {
			return h.QueryDirectory(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.QueryDirectoryResponse {
			return &handlers.QueryDirectoryResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleQueryInfo(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeQueryInfoRequest,
		func(req *handlers.QueryInfoRequest) (*handlers.QueryInfoResponse, error) {
			return h.QueryInfo(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.QueryInfoResponse {
			return &handlers.QueryInfoResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleSetInfo(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeSetInfoRequest,
		func(req *handlers.SetInfoRequest) (*handlers.SetInfoResponse, error) {
			return h.SetInfo(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.SetInfoResponse {
			return &handlers.SetInfoResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleEcho(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return handleRequest(
		body,
		handlers.DecodeEchoRequest,
		func(req *handlers.EchoRequest) (*handlers.EchoResponse, error) {
			return h.Echo(ctx, req)
		},
		types.StatusInvalidParameter,
		func(status types.Status) *handlers.EchoResponse {
			return &handlers.EchoResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
		},
	)
}

func handleIoctl(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.Ioctl(ctx, body)
}

func handleCancel(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.Cancel(ctx, body)
}

func handleChangeNotify(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.ChangeNotify(ctx, body)
}

func handleLock(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.Lock(ctx, body)
}

func handleOplockBreak(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *runtime.Runtime, body []byte) (*HandlerResult, error) {
	return h.OplockBreak(ctx, body)
}
