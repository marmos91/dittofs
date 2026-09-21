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
