package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	v41handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/v41/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
)

// v40OnlyOps contains operations that are valid only in NFSv4.0 COMPOUNDs.
// Per RFC 8881, these operations MUST return NFS4ERR_NOTSUPP when encountered
// in a v4.1 COMPOUND. Their XDR args must be consumed to prevent stream desync.
var v40OnlyOps = map[uint32]bool{
	types.OP_SETCLIENTID:         true, // op 35
	types.OP_SETCLIENTID_CONFIRM: true, // op 36
	types.OP_RENEW:               true, // op 30
	types.OP_OPEN_CONFIRM:        true, // op 20
	types.OP_RELEASE_LOCKOWNER:   true, // op 39
}

// consumeV40OnlyArgs reads and discards XDR args for v4.0-only operations
// to prevent stream desync when rejecting them in v4.1 COMPOUNDs.
// Returns nil on success or error if the reader doesn't have enough data.
func consumeV40OnlyArgs(opCode uint32, reader io.Reader) error {
	switch opCode {
	case types.OP_SETCLIENTID:
		// verifier (8 bytes) + client id string (opaque) + cb_program (uint32)
		// + netid (string) + addr (string) + callback_ident (uint32)
		var verifier [8]byte
		if _, err := io.ReadFull(reader, verifier[:]); err != nil {
			return err
		}
		if _, err := xdr.DecodeString(reader); err != nil { // client id string
			return err
		}
		if _, err := xdr.DecodeUint32(reader); err != nil { // cb_program
			return err
		}
		if _, err := xdr.DecodeString(reader); err != nil { // netid
			return err
		}
		if _, err := xdr.DecodeString(reader); err != nil { // addr
			return err
		}
		if _, err := xdr.DecodeUint32(reader); err != nil { // callback_ident
			return err
		}

	case types.OP_SETCLIENTID_CONFIRM:
		// clientid (uint64) + setclientid_confirm verifier (8 bytes)
		if _, err := xdr.DecodeUint64(reader); err != nil {
			return err
		}
		var verifier [8]byte
		if _, err := io.ReadFull(reader, verifier[:]); err != nil {
			return err
		}

	case types.OP_RENEW:
		// clientid (uint64)
		if _, err := xdr.DecodeUint64(reader); err != nil {
			return err
		}

	case types.OP_OPEN_CONFIRM:
		// stateid4 (seqid:uint32 + other:12 bytes) + seqid (uint32)
		if _, err := types.DecodeStateid4(reader); err != nil {
			return err
		}
		if _, err := xdr.DecodeUint32(reader); err != nil {
			return err
		}

	case types.OP_RELEASE_LOCKOWNER:
		// lock_owner4: clientid (uint64) + owner (opaque)
		if _, err := xdr.DecodeUint64(reader); err != nil {
			return err
		}
		if _, err := xdr.DecodeOpaque(reader); err != nil {
			return err
		}
	}
	return nil
}

// rejectV40OnlyOp consumes XDR args for a v4.0-only operation and returns
// a NOTSUPP result. If the args cannot be consumed, returns BADXDR.
// Used by both dispatchV41 and dispatchV41Ops to reject v4.0-only ops
// in v4.1 COMPOUNDs with proper stream advancement.
func rejectV40OnlyOp(opCode uint32, reader io.Reader, clientAddr string) *types.CompoundResult {
	if err := consumeV40OnlyArgs(opCode, reader); err != nil {
		logger.Debug("NFSv4.1 COMPOUND failed to consume v4.0-only op args",
			"opcode", opCode, "op_name", types.OpName(opCode), "error", err, "client", clientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: opCode,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	logger.Debug("NFSv4.1 COMPOUND rejected v4.0-only operation",
		"opcode", opCode, "op_name", types.OpName(opCode), "client", clientAddr)
	return notSuppHandler(opCode)
}

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
	logger.Debug("NFSv4 COMPOUND routing",
		"minor_version", minorVersion,
		"num_ops", numOps,
		"client", compCtx.ClientAddr)

	// Check configurable minor version range
	if minorVersion < h.minMinorVersion || minorVersion > h.maxMinorVersion {
		logger.Debug("NFSv4 minor version out of configured range",
			"requested", minorVersion,
			"min", h.minMinorVersion,
			"max", h.maxMinorVersion,
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
	}

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
		// Check context cancellation between operations.
		// Instead of returning an error (which would cause the RPC layer to
		// silently drop the reply, hanging the client), encode a partial
		// response with NFS4ERR_DELAY so the client can retry.
		if compCtx.Context.Err() != nil {
			logger.Debug("NFSv4 COMPOUND cancelled between ops",
				"op_index", i,
				"total_ops", numOps,
				"client", compCtx.ClientAddr)
			lastStatus = types.NFS4ERR_DELAY
			break
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

// dispatchV41 executes the v4.1 COMPOUND dispatch loop with SEQUENCE gating.
//
// Per RFC 8881, every non-exempt v4.1 COMPOUND must begin with SEQUENCE.
// SEQUENCE establishes slot-based exactly-once semantics. Exempt operations
// (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION) can
// appear as the first operation without a preceding SEQUENCE.
//
// The dispatch flow:
//  1. Validate op count
//  2. Read first opcode
//  3. If exempt op: dispatch all ops with v41ctx=nil (no session context)
//  4. If SEQUENCE: validate session/slot/seqid, then dispatch remaining ops
//  5. If neither: return NFS4ERR_OP_NOT_IN_SESSION
//
// On SEQUENCE replay (duplicate slot+seqid), returns the cached COMPOUND
// response bytes directly without re-executing any operations.
func (h *Handler) dispatchV41(compCtx *types.CompoundContext, tag []byte, numOps uint32, reader io.Reader) ([]byte, error) {
	// Validate operation count limit
	if numOps > types.MaxCompoundOps {
		logger.Debug("NFSv4.1 COMPOUND op count exceeds limit",
			"count", numOps,
			"max", types.MaxCompoundOps,
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_RESOURCE, tag, nil)
	}

	// Empty compound: just return success
	if numOps == 0 {
		return encodeCompoundResponse(types.NFS4_OK, tag, nil)
	}

	// Read the first operation opcode
	firstOpCode, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, fmt.Errorf("decode v4.1 first op opcode: %w", err)
	}

	// Check if the first operation is session-exempt
	if v41handlers.IsSessionExemptOp(firstOpCode) {
		logger.Debug("NFSv4.1 COMPOUND exempt op",
			"op_name", types.OpName(firstOpCode),
			"num_ops", numOps,
			"client", compCtx.ClientAddr)
		return h.dispatchV41Ops(compCtx, tag, firstOpCode, numOps, nil, reader)
	}

	// Non-exempt: first op MUST be SEQUENCE
	if firstOpCode != types.OP_SEQUENCE {
		logger.Debug("NFSv4.1 COMPOUND missing SEQUENCE",
			"first_op", types.OpName(firstOpCode),
			"client", compCtx.ClientAddr)
		return encodeCompoundResponse(types.NFS4ERR_OP_NOT_IN_SESSION, tag, nil)
	}

	// Process SEQUENCE
	seqResult, v41ctx, sess, cachedReply, seqErr := v41handlers.HandleSequenceOp(h.v41Deps, compCtx, reader)
	if seqErr != nil {
		return nil, fmt.Errorf("SEQUENCE processing error: %w", seqErr)
	}

	// Replay: return cached COMPOUND response bytes directly
	if cachedReply != nil {
		logger.Info("NFSv4.1 COMPOUND replay cache hit",
			"client", compCtx.ClientAddr)
		return cachedReply, nil
	}

	// SEQUENCE error (bad session, misordered, etc.): return single-op error
	if seqResult != nil && seqResult.Status != types.NFS4_OK {
		logger.Debug("NFSv4.1 COMPOUND SEQUENCE error",
			"status", seqResult.Status,
			"client", compCtx.ClientAddr)
		results := []types.CompoundResult{*seqResult}
		return encodeCompoundResponse(seqResult.Status, tag, results)
	}

	// SEQUENCE succeeded -- set v4.1 bypass for per-owner seqid
	compCtx.SkipOwnerSeqid = true

	// Ensure slot is released and response is cached via defer
	var responseBytes []byte
	defer func() {
		if sess != nil && v41ctx != nil {
			sess.ForeChannelSlots.CompleteSlotRequest(
				v41ctx.SlotID,
				v41ctx.SequenceID,
				v41ctx.CacheThis,
				responseBytes,
			)
		}
	}()

	// Check if the connection is draining (returns NFS4ERR_DELAY to redirect
	// client to another connection). SEQUENCE itself always works on draining
	// connections so it is checked after SEQUENCE validation succeeds.
	if compCtx.ConnectionID != 0 && h.StateManager.IsConnectionDraining(compCtx.ConnectionID) {
		logger.Debug("NFSv4.1 COMPOUND connection draining",
			"connection_id", compCtx.ConnectionID,
			"client", compCtx.ClientAddr)
		// Return SEQUENCE result plus a DELAY error for the compound
		results := []types.CompoundResult{*seqResult}
		encoded, encErr := encodeCompoundResponse(types.NFS4ERR_DELAY, tag, results)
		if encErr != nil {
			return nil, encErr
		}
		responseBytes = encoded
		return encoded, nil
	}

	// Build results starting with SEQUENCE result
	results := make([]types.CompoundResult, 0, int(numOps))
	results = append(results, *seqResult)
	var lastStatus uint32 = types.NFS4_OK

	// Dispatch remaining ops (numOps - 1)
	for i := uint32(1); i < numOps; i++ {
		// Check context cancellation between operations.
		// Instead of returning an error (which would skip slot caching and break
		// replay semantics), encode the partial response so the defer caches it.
		if compCtx.Context.Err() != nil {
			logger.Debug("NFSv4.1 COMPOUND cancelled between ops",
				"op_index", i,
				"total_ops", numOps,
				"client", compCtx.ClientAddr)
			lastStatus = types.NFS4ERR_DELAY
			break
		}

		// Read operation code. On decode error, encode partial response so the
		// defer caches it (preserving replay semantics for completed ops).
		opCode, err := xdr.DecodeUint32(reader)
		if err != nil {
			lastStatus = types.NFS4ERR_BADXDR
			break
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

		// Dispatch: reject v4.0-only, then v4.1 table, then v4.0 fallback.
		var result *types.CompoundResult

		if v40OnlyOps[opCode] {
			result = rejectV40OnlyOp(opCode, reader, compCtx.ClientAddr)
		} else if v41Handler, ok := h.v41DispatchTable[opCode]; ok {
			result = v41Handler(compCtx, v41ctx, reader)
		} else if v40Handler, ok := h.opDispatchTable[opCode]; ok {
			result = v40Handler(compCtx, reader)
		} else if opCode >= 3 && opCode <= types.OP_RECLAIM_COMPLETE {
			result = notSuppHandler(opCode)
		} else {
			result = &types.CompoundResult{
				Status: types.NFS4ERR_OP_ILLEGAL,
				OpCode: types.OP_ILLEGAL,
				Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
			}
		}

		if result.OpCode == 0 && opCode != 0 {
			result.OpCode = opCode
		}

		results = append(results, *result)
		lastStatus = result.Status

		logger.Debug("NFSv4.1 COMPOUND op dispatched",
			"op_index", i,
			"opcode", opCode,
			"op_name", types.OpName(opCode),
			"status", result.Status,
			"session_id", hex.EncodeToString(v41ctx.SessionID[:]),
			"slot", v41ctx.SlotID,
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

	encoded, encErr := encodeCompoundResponse(lastStatus, tag, results)
	if encErr != nil {
		return nil, encErr
	}

	// Store response for replay cache (captured by defer)
	responseBytes = encoded

	return encoded, nil
}

// dispatchV41Ops dispatches a v4.1 COMPOUND starting from firstOpCode.
// Used for both exempt ops (v41ctx=nil) and could be used after SEQUENCE.
// The firstOpCode has already been read from the reader.
func (h *Handler) dispatchV41Ops(compCtx *types.CompoundContext, tag []byte, firstOpCode uint32, numOps uint32, v41ctx *types.V41RequestContext, reader io.Reader) ([]byte, error) {
	results := make([]types.CompoundResult, 0, int(numOps))
	var lastStatus uint32 = types.NFS4_OK

	// Process all ops, starting with firstOpCode already read
	for i := uint32(0); i < numOps; i++ {
		var opCode uint32
		if i == 0 {
			opCode = firstOpCode
		} else {
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

			var err error
			opCode, err = xdr.DecodeUint32(reader)
			if err != nil {
				return nil, fmt.Errorf("decode v4.1 op %d opcode: %w", i, err)
			}
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

		// Dispatch: reject v4.0-only, then v4.1 table, then v4.0 fallback.
		var result *types.CompoundResult

		if v40OnlyOps[opCode] {
			result = rejectV40OnlyOp(opCode, reader, compCtx.ClientAddr)
		} else if v41Handler, ok := h.v41DispatchTable[opCode]; ok {
			result = v41Handler(compCtx, v41ctx, reader)
		} else if v40Handler, ok := h.opDispatchTable[opCode]; ok {
			result = v40Handler(compCtx, reader)
		} else if opCode >= 3 && opCode <= types.OP_RECLAIM_COMPLETE {
			result = notSuppHandler(opCode)
		} else {
			result = &types.CompoundResult{
				Status: types.NFS4ERR_OP_ILLEGAL,
				OpCode: types.OP_ILLEGAL,
				Data:   encodeStatusOnly(types.NFS4ERR_OP_ILLEGAL),
			}
		}

		if result.OpCode == 0 && opCode != 0 {
			result.OpCode = opCode
		}

		results = append(results, *result)
		lastStatus = result.Status

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
