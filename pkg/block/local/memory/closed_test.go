package memory

import "testing"

func TestMemoryStore_NotClosedOnFreshStore(t *testing.T) {
	s := New()
	if s.Closed() {
		t.Fatal("fresh store reports closed")
	}
}

func TestMemoryStore_ClosedAfterClose(t *testing.T) {
	s := New()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.Closed() {
		t.Fatal("closed store does not report closed")
	}
}
