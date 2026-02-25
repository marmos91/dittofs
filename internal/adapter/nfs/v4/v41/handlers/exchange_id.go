package v41handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// HandleExchangeID implements the EXCHANGE_ID operation (RFC 8881 Section 18.35).
// Registers a v4.1 client with the server and returns a client ID for session creation.
// Delegates to StateManager.ExchangeID for multi-case algorithm logic; rejects SP4_MACH_CRED/SP4_SSV.
// Creates or updates client record; returns clientid, sequence ID, and server capabilities.
// Errors: NFS4ERR_CLID_INUSE, NFS4ERR_ENCR_ALG_UNSUPP (non-SP4_NONE), NFS4ERR_BADXDR.
func HandleExchangeID(d *Deps, ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.ExchangeIdArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("EXCHANGE_ID: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_EXCHANGE_ID,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Reject SP4_MACH_CRED and SP4_SSV before any state allocation
	if args.StateProtect.How != types.SP4_NONE {
		logger.Debug("EXCHANGE_ID: rejecting non-SP4_NONE state protection",
			"how", args.StateProtect.How, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_ENCR_ALG_UNSUPP,
			OpCode: types.OP_EXCHANGE_ID,
			Data:   EncodeStatusOnly(types.NFS4ERR_ENCR_ALG_UNSUPP),
		}
	}

	if len(args.ClientImplId) > 0 {
		impl := args.ClientImplId[0]
		logger.Info("EXCHANGE_ID: client implementation",
			"impl_name", impl.Name,
			"impl_domain", impl.Domain,
			"client", ctx.ClientAddr)
	}

	// Delegate to StateManager for the multi-case algorithm
	result, err := d.StateManager.ExchangeID(
		args.ClientOwner.OwnerID,
		args.ClientOwner.Verifier,
		args.Flags,
		args.ClientImplId,
		ctx.ClientAddr,
	)
	if err != nil {
		nfsStatus := MapStateError(err)
		logger.Debug("EXCHANGE_ID: state error",
			"error", err, "nfs_status", nfsStatus, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_EXCHANGE_ID,
			Data:   EncodeStatusOnly(nfsStatus),
		}
	}

	res := &types.ExchangeIdRes{
		Status:     types.NFS4_OK,
		ClientID:   result.ClientID,
		SequenceID: result.SequenceID,
		Flags:      result.Flags,
		StateProtect: types.StateProtect4R{
			How: types.SP4_NONE,
		},
		ServerOwner:  result.ServerOwner,
		ServerScope:  result.ServerScope,
		ServerImplId: result.ServerImplId,
	}

	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("EXCHANGE_ID: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_EXCHANGE_ID,
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Debug("EXCHANGE_ID: success",
		"client_id", result.ClientID,
		"sequence_id", result.SequenceID,
		"flags", result.Flags,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_EXCHANGE_ID,
		Data:   buf.Bytes(),
	}
}
