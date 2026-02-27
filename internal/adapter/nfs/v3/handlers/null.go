package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// NullRequest represents a NULL request from an NFS client.
// The NULL procedure takes no arguments per RFC 1813.
//
// This structure exists for consistency with other procedures and to support
// potential future extensions (e.g., debugging parameters).
//
// RFC 1813 Section 3.3.0 specifies the NULL procedure as:
//
//	void NFSPROC3_NULL(void) = 0;
//
// The NULL procedure is defined in RFC 1813 Appendix I and serves as:
//   - A connectivity test - verifies the server is reachable
//   - An RPC validation test - confirms RPC protocol is working
//   - A keep-alive mechanism - maintains network connections
//   - A version check - confirms NFSv3 support
type NullRequest struct {
	// No fields - NULL takes no arguments
}

// NullResponse represents the response to a NULL request.
// The NULL procedure returns no data per RFC 1813.
//
// The response is encoded in XDR format (empty) before being sent to the client.
type NullResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Null handles NFS NULL (RFC 1813 Appendix I, procedure 0).
// No-op ping/health check verifying the NFSv3 service is running and reachable.
// No delegation; returns immediately with empty response (no store access).
// No side effects; stateless operation optimized for minimal overhead.
// Errors: none (NULL always succeeds; context cancellation returns ctx error).
func (h *Handler) Null(
	ctx *NFSHandlerContext,
	req *NullRequest,
) (*NullResponse, error) {
	// ========================================================================
	// Context Cancellation Check
	// ========================================================================
	// Check if the client has disconnected or the request has timed out.
	// While NULL is extremely fast, we should still respect cancellation to
	// avoid wasting resources on abandoned requests (e.g., during server shutdown,
	// load balancer health check timeouts, or client disconnects).
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "NULL: request cancelled", "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := ctx.ClientAddr
	if idx := len(clientIP) - 1; idx >= 0 {
		// Strip port if present (format: "IP:port")
		for i := idx; i >= 0; i-- {
			if clientIP[i] == ':' {
				clientIP = clientIP[:i]
				break
			}
		}
	}

	logger.DebugCtx(ctx.Context, "NULL", "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Optional: store health check
	// ========================================================================
	// NOTE: Health check removed as NULL doesn't have a file handle to decode
	// and shouldn't depend on any particular share. NULL must always succeed
	// per RFC 1813 regardless of backend state.

	logger.DebugCtx(ctx.Context, "NULL: request completed successfully", "client", clientIP)

	// Return empty response - NULL always succeeds
	return &NullResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3OK}}, nil
}
