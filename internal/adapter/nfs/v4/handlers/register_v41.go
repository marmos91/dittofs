// register_v41.go — NFSv4.1 (RFC 8881) operation handler registration.
package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	v41handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/v41/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// registerV41Ops registers the 19 NFSv4.1 operations (op numbers 40-58) into
// v41DispatchTable. Most are stubs that decode their args and return NOTSUPP.
func (h *Handler) registerV41Ops() {
	// BACKCHANNEL_CTL: update callback program and security params (RFC 8881 Section 18.33)
	h.v41DispatchTable[types.OP_BACKCHANNEL_CTL] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleBackchannelCtl(h.v41Deps, ctx, v41ctx, reader)
	}
	// BIND_CONN_TO_SESSION: connection binding (RFC 8881 Section 18.34)
	h.v41DispatchTable[types.OP_BIND_CONN_TO_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleBindConnToSession(h.v41Deps, ctx, v41ctx, reader)
	}
	// EXCHANGE_ID: client identity registration (RFC 8881 Section 18.35)
	h.v41DispatchTable[types.OP_EXCHANGE_ID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleExchangeID(h.v41Deps, ctx, v41ctx, reader)
	}
	// CREATE_SESSION: session lifecycle (RFC 8881 Section 18.36)
	h.v41DispatchTable[types.OP_CREATE_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleCreateSession(h.v41Deps, ctx, v41ctx, reader)
	}
	// DESTROY_SESSION: session teardown (RFC 8881 Section 18.37)
	h.v41DispatchTable[types.OP_DESTROY_SESSION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleDestroySession(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_FREE_STATEID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleFreeStateid(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_GET_DIR_DELEGATION] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleGetDirDelegation(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_GETDEVICEINFO] = v41StubHandler(types.OP_GETDEVICEINFO, func(r io.Reader) error {
		var args types.GetDeviceInfoArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_GETDEVICELIST] = v41StubHandler(types.OP_GETDEVICELIST, func(r io.Reader) error {
		var args types.GetDeviceListArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTCOMMIT] = v41StubHandler(types.OP_LAYOUTCOMMIT, func(r io.Reader) error {
		var args types.LayoutCommitArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTGET] = v41StubHandler(types.OP_LAYOUTGET, func(r io.Reader) error {
		var args types.LayoutGetArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_LAYOUTRETURN] = v41StubHandler(types.OP_LAYOUTRETURN, func(r io.Reader) error {
		var args types.LayoutReturnArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_SECINFO_NO_NAME] = v41StubHandler(types.OP_SECINFO_NO_NAME, func(r io.Reader) error {
		var args types.SecinfoNoNameArgs
		return args.Decode(r)
	})
	// SEQUENCE at position > 0 returns NFS4ERR_SEQUENCE_POS per RFC 8881.
	// SEQUENCE at position 0 is handled specially in dispatchV41 before the op loop.
	h.v41DispatchTable[types.OP_SEQUENCE] = func(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		var args types.SequenceArgs
		if err := args.Decode(reader); err != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_SEQUENCE,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}
		return &types.CompoundResult{
			Status: types.NFS4ERR_SEQUENCE_POS,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(types.NFS4ERR_SEQUENCE_POS),
		}
	}
	h.v41DispatchTable[types.OP_SET_SSV] = v41StubHandler(types.OP_SET_SSV, func(r io.Reader) error {
		var args types.SetSsvArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_TEST_STATEID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleTestStateid(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_WANT_DELEGATION] = v41StubHandler(types.OP_WANT_DELEGATION, func(r io.Reader) error {
		var args types.WantDelegationArgs
		return args.Decode(r)
	})
	h.v41DispatchTable[types.OP_DESTROY_CLIENTID] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleDestroyClientID(h.v41Deps, ctx, v41ctx, reader)
	}
	h.v41DispatchTable[types.OP_RECLAIM_COMPLETE] = func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		return v41handlers.HandleReclaimComplete(h.v41Deps, ctx, v41ctx, reader)
	}
}

// v41StubHandler creates a v4.1 stub handler that decodes args and returns NOTSUPP.
// The decoder parameter consumes the operation's XDR args from the reader to
// prevent stream desync (the CRITICAL invariant for COMPOUND processing).
func v41StubHandler(opCode uint32, decoder func(io.Reader) error) V41OpHandler {
	return func(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
		if err := decoder(reader); err != nil {
			logger.Debug("NFSv4.1 stub decode error",
				"op", types.OpName(opCode), "error", err, "client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: opCode,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}
		logger.Debug("NFSv4.1 operation not yet implemented",
			"op", types.OpName(opCode), "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_NOTSUPP,
			OpCode: opCode,
			Data:   encodeStatusOnly(types.NFS4ERR_NOTSUPP),
		}
	}
}
