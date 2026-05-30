package handlers

// SMB3 channel-sequence verification (MS-SMB2 §3.3.5.2.10).
//
// When a client's channel to the server fails, it reconnects (binding a new
// channel to the same session) and resends operations that may have been
// in-flight, marking the resend SMB2_FLAGS_REPLAY_OPERATION. To keep the
// resend from being applied a second time out of order, every request carries
// a 16-bit ChannelSequence number that the client increments on each channel
// failover. The server tracks the latest ChannelSequence it has seen per Open
// (Open.ChannelSequence) and uses it to decide whether a request is fresh, a
// legitimate failover, or a stale resend on a channel the client has already
// moved past.
//
// Only the three modifying operations — WRITE, SET_INFO, IOCTL — are rejected
// on a stale ChannelSequence (with STATUS_FILE_NOT_AVAILABLE). Read-only
// operations are always allowed; they still advance Open.ChannelSequence when
// they carry a newer one so a subsequent modifying op sees the up-to-date
// value. This mirrors Samba's smbd_smb2_request_dispatch_update_counts in
// source3/smbd/smb2_server.c.
//
// Samba additionally maintains per-Open request_count / pre_request_count
// gauges to defer replay acceptance while requests on a prior ChannelSequence
// are still in flight. DittoFS processes each request to completion before the
// next on a handle is dispatched (no cross-channel reordering of a single
// handle's I/O), so those gauges are always drained by the time the following
// request is verified; the decision then reduces to the ChannelSequence
// comparison alone, which is what the channel-sequence and replay4 torture
// tests observe.

// VerifyChannelSequence applies the MS-SMB2 §3.3.5.2.10 channel-sequence check
// to a request targeting this Open and advances the tracked sequence.
//
//   - reqCSN:   the ChannelSequence from the request header (low 16 bits of the
//     SMB2 ChannelSequence/Reserved field).
//   - isModify: whether the command is a modifying op (WRITE, SET_INFO, IOCTL).
//
// It returns false when the request must be rejected with
// STATUS_FILE_NOT_AVAILABLE; true when it may proceed.
//
// The SMB2_FLAGS_REPLAY_OPERATION flag does not change the decision: a replay
// of a stale modifying op is exactly the out-of-order resend the check exists
// to suppress, and a replay on the current/newer sequence is accepted on the
// same terms as a fresh request (see the package note on request_count).
func (f *OpenFile) VerifyChannelSequence(reqCSN uint16, isModify bool) bool {
	f.csMu.Lock()
	defer f.csMu.Unlock()

	// Seed the tracked sequence from the first request seen on this Open so an
	// initial nonzero ChannelSequence is not misread as a failover.
	if !f.channelSeqSet {
		f.channelSeq = reqCSN
		f.channelSeqSet = true
		return true
	}

	// Full-width difference between the request's ChannelSequence and the one
	// tracked for this Open, matching Samba: compute as plain integers (range
	// -65535..65535), then treat a magnitude greater than 0x7FFF as a 16-bit
	// wraparound of the client counter (an actual forward step) by flipping the
	// sign. cmp == 0 → same channel; cmp > 0 → newer channel (failover);
	// cmp < 0 → older/stale channel.
	cmp := int32(reqCSN) - int32(f.channelSeq)
	if cmp > 0x7FFF || cmp < -0x7FFF {
		cmp = -cmp
	}

	switch {
	case cmp == 0:
		return true
	case cmp > 0:
		f.channelSeq = reqCSN
		return true
	case isModify:
		// Stale ChannelSequence on a modifying op: reject.
		return false
	default:
		return true
	}
}
