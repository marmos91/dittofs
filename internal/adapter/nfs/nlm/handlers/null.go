package handlers

import (
	"bytes"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/xdr"
)

// NullRequest represents an NLM_NULL request.
// This procedure takes no arguments and is used for ping/health checks.
type NullRequest struct{}

// NullResponse represents an NLM_NULL response.
// This procedure returns no data and is used for ping/health checks.
type NullResponse struct {
	// Status is always NLM4Granted for NULL procedure.
	Status uint32
}

// DecodeNullRequest decodes an NLM_NULL request from XDR format.
// NULL procedure has no arguments, so this always succeeds.
func DecodeNullRequest(data []byte) (*NullRequest, error) {
	return &NullRequest{}, nil
}

// EncodeNullResponse encodes an NLM_NULL response to XDR format.
// Per NLM specification, NULL returns an nlm4_res with granted status.
func EncodeNullResponse(resp *NullResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	// NULL response is an nlm4_res with empty cookie and granted status
	res := &types.NLM4Res{
		Cookie: nil,
		Status: resp.Status,
	}

	if err := nlm_xdr.EncodeNLM4Res(buf, res); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Null handles the NLM_NULL procedure (procedure 0).
//
// NLM_NULL is a ping/health check procedure that takes no arguments
// and returns success. It is used to verify the NLM service is running.
//
// Parameters:
//   - ctx: The NLM handler context (unused for NULL)
//   - req: The NULL request (empty)
//
// Returns:
//   - *NullResponse: Always returns NLM4Granted status
//   - error: Always nil for NULL procedure
func (h *Handler) Null(ctx *NLMHandlerContext, req *NullRequest) (*NullResponse, error) {
	return &NullResponse{
		Status: types.NLM4Granted,
	}, nil
}
