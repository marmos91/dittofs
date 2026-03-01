// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// CryptoState is an interface for per-connection cryptographic state.
// This abstraction avoids circular imports between handlers/ and smb/ packages.
// The concrete implementation is ConnectionCryptoState in internal/adapter/smb/.
type CryptoState interface {
	// SetDialect records the negotiated dialect.
	SetDialect(d types.Dialect)
	// GetDialect returns the negotiated dialect.
	GetDialect() types.Dialect
	// SetCipherId records the selected encryption cipher.
	SetCipherId(id uint16)
	// SetPreauthIntegrityHashId records the selected hash algorithm.
	SetPreauthIntegrityHashId(id uint16)
	// SetServerGUID records the server's GUID.
	SetServerGUID(guid [16]byte)
	// GetServerGUID returns the server's GUID.
	GetServerGUID() [16]byte
	// SetServerCapabilities records the server's capabilities.
	SetServerCapabilities(caps types.Capabilities)
	// GetServerCapabilities returns the server's capabilities.
	GetServerCapabilities() types.Capabilities
	// SetServerSecurityMode records the server's security mode.
	SetServerSecurityMode(mode types.SecurityMode)
	// GetServerSecurityMode returns the server's security mode.
	GetServerSecurityMode() types.SecurityMode
	// SetClientGUID records the client's GUID.
	SetClientGUID(guid [16]byte)
	// GetClientGUID returns the client's GUID.
	GetClientGUID() [16]byte
	// SetClientCapabilities records the client's capabilities.
	SetClientCapabilities(caps types.Capabilities)
	// GetClientCapabilities returns the client's capabilities.
	GetClientCapabilities() types.Capabilities
	// SetClientSecurityMode records the client's security mode.
	SetClientSecurityMode(mode types.SecurityMode)
	// GetClientSecurityMode returns the client's security mode.
	GetClientSecurityMode() types.SecurityMode
	// SetClientDialects records the client's offered dialects.
	SetClientDialects(dialects []types.Dialect)
	// GetClientDialects returns the client's offered dialect list.
	GetClientDialects() []types.Dialect
	// SetSigningAlgorithmId records the selected signing algorithm.
	SetSigningAlgorithmId(id uint16)
	// GetSigningAlgorithmId returns the selected signing algorithm.
	GetSigningAlgorithmId() uint16
	// GetCipherId returns the selected encryption cipher.
	GetCipherId() uint16
	// GetPreauthHash returns a copy of the current preauth integrity hash value.
	GetPreauthHash() [64]byte
}

// SMBHandlerContext carries per-request state through all SMB2 handlers.
// It provides session identity, tree connection info, share-level permissions,
// and the async notification callback for CHANGE_NOTIFY operations.
// Created by the dispatch layer for each incoming SMB2 request.
type SMBHandlerContext struct {
	// Context for cancellation and deadlines
	Context context.Context

	// ClientAddr is the remote address of the client
	ClientAddr string

	// SessionID from the request (0 before SESSION_SETUP completes)
	SessionID uint64

	// TreeID from the request (0 before TREE_CONNECT)
	TreeID uint32

	// MessageID from the request header
	MessageID uint64

	// ShareName resolved from TreeID (populated after TREE_CONNECT)
	ShareName string

	// IsGuest indicates guest/anonymous session
	IsGuest bool

	// Username for authenticated sessions
	Username string
	Domain   string

	// User is the authenticated DittoFS user (nil for guest sessions)
	// This is set from the session during request handling.
	User *models.User

	// Permission is the user's permission level for the current share
	// This is resolved during TREE_CONNECT and used for access control.
	Permission models.SharePermission

	// AsyncNotifyCallback is used for sending async CHANGE_NOTIFY responses.
	// Set by the connection layer to enable async notification delivery.
	// If nil, notifications are logged but not sent.
	AsyncNotifyCallback AsyncResponseCallback

	// ConnCryptoState provides access to the per-connection cryptographic state.
	// Used by the NEGOTIATE handler to store negotiation parameters on the
	// connection for subsequent VNEG validation and preauth hash computation.
	// Populated from ConnInfo.CryptoState by prepareDispatch. Nil if no
	// CryptoState is available (e.g., in tests).
	ConnCryptoState CryptoState
}

// NewSMBHandlerContext creates a new handler context from request parameters.
// The context is populated with identifiers extracted from the SMB2 header.
// Share-level fields (ShareName, User, Permission) are set later by handlers.
func NewSMBHandlerContext(ctx context.Context, clientAddr string, sessionID uint64, treeID uint32, messageID uint64) *SMBHandlerContext {
	return &SMBHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		SessionID:  sessionID,
		TreeID:     treeID,
		MessageID:  messageID,
	}
}

// WithUser returns a copy of the context with user identity populated
func (c *SMBHandlerContext) WithUser(user *models.User, permission models.SharePermission) *SMBHandlerContext {
	newCtx := *c
	newCtx.User = user
	newCtx.Permission = permission
	if user != nil {
		newCtx.Username = user.Username
		newCtx.IsGuest = false
	}
	return &newCtx
}
