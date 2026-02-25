package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/protocol/nlm/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestRequest represents an NLM_TEST request.
type TestRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Exclusive indicates the lock type to test for.
	// true = would an exclusive lock succeed?
	// false = would a shared lock succeed?
	Exclusive bool

	// Lock contains the lock parameters to test.
	Lock types.NLM4Lock
}

// TestResponse represents an NLM_TEST response.
type TestResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is NLM4Granted if the lock would succeed,
	// NLM4Denied if there's a conflict.
	Status uint32

	// Holder contains information about the conflicting lock.
	// Only populated when Status is NLM4Denied.
	Holder *types.NLM4Holder
}

// DecodeTestRequest decodes an NLM_TEST request from XDR format.
func DecodeTestRequest(data []byte) (*TestRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4TestArgs(r)
	if err != nil {
		return nil, fmt.Errorf("decode NLM4TestArgs: %w", err)
	}

	return &TestRequest{
		Cookie:    args.Cookie,
		Exclusive: args.Exclusive,
		Lock:      args.Lock,
	}, nil
}

// EncodeTestResponse encodes an NLM_TEST response to XDR format.
func EncodeTestResponse(resp *TestResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	res := &types.NLM4TestRes{
		Cookie: resp.Cookie,
		Status: resp.Status,
		Holder: resp.Holder,
	}

	if err := nlm_xdr.EncodeNLM4TestRes(buf, res); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Test handles the NLM_TEST procedure (procedure 1).
//
// NLM_TEST checks if a lock could be granted without actually acquiring it.
// This is used by clients for F_GETLK fcntl() calls.
//
// Per Phase 1 decision: TEST is allowed during grace period (it doesn't
// acquire locks, just tests).
//
// Parameters:
//   - ctx: The NLM handler context with auth and client info
//   - req: The TEST request containing lock parameters
//
// Returns:
//   - *TestResponse: NLM4Granted if lock would succeed, NLM4Denied with
//     holder info if conflict exists
//   - error: System-level errors only
func (h *Handler) Test(ctx *NLMHandlerContext, req *TestRequest) (*TestResponse, error) {
	// Build owner ID for testing
	ownerID := buildOwnerID(req.Lock.CallerName, req.Lock.Svid, req.Lock.OH)

	logger.Debug("NLM TEST",
		"client", ctx.ClientAddr,
		"caller", req.Lock.CallerName,
		"owner", ownerID,
		"exclusive", req.Exclusive,
		"offset", req.Lock.Offset,
		"length", req.Lock.Length)

	// Convert file handle
	handle := metadata.FileHandle(req.Lock.FH)

	// Build lock owner
	owner := lock.LockOwner{
		OwnerID: ownerID,
	}

	// Call MetadataService to test lock
	granted, conflict, err := h.metadataService.TestLockNLM(
		ctx.Context,
		handle,
		owner,
		req.Lock.Offset,
		req.Lock.Length,
		req.Exclusive,
	)

	if err != nil {
		// System error - return as NLM4Failed
		logger.Warn("NLM TEST failed",
			"client", ctx.ClientAddr,
			"error", err)
		return &TestResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Failed,
		}, nil
	}

	if granted {
		return &TestResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Granted,
		}, nil
	}

	// Lock would conflict - return holder info
	holder := conflictToHolder(conflict)

	return &TestResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Denied,
		Holder: holder,
	}, nil
}

// buildOwnerID constructs the NLM owner ID string.
// Format: nlm:{caller_name}:{svid}:{oh_hex}
//
// This format enables cross-protocol lock conflict detection since the
// lock manager treats OwnerID as opaque and compares for equality.
func buildOwnerID(callerName string, svid int32, oh []byte) string {
	return fmt.Sprintf("nlm:%s:%d:%s", callerName, svid, hex.EncodeToString(oh))
}

// conflictToHolder converts an UnifiedLockConflict to NLM4Holder.
// Returns nil if conflict is nil.
func conflictToHolder(conflict *lock.UnifiedLockConflict) *types.NLM4Holder {
	if conflict == nil || conflict.Lock == nil {
		return nil
	}

	return &types.NLM4Holder{
		Exclusive: conflict.Lock.IsExclusive(),
		Svid:      0, // We don't track svid in UnifiedLock - would need to parse OwnerID
		OH:        nil,
		Offset:    conflict.Lock.Offset,
		Length:    conflict.Lock.Length,
	}
}
