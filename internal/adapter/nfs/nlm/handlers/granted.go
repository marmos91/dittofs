package handlers

import (
	"bytes"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/xdr"
)

// GrantedRequest represents an NLM_GRANTED request (callback response).
//
// When the server sends NLM_GRANTED to a client, the client may respond
// with NLM_GRANTED_RES. However, most implementations ignore the response.
// Per CONTEXT.md, we implement the procedure for protocol completeness
// but simply acknowledge receipt.
type GrantedRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Exclusive indicates the lock type.
	Exclusive bool

	// Lock contains the lock parameters.
	Lock types.NLM4Lock
}

// GrantedResponse represents an NLM_GRANTED response.
type GrantedResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	Status uint32
}

// DecodeGrantedRequest decodes an NLM_GRANTED request from XDR format.
func DecodeGrantedRequest(data []byte) (*GrantedRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4GrantedArgs(r)
	if err != nil {
		return nil, err
	}

	return &GrantedRequest{
		Cookie:    args.Cookie,
		Exclusive: args.Exclusive,
		Lock:      args.Lock,
	}, nil
}

// EncodeGrantedResponse encodes an NLM_GRANTED response to XDR format.
func EncodeGrantedResponse(resp *GrantedResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	res := &types.NLM4Res{
		Cookie: resp.Cookie,
		Status: resp.Status,
	}

	if err := nlm_xdr.EncodeNLM4Res(buf, res); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Granted handles the NLM_GRANTED procedure (procedure 5).
//
// NLM_GRANTED is a callback from server to client. When the server sends
// NLM_GRANTED to notify a client that a blocked lock is now available,
// the client responds with NLM_GRANTED or NLM_GRANTED_RES.
//
// If we receive this as a server, it means:
// 1. We sent an NLM_GRANTED callback to a client
// 2. The client is acknowledging receipt via synchronous response (rare)
//
// Most implementations use asynchronous callbacks (NLM_GRANTED_MSG) which
// don't require a response, but we handle the synchronous case for
// protocol completeness.
//
// Per CONTEXT.md: Simply return NLM4_GRANTED to acknowledge receipt.
//
// Parameters:
//   - ctx: The NLM handler context with auth and client info
//   - req: The GRANTED request (really a callback response)
//
// Returns:
//   - *GrantedResponse: Always NLM4_GRANTED
//   - error: System-level errors only
func (h *Handler) Granted(ctx *NLMHandlerContext, req *GrantedRequest) (*GrantedResponse, error) {
	logger.Debug("NLM GRANTED received (callback ack)",
		"client", ctx.ClientAddr,
		"caller", req.Lock.CallerName,
		"exclusive", req.Exclusive,
		"offset", req.Lock.Offset,
		"length", req.Lock.Length)

	// Simply acknowledge receipt
	return &GrantedResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Granted,
	}, nil
}
