package blockstore

import (
	"bytes"
	"errors"
	"testing"
)

func testHash(b byte) ContentHash {
	var h ContentHash
	h[0] = b
	return h
}

func TestHashSet_Add_Contains(t *testing.T) {
	s := NewHashSet(0)
	h1, h2, h3 := testHash(1), testHash(2), testHash(3)
	absent := testHash(99)

	s.Add(h1)
	s.Add(h2)
	s.Add(h3)

	if !s.Contains(h1) {
		t.Fatalf("expected Contains(h1) = true")
	}
	if !s.Contains(h2) {
		t.Fatalf("expected Contains(h2) = true")
	}
	if !s.Contains(h3) {
		t.Fatalf("expected Contains(h3) = true")
	}
	if s.Contains(absent) {
		t.Fatalf("expected Contains(absent) = false")
	}
}

func TestHashSet_Len(t *testing.T) {
	s := NewHashSet(0)
	if s.Len() != 0 {
		t.Fatalf("empty set: got Len()=%d, want 0", s.Len())
	}

	s.Add(testHash(1))
	s.Add(testHash(2))
	s.Add(testHash(3))
	if s.Len() != 3 {
		t.Fatalf("after 3 adds: got Len()=%d, want 3", s.Len())
	}

	// Duplicate add must not change length.
	s.Add(testHash(1))
	if s.Len() != 3 {
		t.Fatalf("after duplicate add: got Len()=%d, want 3", s.Len())
	}
}

func TestHashSet_ForEach(t *testing.T) {
	s := NewHashSet(0)
	h1, h2, h3 := testHash(1), testHash(2), testHash(3)
	s.Add(h1)
	s.Add(h2)
	s.Add(h3)

	seen := make(map[ContentHash]bool)
	err := s.ForEach(func(h ContentHash) error {
		seen[h] = true
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach returned unexpected error: %v", err)
	}
	for _, h := range []ContentHash{h1, h2, h3} {
		if !seen[h] {
			t.Fatalf("ForEach did not visit hash %x", h)
		}
	}
}

func TestHashSet_ForEach_ErrorPropagation(t *testing.T) {
	s := NewHashSet(0)
	s.Add(testHash(1))
	s.Add(testHash(2))
	s.Add(testHash(3))

	sentinel := errors.New("stop iteration")
	calls := 0
	err := s.ForEach(func(_ ContentHash) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestHashSet_Sorted(t *testing.T) {
	s := NewHashSet(0)
	// Add in reverse order to verify sorting.
	s.Add(testHash(3))
	s.Add(testHash(1))
	s.Add(testHash(2))

	sorted := s.Sorted()
	if len(sorted) != 3 {
		t.Fatalf("Sorted() len=%d, want 3", len(sorted))
	}
	for i := 1; i < len(sorted); i++ {
		if bytes.Compare(sorted[i-1][:], sorted[i][:]) >= 0 {
			t.Fatalf("Sorted() not ascending at index %d: %x >= %x", i, sorted[i-1], sorted[i])
		}
	}
}

func TestHashSet_Hashes(t *testing.T) {
	s := NewHashSet(0)
	h1 := testHash(1)
	s.Add(h1)

	m := s.Hashes()
	// Verify identity (same map, not a copy).
	m[testHash(42)] = struct{}{}
	if !s.Contains(testHash(42)) {
		t.Fatalf("Hashes() should return the internal map (not a copy)")
	}
}
