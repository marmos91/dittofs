package smb

import (
	"context"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// Connection handles a single SMB2 client connection.
type Connection struct {
	server *Adapter
	conn   net.Conn

	// Concurrent request handling
	requestSem chan struct{}  // Semaphore to limit concurrent requests
	wg         sync.WaitGroup // Track active requests for graceful shutdown
	writeMu    smb.LockedWriter // Protects connection writes (replies must be serialized)

	// Session tracking for cleanup on disconnect
	sessionsMu sync.Mutex          // Protects sessions map
	sessions   map[uint64]struct{} // Sessions created on this connection
}

// NewConnection creates a new SMB connection handler.
func NewConnection(server *Adapter, conn net.Conn) *Connection {
	return &Connection{
		server:     server,
		conn:       conn,
		requestSem: make(chan struct{}, server.config.MaxRequestsPerConnection),
		sessions:   make(map[uint64]struct{}),
	}
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
}

// UntrackSession removes a session from this connection's tracking.
// Called when LOGOFF is processed.
func (c *Connection) UntrackSession(sessionID uint64) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	delete(c.sessions, sessionID)
	logger.Debug("Untracking session from connection",
		"sessionID", sessionID,
		"address", c.conn.RemoteAddr().String())
}

// connInfo builds the ConnInfo struct used by internal/ dispatch functions.
func (c *Connection) connInfo() *smb.ConnInfo {
	return &smb.ConnInfo{
		Conn:           c.conn,
		Handler:        c.server.handler,
		SessionManager: c.server.sessionManager,
		WriteMu:        &c.writeMu,
		WriteTimeout:   c.server.config.Timeouts.Write,
		SessionTracker: c,
	}
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

	// Set initial idle timeout
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set deadline", "address", clientAddr, "error", err)
		}
	}

	ci := c.connInfo()
	verifier := smb.NewSessionSigningVerifier(c.server.handler, c.conn)
	handleSMB1 := func(_ context.Context, _ []byte) error {
		return smb.HandleSMB1Negotiate(ci)
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

		// Read and process the request via framing layer
		hdr, body, remainingCompound, err := smb.ReadRequest(
			ctx, c.conn, c.server.config.MaxMessageSize,
			c.server.config.Timeouts.Read, verifier, handleSMB1,
		)
		if err != nil {
			if err == io.EOF {
				logger.Debug("SMB connection closed by client", "address", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("SMB connection timed out", "address", clientAddr, "error", err)
			} else if err == context.Canceled || err == context.DeadlineExceeded {
				logger.Debug("SMB connection cancelled", "address", clientAddr, "error", err)
			} else {
				logger.Debug("Error reading SMB request", "address", clientAddr, "error", err)
			}
			return
		}

		// Check if this is the start of a compound request
		if len(remainingCompound) > 0 {
			c.requestSem <- struct{}{}
			c.wg.Add(1)

			// Copy compound data to avoid races (goroutine owns this copy)
			compoundData := make([]byte, len(remainingCompound))
			copy(compoundData, remainingCompound)

			go func() {
				defer c.handleRequestPanic(clientAddr, hdr.MessageID)
				smb.ProcessCompoundRequest(ctx, hdr, body, compoundData, ci)
			}()
		} else {
			c.requestSem <- struct{}{}
			c.wg.Add(1)

			go func(reqHeader *header.SMB2Header, reqBody []byte) {
				defer c.handleRequestPanic(clientAddr, reqHeader.MessageID)

				asyncCallback := c.makeAsyncNotifyCallback(ci)
				if err := smb.ProcessSingleRequest(ctx, reqHeader, reqBody, ci, asyncCallback); err != nil {
					logger.Debug("Error processing SMB request", "address", clientAddr, "messageId", reqHeader.MessageID, "error", err)
				}
			}(hdr, body)
		}

		// Reset idle timeout after reading request
		if c.server.config.Timeouts.Idle > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
				logger.Warn("Failed to reset deadline", "address", clientAddr, "error", err)
			}
		}
	}
}

// makeAsyncNotifyCallback creates the async callback for CHANGE_NOTIFY responses.
func (c *Connection) makeAsyncNotifyCallback(ci *smb.ConnInfo) handlers.AsyncResponseCallback {
	return func(sessionID, messageID uint64, response *handlers.ChangeNotifyResponse) error {
		return smb.SendAsyncChangeNotifyResponse(sessionID, messageID, response, ci)
	}
}

// handleConnectionClose handles cleanup and panic recovery for the connection.
func (c *Connection) handleConnectionClose() {
	clientAddr := c.conn.RemoteAddr().String()

	if r := recover(); r != nil {
		logger.Error("Panic in SMB connection handler", "address", clientAddr, "error", r)
	}

	c.wg.Wait()
	c.cleanupSessions()
	_ = c.conn.Close()
	logger.Debug("SMB connection closed", "address", clientAddr)
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, sessionID := range sessions {
		c.server.handler.CleanupSession(ctx, sessionID)
	}

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
			"messageId", messageID,
			"error", r,
			"stack", stack)
	}
}
