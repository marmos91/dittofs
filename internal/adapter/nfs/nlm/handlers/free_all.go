package handlers

import (
	"bytes"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// FreeAllRequest represents an NLM4_FREE_ALL request.
//
// Per NLM specification:
//
//	struct nlm_notify {
//	    string name<MAXNAMELEN>;  // Client hostname
//	    int32  state;             // Client state (unused in FREE_ALL)
//	};
//
// Note: The state field is present in the wire format but not used for FREE_ALL.
// We only need the name to identify which client's locks to release.
type FreeAllRequest struct {
	// Name is the client hostname whose locks should be released.
	// This matches the caller_name field used when locks were acquired.
	Name string

	// State is the client's NSM state (unused for FREE_ALL).
	State int32
}

// FreeAllResponse represents an NLM4_FREE_ALL response.
//
// Per NLM specification, FREE_ALL has no response body (void).
type FreeAllResponse struct{}

// DecodeFreeAllRequest decodes an NLM4_FREE_ALL request from XDR format.
//
// Parameters:
//   - data: XDR-encoded request bytes
//
// Returns:
//   - *FreeAllRequest: Decoded request
//   - error: Decoding error
func DecodeFreeAllRequest(data []byte) (*FreeAllRequest, error) {
	r := bytes.NewReader(data)

	// Decode name string
	name, err := xdr.DecodeString(r)
	if err != nil {
		return nil, err
	}

	// Decode state (int32) - not used but must be read
	state, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, err
	}

	return &FreeAllRequest{
		Name:  name,
		State: state,
	}, nil
}

// EncodeFreeAllResponse encodes an NLM4_FREE_ALL response to XDR format.
//
// Per NLM specification, FREE_ALL returns void (no response body).
// Returns an empty byte slice.
func EncodeFreeAllResponse(_ *FreeAllResponse) ([]byte, error) {
	return []byte{}, nil
}

// FreeAll handles the NLM4_FREE_ALL procedure (procedure 23).
//
// FREE_ALL releases all locks held by a specific client. This is called
// by NSM (via rpc.statd) when a client crashes and reboots.
//
// ARCHITECTURE NOTE:
// This handler decodes and logs the FREE_ALL request for auditing. The actual
// lock cleanup across ALL shares is coordinated by the adapter's OnClientCrash
// callback, which has access to all shares' lock managers. Each NLM handler
// instance only serves one share, so comprehensive cleanup must happen at the
// adapter level.
//
// Parameters:
//   - ctx: The NLM handler context with request data
//
// Returns:
//   - []byte: Empty response (FREE_ALL returns void)
//   - error: Always nil (best effort, errors are logged)
func (h *Handler) FreeAll(ctx *NLMHandlerContext) ([]byte, error) {
	req, err := DecodeFreeAllRequest(ctx.Data)
	if err != nil {
		logger.Warn("FREE_ALL: failed to decode request", "error", err)
		return EncodeFreeAllResponse(&FreeAllResponse{})
	}

	logger.Info("FREE_ALL: received",
		"client", req.Name,
		"from", ctx.ClientAddr)

	// The actual lock release is triggered by the adapter's OnClientCrash
	// callback, which iterates all shares and their lock managers.
	// This handler serves as the NLM RPC endpoint for logging and validation.

	return EncodeFreeAllResponse(&FreeAllResponse{})
}
