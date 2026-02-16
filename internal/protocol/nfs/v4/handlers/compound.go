package handlers

import (
	"bytes"
	"fmt"

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
// COMPOUND4args wire format:
//
//	tag:           opaque<> (variable-length bytes, echoed back)
//	minorversion:  uint32   (must be 0 for NFSv4.0)
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

	// Validate minor version: only 0 is supported
	if minorVersion != types.NFS4_MINOR_VERSION_0 {
		logger.Debug("NFSv4 minor version mismatch",
			"requested", minorVersion,
			"supported", types.NFS4_MINOR_VERSION_0,
			"client", compCtx.ClientAddr)

		return encodeCompoundResponse(types.NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
	}

	// Decode number of operations
	numOps, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode COMPOUND numops: %w", err)
	}

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

		// Dispatch to handler
		var result *types.CompoundResult
		handler, ok := h.opDispatchTable[opCode]
		if ok {
			result = handler(compCtx, reader)
		} else {
			// Unknown opcode: check if it's in the valid range (3-39)
			// or truly illegal/unknown
			if opCode < 3 || opCode > types.OP_RELEASE_LOCKOWNER {
				// Truly illegal operation number
				result = &types.CompoundResult{
					Status: types.NFS4ERR_OP_ILLEGAL,
					OpCode: types.OP_ILLEGAL,
					Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
				}
			} else {
				// Valid operation number but not yet implemented
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
