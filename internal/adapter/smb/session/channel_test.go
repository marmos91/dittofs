package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func TestSession_AddGetRemoveChannel(t *testing.T) {
	s := NewSession(42, "127.0.0.1:1234", false, "alice", "WORKGROUP")

	if got := s.GetChannel(100); got != nil {
		t.Fatalf("GetChannel on empty session returned %+v, want nil", got)
	}

	ch := &Channel{
		ConnID:     100,
		RemoteAddr: "10.0.0.1:4455",
		Dialect:    types.Dialect0311,
		SigningKey: make([]byte, 16),
	}
	s.AddChannel(ch)

	got := s.GetChannel(100)
	if got == nil {
		t.Fatal("GetChannel after AddChannel returned nil")
	}
	if got.ConnID != 100 || got.RemoteAddr != "10.0.0.1:4455" {
		t.Fatalf("channel fields not preserved: %+v", got)
	}
	if got.BoundAt.IsZero() {
		t.Fatal("AddChannel did not stamp BoundAt when zero")
	}

	s.RemoveChannel(100)
	if got := s.GetChannel(100); got != nil {
		t.Fatalf("GetChannel after RemoveChannel returned %+v, want nil", got)
	}
}

func TestSession_AddChannel_PreservesExplicitBoundAt(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	when := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	s.AddChannel(&Channel{ConnID: 1, BoundAt: when})

	got := s.GetChannel(1)
	if !got.BoundAt.Equal(when) {
		t.Fatalf("BoundAt=%v, want %v (AddChannel should not overwrite explicit value)", got.BoundAt, when)
	}
}

func TestSession_AddChannel_NilIsNoop(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	s.AddChannel(nil) // must not panic
	if got := s.ListChannels(); len(got) != 0 {
		t.Fatalf("ListChannels after nil AddChannel = %d, want 0", len(got))
	}
}

func TestSession_ListChannels(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	for _, id := range []uint64{10, 20, 30} {
		s.AddChannel(&Channel{ConnID: id})
	}

	got := s.ListChannels()
	if len(got) != 3 {
		t.Fatalf("ListChannels len=%d, want 3", len(got))
	}

	seen := make(map[uint64]bool)
	for _, c := range got {
		seen[c.ConnID] = true
	}
	for _, id := range []uint64{10, 20, 30} {
		if !seen[id] {
			t.Fatalf("ListChannels missing ConnID=%d; got=%+v", id, got)
		}
	}
}

func TestSession_AddChannel_ReplaceSameConnID(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	s.AddChannel(&Channel{ConnID: 7, RemoteAddr: "first"})
	s.AddChannel(&Channel{ConnID: 7, RemoteAddr: "second"})

	got := s.GetChannel(7)
	if got == nil || got.RemoteAddr != "second" {
		t.Fatalf("second AddChannel did not replace first: got=%+v", got)
	}
	if l := s.ListChannels(); len(l) != 1 {
		t.Fatalf("ListChannels len=%d, want 1 after same-ConnID replacement", len(l))
	}
}

// TestSession_AddChannel_CapReject verifies the per-session channel cap
// (MS-SMB2 §3.3.5.5.2 / smb2.multichannel.generic.num_channels): channels 1..N
// where N = MaxChannelsPerSession must succeed, channel N+1 must fail.
func TestSession_AddChannel_CapReject(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	for i := 0; i < MaxChannelsPerSession; i++ {
		if !s.AddChannel(&Channel{ConnID: uint64(i + 1)}) {
			t.Fatalf("AddChannel #%d rejected unexpectedly", i+1)
		}
	}
	if s.AddChannel(&Channel{ConnID: uint64(MaxChannelsPerSession + 1)}) {
		t.Fatalf("AddChannel past cap (%d) succeeded; must be rejected",
			MaxChannelsPerSession)
	}
	if got := s.ChannelCount(); got != MaxChannelsPerSession {
		t.Fatalf("ChannelCount=%d, want %d", got, MaxChannelsPerSession)
	}

	// Removing a slot must free capacity for a new channel.
	s.RemoveChannel(1)
	if !s.AddChannel(&Channel{ConnID: 999}) {
		t.Fatalf("AddChannel after RemoveChannel rejected; cap slot should free")
	}
}

// TestSession_AddChannel_ReplaceDoesNotCountAgainstCap verifies that updating
// an already-registered ConnID does not consume an additional slot.
func TestSession_AddChannel_ReplaceDoesNotCountAgainstCap(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	for i := 0; i < MaxChannelsPerSession; i++ {
		if !s.AddChannel(&Channel{ConnID: uint64(i + 1)}) {
			t.Fatalf("AddChannel #%d rejected unexpectedly", i+1)
		}
	}
	// Re-adding an existing ConnID with updated state must succeed even at cap.
	if !s.AddChannel(&Channel{ConnID: 1, RemoteAddr: "updated"}) {
		t.Fatal("replacing existing ConnID was rejected at cap; it should not count")
	}
	got := s.GetChannel(1)
	if got == nil || got.RemoteAddr != "updated" {
		t.Fatalf("replacement did not take effect: %+v", got)
	}
}

// TestSession_AddChannel_ConcurrentCap stresses concurrent binds far in excess
// of the cap and verifies exactly MaxChannelsPerSession succeed — guarding the
// atomicity of the count-and-insert under concurrent AddChannel calls. A naive
// sync.Map-backed registry with a "len() < cap then Store" sequence would
// admit more than MaxChannelsPerSession under race.
func TestSession_AddChannel_ConcurrentCap(t *testing.T) {
	s := NewSession(1, "", false, "", "")

	const totalAttempts = 200
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < totalAttempts; i++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			if s.AddChannel(&Channel{ConnID: id}) {
				accepted.Add(1)
			}
		}(uint64(i + 1))
	}
	wg.Wait()

	if got := accepted.Load(); got != int64(MaxChannelsPerSession) {
		t.Fatalf("accepted=%d, want exactly %d", got, MaxChannelsPerSession)
	}
	if got := s.ChannelCount(); got != MaxChannelsPerSession {
		t.Fatalf("ChannelCount=%d, want %d", got, MaxChannelsPerSession)
	}
}

// TestSession_AddChannel_RefusesALoggedOffSession pins the refusal at the
// registration itself rather than at the callers that check LoggedOff before
// asking. A bind handshake is several round trips long, and a LOGOFF arriving
// inside one leaves the caller's check stale by the time it attaches: without
// this the session would take a channel whose connection it can never
// authenticate, its signing key having already been retired.
func TestSession_AddChannel_RefusesALoggedOffSession(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	s.LoggedOff.Store(true)

	if s.AddChannel(&Channel{ConnID: 1}) {
		t.Fatal("a logged-off session accepted a new channel")
	}
	if got := s.ChannelCount(); got != 0 {
		t.Fatalf("ChannelCount=%d, want 0: the refused channel was registered anyway", got)
	}
}

// TestSession_RemoveChannelRetiringLast_RefusesALaterAddChannel pins the second
// way a session reaches the retired state. LOGOFF sets LoggedOff explicitly;
// transport teardown does not — it removes the closing connection's channel and
// deletes the session once none are left. A bind on another connection that has
// already passed its own session lookup would otherwise register a channel in
// between, on a session the teardown is about to delete.
func TestSession_RemoveChannelRetiringLast_RefusesALaterAddChannel(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	if !s.AddChannel(&Channel{ConnID: 1}) {
		t.Fatal("setup: the first channel was refused")
	}

	if !s.RemoveChannelRetiringLast(1) {
		t.Fatal("removing the only channel must report the session retired")
	}
	if s.AddChannel(&Channel{ConnID: 2}) {
		t.Error("a session whose last channel closed accepted a new one: transport teardown is about to delete it")
	}
	if got := s.ChannelCount(); got != 0 {
		t.Errorf("ChannelCount=%d, want 0: the refused channel was registered anyway", got)
	}
}

// TestSession_RemoveChannelRetiringLast_KeepsASurvivingSession is the other half:
// a session that still holds a channel is not retired and still takes new ones,
// so the multichannel survival rule (MS-SMB2 §3.3.7.1) is unaffected.
func TestSession_RemoveChannelRetiringLast_KeepsASurvivingSession(t *testing.T) {
	s := NewSession(1, "", false, "", "")
	s.AddChannel(&Channel{ConnID: 1})
	s.AddChannel(&Channel{ConnID: 2})

	if s.RemoveChannelRetiringLast(1) {
		t.Fatal("a session with a surviving channel was reported retired")
	}
	if s.LoggedOff.Load() {
		t.Error("a session with a surviving channel must not be marked logged off")
	}
	if !s.AddChannel(&Channel{ConnID: 3}) {
		t.Error("a surviving session refused a new channel")
	}
}
