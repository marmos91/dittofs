package nfs

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	mount_handlers "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// handleRPCCall dispatches an RPC call to the appropriate handler.
//
// It routes calls to either NFS or MOUNT handlers based on the program number,
// records metrics, and sends the reply back to the client.
//
// The context is passed through to handlers to enable cancellation of
// long-running operations like large file reads/writes or directory scans.
//
// Returns an error if:
// - Context is cancelled during processing
// - Handler returns an error
// - Reply cannot be sent
func (c *NFSConnection) handleRPCCall(ctx context.Context, call *rpc.RPCCallMessage, procedureData []byte) error {
	var replyData []byte
	var err error

	clientAddr := c.conn.RemoteAddr().String()

	logger.Debug("RPC Call Details", "program", call.Program, "version", call.Version, "procedure", call.Procedure)

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("RPC call cancelled before handler dispatch", "xid", fmt.Sprintf("0x%x", call.XID), "client", clientAddr, "error", ctx.Err())
		return ctx.Err()
	default:
	}

	// ====================================================================
	// RPCSEC_GSS Interception (before program dispatch)
	// ====================================================================
	//
	// Auth flavor 6 (RPCSEC_GSS) must be intercepted here, before routing
	// to NFS/Mount/NLM/NSM handlers. GSS control messages (INIT/DESTROY)
	// are handled entirely here and never reach NFS handlers.
	// GSS DATA messages have their procedureData replaced with the unwrapped
	// arguments and GSS identity injected into the context.
	if call.GetAuthFlavor() == rpc.AuthRPCSECGSS && c.server.gssProcessor != nil {
		gssResult := c.server.gssProcessor.Process(ctx, call.GetAuthBody(), call.GetVerifierBody(), procedureData)

		// Handle GSS processing errors (CREDPROBLEM / CTXPROBLEM)
		if gssResult.Err != nil {
			// Determine the auth_stat from the structured result field
			authStat := gssResult.AuthStat
			if authStat == 0 {
				authStat = rpc.RPCSECGSSCredProblem // default
			}
			if gssResult.GSSReply != nil {
				// INIT failure with encoded error reply - send as control response
				reply, makeErr := rpc.MakeGSSSuccessReply(call.XID, gssResult.GSSReply,
					rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}})
				if makeErr != nil {
					return fmt.Errorf("make GSS error reply: %w", makeErr)
				}
				return c.writeReply(call.XID, reply)
			}

			logger.Debug("GSS processing error, sending AUTH_ERROR",
				"xid", fmt.Sprintf("0x%x", call.XID),
				"auth_stat", authStat,
				"error", gssResult.Err)

			authErrReply, makeErr := rpc.MakeAuthErrorReply(call.XID, authStat)
			if makeErr != nil {
				return fmt.Errorf("make auth error reply: %w", makeErr)
			}
			return c.writeReply(call.XID, authErrReply)
		}

		// Silent discard: per RFC 2203, drop the request without reply
		if gssResult.SilentDiscard {
			logger.Debug("GSS silent discard (sequence violation)",
				"xid", fmt.Sprintf("0x%x", call.XID),
				"client", clientAddr)
			return nil
		}

		// Control messages (INIT/DESTROY): send reply directly, no NFS dispatch
		if gssResult.IsControl {
			// Per RFC 2203 Section 5.3.3.2: When the server returns GSS_S_COMPLETE,
			// the reply verifier contains a MIC of the sequence window. This proves
			// to the client that the server successfully established the context.
			//
			// The client code (authgss_refresh in krb5/libtirpc) verifies this MIC
			// after receiving GSS_S_COMPLETE:
			//   seq = htonl(gr.gr_win);
			//   gss_verify_mic(&min_stat, gd->ctx, &bufin, &bufout, &qop_state);
			var verifier rpc.OpaqueAuth
			if gssResult.SessionKey.KeyValue != nil {
				// Compute MIC with FLAG_ACCEPTOR_SUBKEY if we included subkey in AP-REP
				micBytes, micErr := gss.ComputeInitVerifier(
					gssResult.SessionKey,
					gss.DefaultSeqWindowSize,
					gssResult.HasAcceptorSubkey,
				)
				if micErr != nil {
					logger.Debug("Failed to compute INIT verifier MIC", "error", micErr)
					verifier = rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}}
				} else {
					verifier = gss.WrapReplyVerifier(micBytes)
					logger.Debug("GSS INIT verifier MIC computed",
						"mic_len", len(micBytes),
						"mic_hex", fmt.Sprintf("%x", micBytes),
						"has_acceptor_subkey", gssResult.HasAcceptorSubkey,
					)
				}
			} else {
				// Fallback to AUTH_NULL if no session key (shouldn't happen for successful INIT)
				verifier = rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}}
			}
			reply, makeErr := rpc.MakeGSSSuccessReply(call.XID, gssResult.GSSReply, verifier)
			if makeErr != nil {
				return fmt.Errorf("make GSS control reply: %w", makeErr)
			}
			logger.Debug("GSS INIT sending RPC reply",
				"xid", fmt.Sprintf("0x%x", call.XID),
				"reply_len", len(reply),
				"gss_reply_len", len(gssResult.GSSReply),
				"verifier_flavor", verifier.Flavor,
				"verifier_len", len(verifier.Body),
			)
			return c.writeReply(call.XID, reply)
		}

		// DATA: replace procedureData with unwrapped arguments
		procedureData = gssResult.ProcessedData

		// Inject GSS identity into context for NFS handlers
		if gssResult.Identity != nil {
			ctx = gss.ContextWithIdentity(ctx, gssResult.Identity)
		}

		// Store session info for reply verifier computation and body wrapping
		ctx = gss.ContextWithSessionInfo(ctx, &gss.GSSSessionInfo{
			SessionKey: gssResult.SessionKey,
			SeqNum:     gssResult.SeqNum,
			Service:    gssResult.Service,
		})
	}

	switch call.Program {
	case rpc.ProgramNFS:
		// Route based on NFS version: v3 and v4 are both supported
		switch call.Version {
		case rpc.NFSVersion3:
			replyData, err = c.handleNFSProcedure(ctx, call, procedureData, clientAddr)
		case rpc.NFSVersion4:
			replyData, err = c.handleNFSv4Procedure(ctx, call, procedureData, clientAddr)
		default:
			return c.handleUnsupportedVersion(call, rpc.NFSVersion3, rpc.NFSVersion4, "NFS", clientAddr)
		}

	case rpc.ProgramMount:
		// Mount protocol version handling:
		// - MNT requires v3 (returns v3 file handle format)
		// - Other procedures (NULL, DUMP, UMNT, UMNTALL, EXPORT) are version-agnostic
		// macOS umount uses mount v1 for UMNT, so we accept v1/v2/v3 for those procedures
		if call.Procedure == mount_handlers.MountProcMnt && call.Version != rpc.MountVersion3 {
			return c.handleUnsupportedVersion(call, rpc.MountVersion3, rpc.MountVersion3, "Mount", clientAddr)
		}
		replyData, err = c.handleMountProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramNLM:
		// NLM v4 only per CONTEXT.md decision
		if call.Version != rpc.NLMVersion4 {
			return c.handleUnsupportedVersion(call, rpc.NLMVersion4, rpc.NLMVersion4, "NLM", clientAddr)
		}
		replyData, err = c.handleNLMProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramNSM:
		// NSM v1 only
		if call.Version != rpc.NSMVersion1 {
			return c.handleUnsupportedVersion(call, rpc.NSMVersion1, rpc.NSMVersion1, "NSM", clientAddr)
		}
		replyData, err = c.handleNSMProcedure(ctx, call, procedureData, clientAddr)

	default:
		logger.Debug("Unknown program", "program", call.Program)
		// Send PROC_UNAVAIL error reply for unknown programs
		errorReply, err := rpc.MakeErrorReply(call.XID, rpc.RPCProcUnavail)
		if err != nil {
			return fmt.Errorf("make error reply: %w", err)
		}
		return c.writeReply(call.XID, errorReply)
	}

	if err != nil {
		// Check if error was due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("Handler cancelled", "program", call.Program, "procedure", call.Procedure, "xid", fmt.Sprintf("0x%x", call.XID), "client", clientAddr, "error", err)
			return err
		}

		// Handler returned an error - send RPC SYSTEM_ERR reply to client
		// Per RFC 5531, every RPC call should receive a reply, even on failure
		logger.Debug("Handler error", "program", call.Program, "procedure", call.Procedure, "xid", fmt.Sprintf("0x%x", call.XID), "error", err)

		errorReply, makeErr := rpc.MakeErrorReply(call.XID, rpc.RPCSystemErr)
		if makeErr != nil {
			// Failed to create error reply - return error to close connection
			return fmt.Errorf("make error reply: %w", makeErr)
		}

		// Send the error reply to client
		if sendErr := c.writeReply(call.XID, errorReply); sendErr != nil {
			return fmt.Errorf("send error reply: %w", sendErr)
		}

		// Return original error for logging/metrics but reply was sent
		return fmt.Errorf("handle program %d: %w", call.Program, err)
	}

	// For GSS-authenticated DATA requests, compute GSS reply verifier
	if sessionInfo := gss.SessionInfoFromContext(ctx); sessionInfo != nil {
		return c.sendGSSReply(call.XID, replyData, sessionInfo)
	}

	return c.sendReply(call.XID, replyData)
}

// extractShareName attempts to extract the share name from NFS request data.
//
// Most NFS procedures include a file handle at the beginning of the request.
// This function decodes the file handle using XDR and resolves it to a share
// name using the registry.
//
// Parameters:
//   - ctx: Context for cancellation
//   - data: Raw procedure data (XDR-encoded, file handle as first field)
//
// Returns:
//   - string: Share name, or empty string if no handle present (e.g., NULL procedure)
//   - error: Decoding or resolution error
func (c *NFSConnection) extractShareName(ctx context.Context, data []byte) (string, error) {
	// Decode file handle from XDR request data
	handle, err := xdr.DecodeFileHandleFromRequest(data)
	if err != nil {
		return "", fmt.Errorf("decode file handle: %w", err)
	}

	// No handle present (procedures like NULL, FSINFO don't have handles)
	if handle == nil {
		return "", nil
	}

	// Resolve share name from handle using registry
	shareName, err := c.server.registry.GetShareNameForHandle(ctx, handle)
	if err != nil {
		return "", fmt.Errorf("resolve share from handle: %w", err)
	}

	return shareName, nil
}
