package nfs

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc/gss"
)

// sendGSSReply sends an RPC reply with a GSS verifier for RPCSEC_GSS DATA requests.
//
// Per RFC 2203 Section 5.3.3.2, the reply verifier for DATA requests contains
// the MIC of the XDR-encoded sequence number. This proves to the client that
// the server holds the session key.
//
// For krb5i (integrity): the reply body is wrapped in rpc_gss_integ_data with a MIC.
// For krb5p (privacy): the reply body is wrapped in rpc_gss_priv_data with encryption.
// For krb5 (auth-only): the reply body is sent as-is.
//
// Parameters:
//   - xid: Transaction ID for the reply
//   - data: XDR-encoded procedure results
//   - sessionInfo: GSS session info with session key, sequence number, and service level
//
// Returns an error if verifier computation or reply construction fails.
func (c *NFSConnection) sendGSSReply(xid uint32, data []byte, sessionInfo *gss.GSSSessionInfo) error {
	replyData := data

	// Wrap reply body based on service level.
	// If wrapping fails, return error rather than sending unwrapped data,
	// which would be rejected by the client and could desync the session.
	switch sessionInfo.Service {
	case gss.RPCGSSSvcIntegrity:
		// krb5i: wrap reply body with MIC
		wrapped, err := gss.WrapIntegrity(sessionInfo.SessionKey, sessionInfo.SeqNum, data)
		if err != nil {
			return fmt.Errorf("wrap GSS integrity reply: %w", err)
		}
		replyData = wrapped

	case gss.RPCGSSSvcPrivacy:
		// krb5p: wrap reply body with encryption
		wrapped, err := gss.WrapPrivacy(sessionInfo.SessionKey, sessionInfo.SeqNum, data)
		if err != nil {
			return fmt.Errorf("wrap GSS privacy reply: %w", err)
		}
		replyData = wrapped

	default:
		// krb5 (svc_none): reply body sent as-is
	}

	// Compute the reply verifier: MIC of the sequence number
	mic, err := gss.ComputeReplyVerifier(sessionInfo.SessionKey, sessionInfo.SeqNum)
	if err != nil {
		return fmt.Errorf("compute GSS reply verifier: %w", err)
	}

	// Wrap MIC into OpaqueAuth verifier
	verifier := gss.WrapReplyVerifier(mic)

	// Build reply with GSS verifier
	reply, err := rpc.MakeGSSSuccessReply(xid, replyData, verifier)
	if err != nil {
		return fmt.Errorf("make GSS reply: %w", err)
	}

	return c.writeReply(xid, reply)
}

// sendReply sends an RPC reply to the client.
//
// It applies write timeout if configured, constructs the RPC success reply,
// and writes it to the connection.
//
// This method is thread-safe and can be called from multiple goroutines.
// Writes are serialized using writeMu to prevent concurrent writes from
// corrupting the TCP stream.
//
// Returns an error if:
// - Write timeout cannot be set
// - Reply construction fails
// - Network write fails
func (c *NFSConnection) sendReply(xid uint32, data []byte) error {
	reply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		return fmt.Errorf("make reply: %w", err)
	}

	return c.writeReply(xid, reply)
}

// writeReply writes a complete RPC reply to the connection.
//
// This is the core method for sending replies. It handles:
//   - Serializing writes with a mutex to prevent TCP stream corruption
//   - Setting write deadlines for timeout handling
//   - Logging the sent reply
//
// The reply parameter must be a complete RPC message including the fragment
// header. Use this for pre-formatted replies from MakeSuccessReply,
// MakeErrorReply, or MakeProgMismatchReply.
//
// Parameters:
//   - xid: Transaction ID for logging purposes
//   - reply: Complete RPC reply including fragment header
//
// Returns an error if:
//   - Write deadline cannot be set
//   - Network write fails
func (c *NFSConnection) writeReply(xid uint32, reply []byte) error {
	// Serialize all connection writes to prevent corruption
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.server.config.Timeouts.Write > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Write)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	_, err := c.conn.Write(reply)
	if err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	logger.Debug("Sent reply", "xid", fmt.Sprintf("0x%x", xid), "bytes", bytesize.ByteSize(len(reply)))
	return nil
}
