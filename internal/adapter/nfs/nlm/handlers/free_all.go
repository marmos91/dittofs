package handlers

import (
	"bytes"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
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

// FreeAll handles NLM FREE_ALL (RFC 1813, NLM procedure 23).
// Releases all byte-range locks held by the named client and drains its blocking
// waiters, triggered by the client's lock manager on reboot. This runs the same
// per-share cleanup as NSM SM_NOTIFY crash detection so that a lost or delayed
// SM_NOTIFY does not leave a rebooted client's locks orphaned (defense in depth):
// FREE_ALL and SM_NOTIFY are independent crash signals and either must suffice.
// Errors: none (returns void per NLM specification; decode errors are logged).
func (h *Handler) FreeAll(ctx *NLMHandlerContext) ([]byte, error) {
	req, err := DecodeFreeAllRequest(ctx.Data)
	if err != nil {
		logger.Warn("FREE_ALL: failed to decode request", "error", err)
		return EncodeFreeAllResponse(&FreeAllResponse{})
	}

	logger.Info("FREE_ALL: received",
		"client", req.Name,
		"from", ctx.ClientAddr)

	if req.Name == "" {
		logger.Warn("FREE_ALL: empty client name; ignoring", "from", ctx.ClientAddr)
		return EncodeFreeAllResponse(&FreeAllResponse{})
	}

	// Release the named client's locks via the adapter-wired cleanup, which
	// iterates all shares' lock managers and the blocking queue. Idempotent and
	// safe if the client held no locks. Cleanup is gated on grace period inside
	// the wired routine, mirroring NSM crash handling.
	if h.crashCleanup != nil {
		h.crashCleanup(req.Name)
	} else {
		logger.Warn("FREE_ALL: no crash-cleanup wired; locks not released",
			"client", req.Name)
	}

	return EncodeFreeAllResponse(&FreeAllResponse{})
}
