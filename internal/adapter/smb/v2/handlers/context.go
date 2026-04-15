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
	// GetPreauthHash returns a copy of the current connection-level preauth integrity hash value.
	GetPreauthHash() [64]byte
	// InitSessionPreauthHash creates a per-session preauth hash from the connection
	// hash and chains the given SESSION_SETUP request bytes into it. Caller must pass
	// the rawMessage from THIS request — must NOT come from any per-connection stash,
	// since concurrent SESSION_SETUPs on a single connection would otherwise overwrite
	// each other's bytes (the cause of issue #362's "Bad SMB2 (sign_algo_id=2)
	// signature" failures in bench.* tests).
	InitSessionPreauthHash(sessionID uint64, ssRequestBytes []byte)
	// UpdateSessionPreauthHash updates the per-session preauth hash with message bytes.
	UpdateSessionPreauthHash(sessionID uint64, message []byte)
	// GetSessionPreauthHash returns the per-session preauth hash (falls back to connection hash).
	GetSessionPreauthHash(sessionID uint64) [64]byte
	// DeleteSessionPreauthHash removes the per-session preauth hash entry.
	DeleteSessionPreauthHash(sessionID uint64)
	// (removed: StashPendingSessionSetup — see #362 fix; rawMessage now flows
	// through SMBHandlerContext.RawRequest into InitSessionPreauthHash.)
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

	// RawRequest is the complete raw SMB2 message bytes (header + body) for
	// THIS request. Used by the SESSION_SETUP handler to chain its own bytes
	// into the per-session preauth integrity hash. Threading the bytes
	// through context (instead of via a per-connection stash) is what fixes
	// the concurrent-SESSION_SETUP race in issue #362. Nil for tests that
	// don't care about preauth hash chaining.
	RawRequest []byte

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

	// RequestAsyncId is the AsyncId from the request header when FlagAsync is set.
	// Used by CANCEL to identify which async operation to cancel.
	RequestAsyncId uint64

	// ConnCryptoState provides access to the per-connection cryptographic state.
	// Used by the NEGOTIATE handler to store negotiation parameters on the
	// connection for subsequent VNEG validation and preauth hash computation.
	// Populated from ConnInfo.CryptoState by prepareDispatch. Nil if no
	// CryptoState is available (e.g., in tests).
	ConnCryptoState CryptoState

	// RequestEncrypted indicates whether the incoming request was received
	// inside an SMB3 Transform Header (protocol ID 0xFD). Used to enforce
	// global and per-share encryption requirements per MS-SMB2 3.3.5.2.1.
	RequestEncrypted bool

	// PostSend is an optional hook invoked by the dispatch layer AFTER the
	// response for this command has been written to the wire. It is used by
	// handlers (currently only CLOSE) to defer async side-effects that must
	// be ordered strictly after their own response.
	//
	// Per MS-SMB2 3.3.4.1: "CHANGE_NOTIFY responses MUST be the last responses
	// sent for the FileId". When CLOSE completes a pending CHANGE_NOTIFY watch
	// with STATUS_NOTIFY_CLEANUP, the cleanup response must be sent AFTER the
	// CLOSE response, otherwise clients that arm their async-receive loop only
	// after consuming the CLOSE response will miss the cleanup and time out.
	//
	// The hook runs on the same goroutine as the dispatch loop after the
	// writeMu for the CLOSE response has been released; the hook is responsible
	// for acquiring writeMu itself (SendMessage handles this transparently).
	PostSend func()
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
