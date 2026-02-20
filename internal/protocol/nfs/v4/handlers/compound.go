package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ProcessCompound is the main NFSv4 COMPOUND dispatcher.
//
// Per RFC 7530 Section 16.2, COMPOUND bundles multiple operations into a single
// RPC call. Operations execute sequentially with shared filehandle state.
// Execution stops on the first error, and the response includes results up to
// and including the failed operation.
//
// Minorversion routing (per RFC 8881):
//   - 0: NFSv4.0 dispatch table (OpHandler signature)
//   - 1: NFSv4.1 dispatch table (V41OpHandler), with fallback to v4.0 table
//   - 2+: NFS4ERR_MINOR_VERS_MISMATCH
//
// COMPOUND4args wire format:
//
//	tag:           opaque<> (variable-length bytes, echoed back)
//	minorversion:  uint32   (0 for NFSv4.0, 1 for NFSv4.1)
//	numops:        uint32   (count of operations)
//	ops[]:         each op is opcode (uint32) followed by op-specific args
//
// COMPOUND4res wire format:
//
//	status:        nfsstat4 (status of last evaluated op, or NFS4_OK if all succeed)
//	tag:           opaque<> (echoed from request)
//	numresults:    uint32   (count of results)
//	results[]:     each result is opcode (uint32) followed by op-specific result
func (h *Handler) ProcessCompound(compCtx *types.CompoundContext, data []byte) ([]byte, error) {
	reader := bytes.NewReader(data)

	// Decode tag (XDR opaque: length + bytes + padding)
	tag, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode COMPOUND tag: %w", err)
	}

	// Decode minor version
	minorVersion, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode COMPOUND minorversion: %w", err)
	}

	// Decode number of operations
	numOps, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode COMPOUND numops: %w", err)
	}

	// Route based on minor version
	switch minorVersion {
	case types.NFS4_MINOR_VERSION_0:
		return h.dispatchV40(compCtx, tag, numOps, reader)

	case types.NFS4_MINOR_VERSION_1:
		return h.dispatchV41(compCtx, tag, numOps, reader)

	default:
		logger.Debug("NFSv4 minor version mismatch",
			"requested", minorVersion,
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
	}
}

// dispatchV40 executes the v4.0 COMPOUND dispatch loop.
// Operations are dispatched through opDispatchTable.
func (h *Handler) dispatchV40(compCtx *types.CompoundContext, tag []byte, numOps uint32, reader io.Reader) ([]byte, error) {
	// Validate operation count limit
	if numOps > types.MaxCompoundOps {
		logger.Debug("NFSv4 COMPOUND op count exceeds limit",
			"count", numOps,
			"max", types.MaxCompoundOps,
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_RESOURCE, tag, nil)
	}

	// Sequential dispatch loop
	results := make([]types.CompoundResult, 0, int(numOps))
	var lastStatus uint32 = types.NFS4_OK

	for i := uint32(0); i < numOps; i++ {
		// Check context cancellation between operations
		select {
		case <-compCtx.Context.Done():
			logger.Debug("NFSv4 COMPOUND cancelled between ops",
				"op_index", i,
				"total_ops", numOps,
				"client", compCtx.ClientAddr)
			return nil, compCtx.Context.Err()
		default:
		}

		// Read operation code
		opCode, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode op %d opcode: %w", i, err)
		}

		// Check adapter-level operation blocklist before dispatch.
		// Blocked operations return NFS4ERR_NOTSUPP per locked decision.
		if h.IsOperationBlocked(opCode) {
			logger.Debug("NFSv4 COMPOUND op blocked by adapter settings",
				"op_index", i,
				"opcode", opCode,
				"op_name", types.OpName(opCode),
				"client", compCtx.ClientAddr)

			result := notSuppHandler(opCode)
			result.OpCode = opCode
			results = append(results, *result)
			lastStatus = result.Status
			break
		}

		// Dispatch to v4.0 handler
		var result *types.CompoundResult
		handler, ok := h.opDispatchTable[opCode]
		if ok {
			result = handler(compCtx, reader)
		} else {
			// Unknown opcode: check if it's in the valid v4.0 range (3-39)
			// or truly illegal/unknown
			if opCode < 3 || opCode > types.OP_RELEASE_LOCKOWNER {
				// Truly illegal operation number
				result = &types.CompoundResult{
					Status: types.NFS4ERR_OP_ILLEGAL,
					OpCode: types.OP_ILLEGAL,
					Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
				}
			} else {
				// Valid v4.0 operation number but not yet implemented
				result = notSuppHandler(opCode)
			}
		}

		// Set the opcode on the result (in case handler didn't set it)
		if result.OpCode == 0 && opCode != 0 {
			result.OpCode = opCode
		}

		results = append(results, *result)
		lastStatus = result.Status

		// Log every operation for debugging
		logger.Debug("NFSv4 COMPOUND op dispatched",
			"op_index", i,
			"opcode", opCode,
			"op_name", types.OpName(opCode),
			"status", result.Status,
			"client", compCtx.ClientAddr)

		if result.Status != types.NFS4_OK {
			logger.Debug("NFSv4 COMPOUND op failed, stopping",
				"op_index", i,
				"opcode", opCode,
				"op_name", types.OpName(opCode),
				"status", result.Status,
				"client", compCtx.ClientAddr)
			break
		}
	}

	return encodeCompoundResponse(lastStatus, tag, results)
}

// dispatchV41 executes the v4.1 COMPOUND dispatch loop.
//
// Per RFC 8881, v4.1 COMPOUNDs can include both v4.0 operations (3-39) and
// v4.1-only operations (40-58). v4.0 operations are dispatched through the
// v4.0 opDispatchTable via fallback. v4.1 operations use the v41DispatchTable.
//
// Phase 20 will populate V41RequestContext from SEQUENCE processing.
// Until then, v41ctx is nil for all operations.
func (h *Handler) dispatchV41(compCtx *types.CompoundContext, tag []byte, numOps uint32, reader io.Reader) ([]byte, error) {
	// Validate operation count limit
	if numOps > types.MaxCompoundOps {
		logger.Debug("NFSv4.1 COMPOUND op count exceeds limit",
			"count", numOps,
			"max", types.MaxCompoundOps,
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_RESOURCE, tag, nil)
	}

	// v4.1 request context (nil until SEQUENCE processing in Phase 20)
	var v41ctx *types.V41RequestContext

	// Sequential dispatch loop
	results := make([]types.CompoundResult, 0, int(numOps))
	var lastStatus uint32 = types.NFS4_OK

	for i := uint32(0); i < numOps; i++ {
		// Check context cancellation between operations
		select {
		case <-compCtx.Context.Done():
			logger.Debug("NFSv4.1 COMPOUND cancelled between ops",
				"op_index", i,
				"total_ops", numOps,
				"client", compCtx.ClientAddr)
			return nil, compCtx.Context.Err()
		default:
		}

		// Read operation code
		opCode, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, fmt.Errorf("decode v4.1 op %d opcode: %w", i, err)
		}

		// Check adapter-level operation blocklist before dispatch.
		if h.IsOperationBlocked(opCode) {
			logger.Debug("NFSv4.1 COMPOUND op blocked by adapter settings",
				"op_index", i,
				"opcode", opCode,
				"op_name", types.OpName(opCode),
				"client", compCtx.ClientAddr)

			result := notSuppHandler(opCode)
			result.OpCode = opCode
			results = append(results, *result)
			lastStatus = result.Status
			break
		}

		// Dispatch: v4.1 table first, then fallback to v4.0 table.
		var result *types.CompoundResult

		if v41Handler, ok := h.v41DispatchTable[opCode]; ok {
			// v4.1-only operation (40-58)
			result = v41Handler(compCtx, v41ctx, reader)
		} else if v40Handler, ok := h.opDispatchTable[opCode]; ok {
			// v4.0 operation accessible from v4.1 compound (3-39)
			result = v40Handler(compCtx, reader)
		} else {
			// Unknown opcode: determine error based on range
			if opCode >= 3 && opCode <= types.OP_RECLAIM_COMPLETE {
				// Within valid range (3-58) but not in either table
				result = notSuppHandler(opCode)
			} else {
				// Outside all valid ranges: truly illegal
				result = &types.CompoundResult{
					Status: types.NFS4ERR_OP_ILLEGAL,
					OpCode: types.OP_ILLEGAL,
					Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
				}
			}
		}

		// Set the opcode on the result (in case handler didn't set it)
		if result.OpCode == 0 && opCode != 0 {
			result.OpCode = opCode
		}

		results = append(results, *result)
		lastStatus = result.Status

		// Log every operation for debugging
		logger.Debug("NFSv4.1 COMPOUND op dispatched",
			"op_index", i,
			"opcode", opCode,
			"op_name", types.OpName(opCode),
			"status", result.Status,
			"client", compCtx.ClientAddr)

		if result.Status != types.NFS4_OK {
			logger.Debug("NFSv4.1 COMPOUND op failed, stopping",
				"op_index", i,
				"opcode", opCode,
				"op_name", types.OpName(opCode),
				"status", result.Status,
				"client", compCtx.ClientAddr)
			break
		}
	}

	return encodeCompoundResponse(lastStatus, tag, results)
}

// encodeCompoundResponse encodes a COMPOUND4res response.
//
// Wire format:
//
//	status:     uint32   (nfsstat4)
//	tag:        opaque<> (echoed from request)
//	numresults: uint32
//	results[]:  opcode (uint32) + result data
func encodeCompoundResponse(status uint32, tag []byte, results []types.CompoundResult) ([]byte, error) {
	var buf bytes.Buffer

	// Write overall status
	if err := xdr.WriteUint32(&buf, status); err != nil {
		return nil, fmt.Errorf("encode COMPOUND status: %w", err)
	}

	// Write tag (echoed from request as opaque)
	if err := xdr.WriteXDROpaque(&buf, tag); err != nil {
		return nil, fmt.Errorf("encode COMPOUND tag: %w", err)
	}

	// Write number of results
	numResults := uint32(len(results))
	if err := xdr.WriteUint32(&buf, numResults); err != nil {
		return nil, fmt.Errorf("encode COMPOUND numresults: %w", err)
	}

	// Write each result
	for i, result := range results {
		// Write operation code
		if err := xdr.WriteUint32(&buf, result.OpCode); err != nil {
			return nil, fmt.Errorf("encode result %d opcode: %w", i, err)
		}

		// Write result data (status + op-specific data)
		if _, err := buf.Write(result.Data); err != nil {
			return nil, fmt.Errorf("encode result %d data: %w", i, err)
		}
	}

	return buf.Bytes(), nil
}
