package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/clients"
)

// nextConnID is the global monotonic counter for Connection.ID. Starts at 1
// so the zero value remains a sentinel for "no connection assigned" in
// internal code paths that don't go through NewConnection.
var nextConnID atomic.Uint64

// Connection handles a single SMB2 client connection.
type Connection struct {
	// ID is a stable, process-wide monotonic identifier for this TCP
	// connection. Used by Session.Channels to key per-channel signing state
	// when SMB2 multi-channel session binding is in play (MS-SMB2 §3.3.5.5.2).
	ID uint64

	server *Adapter
	conn   net.Conn

	// Concurrent request handling
	requestSem chan struct{}    // Semaphore to limit concurrent requests
	wg         sync.WaitGroup   // Track active requests for graceful shutdown
	writeMu    smb.LockedWriter // Protects connection writes (replies must be serialized)

	// Session tracking for cleanup on disconnect
	sessionsMu sync.Mutex          // Protects sessions map
	sessions   map[uint64]struct{} // Sessions created on this connection

	// CryptoState holds per-connection cryptographic negotiation state
	// including the preauth integrity hash chain for SMB 3.1.1.
	// Created eagerly for all connections.
	CryptoState *smb.ConnectionCryptoState
}

// NewConnection creates a new SMB connection handler.
// CryptoState is created eagerly for all connections to support SMB 3.1.1
// preauth integrity hash computation from the very first message.
func NewConnection(server *Adapter, conn net.Conn) *Connection {
	return &Connection{
		ID:          nextConnID.Add(1),
		server:      server,
		conn:        conn,
		requestSem:  make(chan struct{}, server.config.MaxRequestsPerConnection),
		sessions:    make(map[uint64]struct{}),
		CryptoState: smb.NewConnectionCryptoState(),
	}
}

// smbClientID returns the client registry key for an SMB session.
func smbClientID(sessionID uint64) string {
	return fmt.Sprintf("smb-%d", sessionID)
}

// TrackSession records a session as belonging to this connection.
// Called when SESSION_SETUP completes successfully.
func (c *Connection) TrackSession(sessionID uint64) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	c.sessions[sessionID] = struct{}{}
	logger.Debug("Tracking session on connection",
		"sessionID", sessionID,
		"address", c.conn.RemoteAddr().String())

	// Register with the client registry for operational visibility.
	if rt := c.server.Registry; rt != nil {
		rt.Clients().Register(&clients.ClientRecord{
			ClientID: smbClientID(sessionID),
			Protocol: "smb",
			Address:  c.conn.RemoteAddr().String(),
			SMB:      &clients.SmbDetails{SessionID: sessionID},
		})
	}
}

// UntrackSession removes a session from this connection's tracking.
// Called when LOGOFF is processed.
//
// LOGOFF is processed synchronously on the read loop to guarantee the
// LoggedOff flag is visible before the next request is read. Registry
// deregistration is done asynchronously to avoid adding lock contention
// in this critical path — contention widens the race window, causing
// the signing verifier to return STATUS_ACCESS_DENIED instead of
// STATUS_USER_SESSION_DELETED.
func (c *Connection) UntrackSession(sessionID uint64) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	delete(c.sessions, sessionID)
	logger.Debug("Untracking session from connection",
		"sessionID", sessionID,
		"address", c.conn.RemoteAddr().String())

	// Deregister from the client registry asynchronously.
	if rt := c.server.Registry; rt != nil {
		go rt.Clients().Deregister(smbClientID(sessionID))
	}
}

// connInfo builds the ConnInfo struct used by internal/ dispatch functions.
func (c *Connection) connInfo() *smb.ConnInfo {
	ci := &smb.ConnInfo{
		ConnID:         c.ID,
		Conn:           c.conn,
		Handler:        c.server.handler,
		SessionManager: c.server.sessionManager,
		WriteMu:        &c.writeMu,
		WriteTimeout:   c.server.config.Timeouts.Write,
		SessionTracker: c, // overwritten below
		CryptoState:    c.CryptoState,
		EncryptionMiddleware: encryption.NewEncryptionMiddleware(
			func(sessionID uint64) (encryption.EncryptableSession, bool) {
				sess, ok := c.server.handler.GetSession(sessionID)
				if !ok {
					return nil, false
				}
				return sess, true
			},
		),
		DecryptFailures: &atomic.Int32{},
		// Per MS-SMB2 3.3.1.1: Initialize CommandSequenceWindow with {0}.
		// Max size = 2 * MaxSessionCredits to allow generous window growth.
		SequenceWindow: smb.NewSequenceWindowForConnection(c.server.sessionManager),
	}
	// Wrap the session tracker so that session creation/deletion also
	// registers/deregisters the ConnInfo in the adapter's session→connection
	// map. This enables lease break notifications to be routed to the correct
	// TCP connection.
	ci.SessionTracker = &connRegistryTracker{
		inner:         c,
		connInfo:      ci,
		sessionConns:  &c.server.sessionConns,
		sessionLookup: c.server.handler,
	}
	return ci
}

// Serve handles all SMB2 requests for this connection.
//
// It delegates request reading to smb.ReadRequest (framing), compound handling
// to smb.ProcessCompoundRequest, and single request dispatch to smb.ProcessSingleRequest.
//
// The connection is automatically closed when:
// - The context is cancelled (server shutdown)
// - An idle timeout occurs
// - A read or write timeout occurs
// - An unrecoverable error occurs
// - The client closes the connection
func (c *Connection) Serve(ctx context.Context) {
	defer c.handleConnectionClose()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New SMB connection", "address", clientAddr)

	// Set initial idle timeout (read-only so writes are never cut short)
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set read deadline", "address", clientAddr, "error", err)
		}
	}

	ci := c.connInfo()
	verifier := smb.NewSessionSigningVerifier(c.server.handler, c.conn, c.CryptoState, c.ID)
	handleSMB1 := func(_ context.Context, message []byte) error {
		return smb.HandleSMB1Negotiate(ci, message)
	}

	for {
		// Check for context cancellation before processing next request
		select {
		case <-ctx.Done():
			logger.Debug("SMB connection closed due to context cancellation", "address", clientAddr)
			return
		case <-c.server.Shutdown:
			logger.Debug("SMB connection closed due to server shutdown", "address", clientAddr)
			return
		default:
		}

		// Read and process the request via framing layer.
		// Pass EncryptionMiddleware so 0xFD transform headers are decrypted transparently.
		hdr, body, remainingCompound, isEncrypted, err := smb.ReadRequest(
			ctx, c.conn, c.server.config.MaxMessageSize,
			c.server.config.Timeouts.Read, verifier, ci.EncryptionMiddleware, handleSMB1,
		)
		if err != nil {
			// Track consecutive decryption failures. After 5, drop the connection
			// to prevent brute-force attacks on the AEAD authentication.
			if isDecryptionError(err) {
				failures := ci.DecryptFailures.Add(1)
				logger.Warn("Decryption failure on connection",
					"address", clientAddr,
					"consecutiveFailures", failures,
					"error", err)
				if failures >= 5 {
					logger.Warn("Dropping connection after 5 consecutive decryption failures",
						"address", clientAddr)
					return
				}
				continue // Try reading next message
			}

			switch {
			case err == io.EOF:
				logger.Debug("SMB connection closed by client", "address", clientAddr)
			case isNetTimeout(err):
				logger.Debug("SMB connection timed out", "address", clientAddr, "error", err)
			case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
				logger.Debug("SMB connection cancelled", "address", clientAddr, "error", err)
			default:
				logger.Debug("Error reading SMB request", "address", clientAddr, "error", err)
			}
			return
		}

		// Reset consecutive decryption failure counter on successful read
		ci.DecryptFailures.Store(0)

		// Reconstruct raw message (header + body) for dispatch hooks.
		// The hooks need the original wire bytes for preauth integrity hash computation.
		rawMessage := make([]byte, header.HeaderSize+len(body))
		copy(rawMessage, hdr.Encode())
		copy(rawMessage[header.HeaderSize:], body)

		// LOGOFF must be processed synchronously to guarantee the LoggedOff
		// flag is set before the next request is read from the connection.
		// Without this, a concurrent goroutine for the next request could
		// race with the LOGOFF handler, causing the signing verifier to
		// return STATUS_ACCESS_DENIED instead of STATUS_USER_SESSION_DELETED.
		if hdr.Command == types.CommandLogoff && len(remainingCompound) == 0 {
			if err := smb.ProcessSingleRequest(ctx, hdr, body, rawMessage, ci, isEncrypted, nil); err != nil {
				logger.Debug("Error processing LOGOFF request", "address", clientAddr, "messageID", hdr.MessageID, "error", err)
			}
			continue
		}

		// Acquire semaphore slot and track the request goroutine
		c.requestSem <- struct{}{}
		c.wg.Add(1)

		if len(remainingCompound) > 0 {
			// Copy compound data to avoid races (goroutine owns this copy)
			compoundData := make([]byte, len(remainingCompound))
			copy(compoundData, remainingCompound)

			go func(reqHeader *header.SMB2Header, reqBody, raw []byte, encrypted bool) {
				defer c.handleRequestPanic(clientAddr, reqHeader.MessageID)
				asyncCallback := c.makeAsyncNotifyCallback(ci)
				smb.ProcessCompoundRequest(ctx, reqHeader, reqBody, raw, compoundData, ci, encrypted, asyncCallback)
			}(hdr, body, rawMessage, isEncrypted)
		} else {
			go func(reqHeader *header.SMB2Header, reqBody, raw []byte, encrypted bool) {
				defer c.handleRequestPanic(clientAddr, reqHeader.MessageID)

				asyncCallback := c.makeAsyncNotifyCallback(ci)
				if err := smb.ProcessSingleRequest(ctx, reqHeader, reqBody, raw, ci, encrypted, asyncCallback); err != nil {
					logger.Debug("Error processing SMB request", "address", clientAddr, "messageID", reqHeader.MessageID, "error", err)
				}
			}(hdr, body, rawMessage, isEncrypted)
		}

		// Reset idle timeout after reading request (read-only)
		if c.server.config.Timeouts.Idle > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
				logger.Warn("Failed to reset read deadline", "address", clientAddr, "error", err)
			}
		}
	}
}

// makeAsyncNotifyCallback creates the async callback for CHANGE_NOTIFY responses.
func (c *Connection) makeAsyncNotifyCallback(ci *smb.ConnInfo) handlers.AsyncResponseCallback {
	return func(sessionID, messageID, asyncId uint64, response *handlers.ChangeNotifyResponse) error {
		return smb.SendAsyncChangeNotifyResponse(sessionID, messageID, asyncId, response, ci)
	}
}

// handleConnectionClose handles cleanup and panic recovery for the connection.
func (c *Connection) handleConnectionClose() {
	clientAddr := c.conn.RemoteAddr().String()

	if r := recover(); r != nil {
		logger.Error("Panic in SMB connection handler", "address", clientAddr, "error", r)
	}

	start := time.Now()
	c.wg.Wait()
	waitDur := time.Since(start)

	cleanupStart := time.Now()
	c.cleanupSessions()
	cleanupDur := time.Since(cleanupStart)

	_ = c.conn.Close()
	logger.Debug("SMB connection closed",
		"address", clientAddr,
		"waitDuration", waitDur,
		"cleanupDuration", cleanupDur)
}

// cleanupSessions cleans up all sessions that were created on this connection.
func (c *Connection) cleanupSessions() {
	clientAddr := c.conn.RemoteAddr().String()

	c.sessionsMu.Lock()
	sessions := make([]uint64, 0, len(c.sessions))
	for sessionID := range c.sessions {
		sessions = append(sessions, sessionID)
	}
	c.sessions = make(map[uint64]struct{})
	c.sessionsMu.Unlock()

	if len(sessions) == 0 {
		return
	}

	logger.Debug("Cleaning up sessions on connection close",
		"address", clientAddr,
		"sessionCount", len(sessions))

	// Clean up session→ConnInfo mapping so lease break notifications
	// are no longer routed to this (now-closed) connection.
	// Also deregister from the client registry.
	rt := c.server.Registry
	for _, sessionID := range sessions {
		c.server.sessionConns.Delete(sessionID)
		if rt != nil {
			rt.Clients().Deregister(smbClientID(sessionID))
		}
	}

	// State leak detection: log state before cleanup starts
	c.server.handler.LogStateSnapshot("Connection close: state before session cleanup", 0)

	// Signal the cleanup barrier BEFORE starting any cleanup work.
	// This ensures that WaitForCleanup() in a new connection's SESSION_SETUP
	// will block even if the new goroutine reaches SESSION_SETUP before we
	// enter CleanupSession. Without this pre-increment, there is a race window
	// where cleanupWg is 0 (Add hasn't been called yet) and WaitForCleanup
	// returns immediately, allowing the new session to access stale state.
	c.server.handler.SignalPendingCleanup(len(sessions))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Guard the cleanup loop against panics. If CleanupSession panics,
	// call Done() for remaining sessions so cleanupWg doesn't get stuck,
	// then re-panic to preserve the original failure signal.
	cleaned := 0
	defer func() {
		if r := recover(); r != nil {
			remaining := len(sessions) - cleaned
			for i := 0; i < remaining; i++ {
				c.server.handler.SignalCleanupDone()
			}
			panic(r)
		}
	}()

	for _, sessionID := range sessions {
		c.server.handler.CleanupSession(ctx, sessionID, true /* transport disconnect */)
		cleaned++
	}

	// State leak detection: log final state after all session cleanups
	c.server.handler.LogStateSnapshot("Connection close: state after all session cleanups", 0)

	logger.Debug("Session cleanup complete",
		"address", clientAddr,
		"sessionCount", len(sessions))
}

// handleRequestPanic handles cleanup and panic recovery for individual requests.
func (c *Connection) handleRequestPanic(clientAddr string, messageID uint64) {
	<-c.requestSem
	c.wg.Done()

	if r := recover(); r != nil {
		stack := string(debug.Stack())
		logger.Error("Panic in SMB request handler",
			"address", clientAddr,
			"messageID", messageID,
			"error", r,
			"stack", stack)
	}
}

// isNetTimeout reports whether err is a network timeout error.
func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isDecryptionError reports whether err is a transform header decryption failure.
func isDecryptionError(err error) bool {
	return errors.Is(err, encryption.ErrDecryptFailed)
}
