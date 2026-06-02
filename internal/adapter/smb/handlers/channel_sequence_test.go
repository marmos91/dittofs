package handlers

import "testing"

// TestVerifyChannelSequence_SambaTable replays the exact ChannelSequence
// table from Samba's source4/torture/smb2/replay.c test_channel_sequence_table
// for a modifying opcode (WRITE/SET_INFO/IOCTL) and asserts the accept/reject
// decision matches the upstream expected NT_STATUS at every step. This is the
// authoritative spec for smb2.replay.channel-sequence.
func TestVerifyChannelSequence_SambaTable(t *testing.T) {
	// allow == true means the op is expected to succeed (NT_STATUS_OK);
	// allow == false means it must be rejected (NT_STATUS_FILE_NOT_AVAILABLE).
	steps := []struct {
		csn   uint16
		allow bool
	}{
		{csn: 0, allow: true},           // i0: seeds stored=0
		{csn: 0x7fff + 1, allow: false}, // i1
		{csn: 0x7fff + 2, allow: false}, // i2
		{csn: 0x8000, allow: false},     // i3: csn_rand_high substitute (stale)
		{csn: 0xffff, allow: false},     // i4
		{csn: 0x7fff, allow: true},      // i5: advances stored=0x7fff
		{csn: 0x7ffe, allow: false},     // i6
		{csn: 0, allow: false},          // i7
		{csn: 0, allow: false},          // i8: csn_rand_low substitute (stale)
		{csn: 0x7fff + 1, allow: true},  // i9: advances stored=0x8000
		{csn: 0xffff, allow: true},      // i10: advances stored=0xffff
		{csn: 0, allow: true},           // i11: wrap-forward, advances stored=0
		{csn: 1, allow: true},           // i12: advances stored=1
		{csn: 0, allow: false},          // i13
		{csn: 1, allow: true},           // i14
		{csn: 0xffff, allow: false},     // i15
	}

	f := &OpenFile{}
	for i, s := range steps {
		got := f.VerifyChannelSequence(s.csn, true /*modify*/)
		if got != s.allow {
			t.Fatalf("step %d csn=0x%04x: got allow=%v, want %v (stored=0x%04x)",
				i, s.csn, got, s.allow, f.channelSeq)
		}
	}
}

// TestVerifyChannelSequence_ReadAdvancesModifyRejects reproduces the core of
// smb2.replay.replay4: a READ on an incremented ChannelSequence advances the
// Open's tracked sequence, after which a WRITE that resends on the original
// (now-stale) sequence must be rejected, while a READ on the stale sequence is
// still allowed.
func TestVerifyChannelSequence_ReadAdvancesModifyRejects(t *testing.T) {
	f := &OpenFile{}

	// Initial WRITE at csn=0 seeds the Open.
	if !f.VerifyChannelSequence(0, true) {
		t.Fatal("initial write at csn=0 should be allowed")
	}
	// READ at csn=1 (channel failover) advances the tracked sequence.
	if !f.VerifyChannelSequence(1, false) {
		t.Fatal("read at csn=1 should be allowed")
	}
	// WRITE resent at stale csn=0 must be rejected.
	if f.VerifyChannelSequence(0, true) {
		t.Fatal("write at stale csn=0 should be rejected")
	}
	// SET_INFO at stale csn=0 must also be rejected.
	if f.VerifyChannelSequence(0, true) {
		t.Fatal("set_info at stale csn=0 should be rejected")
	}
	// READ at stale csn=0 is allowed (read-only ops never reject).
	if !f.VerifyChannelSequence(0, false) {
		t.Fatal("read at stale csn=0 should be allowed")
	}
}

// TestVerifyChannelSequence_ReplayResend covers the resend semantics from
// replay4: a write resent on the correct (current) ChannelSequence is allowed,
// and a write resent on a stale ChannelSequence is rejected. The
// REPLAY_OPERATION flag does not alter the decision, so these assertions hold
// for both replayed and fresh requests.
func TestVerifyChannelSequence_ReplayResend(t *testing.T) {
	f := &OpenFile{}

	// Seed at csn=0, then advance to csn=5 via a write on the new channel.
	if !f.VerifyChannelSequence(0, true) {
		t.Fatal("seed write should be allowed")
	}
	if !f.VerifyChannelSequence(5, true) {
		t.Fatal("write at csn=5 should be allowed")
	}

	// Write resent on the current sequence (csn=5) is allowed.
	if !f.VerifyChannelSequence(5, true) {
		t.Fatal("write resend on current csn should be allowed")
	}
	// Write resent on a stale sequence (csn=0) is rejected.
	if f.VerifyChannelSequence(0, true) {
		t.Fatal("write resend on stale csn should be rejected")
	}
}

// TestVerifyChannelSequence_NonSMB3GateNoop documents that a fresh Open seeds
// from whatever the first request's ChannelSequence is, so an initial nonzero
// sequence is not mistaken for a failover.
func TestVerifyChannelSequence_SeedNonZero(t *testing.T) {
	f := &OpenFile{}
	if !f.VerifyChannelSequence(0x1234, true) {
		t.Fatal("first modify at nonzero csn should seed and be allowed")
	}
	if f.channelSeq != 0x1234 {
		t.Fatalf("expected seeded channelSeq=0x1234, got 0x%04x", f.channelSeq)
	}
	// A stale write below the seed is rejected.
	if f.VerifyChannelSequence(0x1233, true) {
		t.Fatal("write below seeded csn should be rejected")
	}
}
