package session

import (
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
