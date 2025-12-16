// Package smb provides SMB2 protocol dispatch and result types.
package smb

import (
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/internal/protocol/smb/v2/handlers"
	"github.com/marmos91/dittofs/pkg/registry"
)

// HandlerResult is an alias for the handlers.HandlerResult type
type HandlerResult = handlers.HandlerResult

// CommandHandler is the signature for SMB2 command handlers
type CommandHandler func(
	ctx *handlers.SMBHandlerContext,
	handler *handlers.Handler,
	reg *registry.Registry,
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
func handleNegotiate(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Negotiate(ctx, body)
}

func handleSessionSetup(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.SessionSetup(ctx, body)
}

func handleLogoff(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Logoff(ctx, body)
}

func handleTreeConnect(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.TreeConnect(ctx, body)
}

func handleTreeDisconnect(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.TreeDisconnect(ctx, body)
}

func handleCreate(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Create(ctx, body)
}

func handleClose(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Close(ctx, body)
}

func handleFlush(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Flush(ctx, body)
}

func handleRead(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Read(ctx, body)
}

func handleWrite(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Write(ctx, body)
}

func handleQueryDirectory(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.QueryDirectory(ctx, body)
}

func handleQueryInfo(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.QueryInfo(ctx, body)
}

func handleSetInfo(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.SetInfo(ctx, body)
}

func handleEcho(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Echo(ctx, body)
}

func handleIoctl(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Ioctl(ctx, body)
}

func handleCancel(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Cancel(ctx, body)
}

func handleChangeNotify(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.ChangeNotify(ctx, body)
}

func handleLock(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.Lock(ctx, body)
}

func handleOplockBreak(ctx *handlers.SMBHandlerContext, h *handlers.Handler, _ *registry.Registry, body []byte) (*HandlerResult, error) {
	return h.OplockBreak(ctx, body)
}
