package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleSequenceOp implements the SEQUENCE operation (RFC 8881 Section 18.46).
//
// SEQUENCE must be the first operation in every non-exempt NFSv4.1 COMPOUND.
// It establishes slot-based exactly-once semantics by:
//  1. Looking up the session by ID
//  2. Validating the slot sequence ID (new request, replay, or misordered)
//  3. On replay: returning the cached COMPOUND response bytes directly
//  4. On new: building a V41RequestContext, renewing the client lease, computing status flags
//
// Returns:
//   - sequenceResult: the CompoundResult for the SEQUENCE op itself
//   - v41ctx: the V41RequestContext for subsequent ops (nil on error/replay)
//   - session: the Session object (nil on error/replay)
//   - cachedReply: non-nil on replay (full COMPOUND response bytes to return directly)
//   - err: non-nil on decode failure
func (h *Handler) handleSequenceOp(compCtx *types.CompoundContext, reader io.Reader) (
	sequenceResult *types.CompoundResult,
	v41ctx *types.V41RequestContext,
	session *state.Session,
	cachedReply []byte,
	err error,
) {
	// Record every SEQUENCE invocation
	h.sequenceMetrics.RecordSequence()

	// Decode SEQUENCE args
	var args types.SequenceArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("SEQUENCE: decode error", "error", err, "client", compCtx.ClientAddr)
		h.sequenceMetrics.RecordError("bad_xdr")
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}, nil, nil, nil, nil
	}

	// Look up session
	sess := h.StateManager.GetSession(args.SessionID)
	if sess == nil {
		logger.Debug("SEQUENCE: session not found",
			"session_id", hex.EncodeToString(args.SessionID[:]),
			"client", compCtx.ClientAddr)
		h.sequenceMetrics.RecordError("bad_session")
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADSESSION,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADSESSION),
		}, nil, nil, nil, nil
	}

	// Validate slot sequence ID
	validation, slot, validationErr := sess.ForeChannelSlots.ValidateSequence(args.SlotID, args.SequenceID)

	switch validation {
	case state.SeqRetry:
		// Replay: return cached COMPOUND response bytes directly
		logger.Info("SEQUENCE: replay cache hit",
			"session_id", args.SessionID.String(),
			"slot", args.SlotID,
			"seqid", args.SequenceID,
			"client", compCtx.ClientAddr)
		h.sequenceMetrics.RecordError("replay_hit")
		h.sequenceMetrics.RecordReplayHit()
		if slot != nil && slot.CachedReply != nil {
			return nil, nil, nil, slot.CachedReply, nil
		}
		// Invariant violation: SeqRetry should only be returned when CachedReply != nil.
		// ValidateSequence returns SeqMisordered with NFS4ERR_RETRY_UNCACHED_REP for
		// retries without cache. If we ever reach this point, treat as internal error.
		h.sequenceMetrics.RecordError("retry_uncached")
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}, nil, nil, nil, nil

	case state.SeqMisordered:
		// Misordered, bad slot, delay, or uncached retry -- extract status from error
		nfsStatus := uint32(types.NFS4ERR_SEQ_MISORDERED)
		if stateErr, ok := validationErr.(*state.NFS4StateError); ok {
			nfsStatus = stateErr.Status
		}
		// Classify error type for metrics
		switch nfsStatus {
		case types.NFS4ERR_SEQ_MISORDERED:
			h.sequenceMetrics.RecordError("seq_misordered")
		case types.NFS4ERR_BADSLOT:
			h.sequenceMetrics.RecordError("bad_slot")
		case types.NFS4ERR_DELAY:
			h.sequenceMetrics.RecordError("slot_busy")
		case types.NFS4ERR_RETRY_UNCACHED_REP:
			h.sequenceMetrics.RecordError("retry_uncached")
		default:
			h.sequenceMetrics.RecordError("seq_misordered")
		}
		logger.Debug("SEQUENCE: validation failed",
			"session_id", args.SessionID.String(),
			"slot", args.SlotID,
			"seqid", args.SequenceID,
			"status", nfsStatus,
			"error", validationErr,
			"client", compCtx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_SEQUENCE,
			Data:   encodeStatusOnly(nfsStatus),
		}, nil, nil, nil, nil

	case state.SeqNew:
		// New request: build context, renew lease, compute status flags
		_ = slot // slot is reserved (InUse=true), will be released by caller via defer

		// Update slot utilization metrics
		sessionIDHex := args.SessionID.String()
		h.sequenceMetrics.SetSlotsInUse(sessionIDHex, float64(sess.ForeChannelSlots.SlotsInUse()))

		// Renew lease (implicit per RFC 8881 Section 8.1.3)
		h.StateManager.RenewV41Lease(sess.ClientID)

		// Compute status flags
		statusFlags := h.StateManager.GetStatusFlags(sess)

		// Build SEQUENCE response
		res := &types.SequenceRes{
			Status:              types.NFS4_OK,
			SessionID:           args.SessionID,
			SequenceID:          args.SequenceID,
			SlotID:              args.SlotID,
			HighestSlotID:       sess.ForeChannelSlots.GetHighestSlotID(),
			TargetHighestSlotID: sess.ForeChannelSlots.GetTargetHighestSlotID(),
			StatusFlags:         statusFlags,
		}

		var buf bytes.Buffer
		if encErr := res.Encode(&buf); encErr != nil {
			logger.Error("SEQUENCE: encode response error", "error", encErr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_SERVERFAULT,
				OpCode: types.OP_SEQUENCE,
				Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
			}, nil, nil, nil, nil
		}

		// Build V41RequestContext
		ctx := &types.V41RequestContext{
			SessionID:   args.SessionID,
			SlotID:      args.SlotID,
			SequenceID:  args.SequenceID,
			HighestSlot: args.HighestSlotID,
			CacheThis:   args.CacheThis,
		}

		logger.Debug("SEQUENCE: validated successfully",
			"session_id", args.SessionID.String(),
			"slot", args.SlotID,
			"seqid", args.SequenceID,
			"status_flags", fmt.Sprintf("0x%08x", statusFlags),
			"client", compCtx.ClientAddr)

		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_SEQUENCE,
			Data:   buf.Bytes(),
		}, ctx, sess, nil, nil
	}

	// Should not reach here
	return &types.CompoundResult{
		Status: types.NFS4ERR_SERVERFAULT,
		OpCode: types.OP_SEQUENCE,
		Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
	}, nil, nil, nil, nil
}

// isSessionExemptOp returns true if the given operation code is exempt from
// the SEQUENCE-first requirement per RFC 8881.
//
// These operations can appear as the first (and often only) operation in a
// v4.1 COMPOUND without a preceding SEQUENCE:
//   - EXCHANGE_ID: client registration (must work before any session exists)
//   - CREATE_SESSION: session creation (must work before any session exists)
//   - DESTROY_SESSION: session teardown (can target a different session)
//   - BIND_CONN_TO_SESSION: connection binding (must work on new connections)
//   - DESTROY_CLIENTID: client teardown (RFC 8881 Section 18.50.3 -- MAY be
//     the only operation, allowing it after the client's last session is destroyed)
func isSessionExemptOp(opCode uint32) bool {
	switch opCode {
	case types.OP_EXCHANGE_ID,
		types.OP_CREATE_SESSION,
		types.OP_DESTROY_SESSION,
		types.OP_BIND_CONN_TO_SESSION,
		types.OP_DESTROY_CLIENTID:
		return true
	default:
		return false
	}
}
