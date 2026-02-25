package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// ShareRequest represents an NLM_SHARE request.
type ShareRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// CallerName is the client hostname.
	CallerName string

	// FH is the NFS file handle.
	FH []byte

	// OH is the owner handle.
	OH []byte

	// Mode is the share access mode (read, write, read-write).
	Mode uint32

	// Access is the share deny mode (deny none, deny read, deny write, deny both).
	Access uint32

	// Reclaim indicates whether this is a reclaim during grace period.
	Reclaim bool
}

// ShareResponse represents an NLM_SHARE response.
type ShareResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	Status uint32

	// Sequence is a monotonically increasing counter for state tracking.
	Sequence int32
}

// DecodeShareRequest decodes an NLM_SHARE request from XDR format.
func DecodeShareRequest(data []byte) (*ShareRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4ShareArgs(r)
	if err != nil {
		return nil, fmt.Errorf("decode NLM4ShareArgs: %w", err)
	}

	return &ShareRequest{
		Cookie:     args.Cookie,
		CallerName: args.CallerName,
		FH:         args.FH,
		OH:         args.OH,
		Mode:       args.Mode,
		Access:     args.Access,
		Reclaim:    args.Reclaim,
	}, nil
}

// EncodeShareResponse encodes an NLM_SHARE response to XDR format.
func EncodeShareResponse(resp *ShareResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	res := &types.NLM4ShareRes{
		Cookie:   resp.Cookie,
		Status:   resp.Status,
		Sequence: resp.Sequence,
	}

	if err := nlm_xdr.EncodeNLM4ShareRes(buf, res); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Share handles NLM SHARE (RFC 1813, NLM procedure 20).
// Acquires a DOS-style share mode lock; used by Windows NFS clients for file sharing.
// No delegation; always grants immediately (share-mode conflict detection not implemented).
// No side effects; shares are not tracked (safe due to advisory-only NFS locking model).
// Errors: always NLM4Granted (share requests are unconditionally granted).
func (h *Handler) Share(ctx *NLMHandlerContext, req *ShareRequest) (*ShareResponse, error) {
	logger.Debug("NLM SHARE",
		"client", ctx.ClientAddr,
		"caller", req.CallerName,
		"mode", req.Mode,
		"access", req.Access,
		"reclaim", req.Reclaim)

	return &ShareResponse{
		Cookie:   req.Cookie,
		Status:   types.NLM4Granted,
		Sequence: 0,
	}, nil
}

// Unshare handles NLM UNSHARE (RFC 1813, NLM procedure 21).
// Releases a previously acquired DOS-style share mode lock.
// No delegation; always succeeds immediately (shares are not tracked).
// No side effects; stateless operation (safe due to advisory-only NFS locking model).
// Errors: always NLM4Granted (unshare requests are unconditionally granted).
func (h *Handler) Unshare(ctx *NLMHandlerContext, req *ShareRequest) (*ShareResponse, error) {
	logger.Debug("NLM UNSHARE",
		"client", ctx.ClientAddr,
		"caller", req.CallerName)

	return &ShareResponse{
		Cookie:   req.Cookie,
		Status:   types.NLM4Granted,
		Sequence: 0,
	}, nil
}
