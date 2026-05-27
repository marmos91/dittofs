package blockstore

import (
	"bytes"
	"slices"
)

// HashSet is an unordered collection of unique ContentHash values. It is
// used by the backup system to report which block hashes are referenced
// by a metadata snapshot so the orchestrator can place GC holds and build
// the block manifest.
//
// HashSet is NOT safe for concurrent use. Callers that share a HashSet
// across goroutines must synchronize externally.
type HashSet struct {
	m map[ContentHash]struct{}
}

// NewHashSet returns a new HashSet pre-allocated for sizeHint entries.
// A sizeHint of 0 is valid and avoids any upfront allocation.
func NewHashSet(sizeHint int) *HashSet {
	return &HashSet{m: make(map[ContentHash]struct{}, sizeHint)}
}

// Add inserts h into the set. If h is already present, Add is a no-op.
func (s *HashSet) Add(h ContentHash) {
	s.m[h] = struct{}{}
}

// Contains reports whether h is in the set.
func (s *HashSet) Contains(h ContentHash) bool {
	_, ok := s.m[h]
	return ok
}

// Len returns the number of unique hashes in the set.
func (s *HashSet) Len() int {
	return len(s.m)
}

// ForEach calls fn for each hash in the set in unspecified order.
// If fn returns a non-nil error, iteration stops and that error is
// returned to the caller.
func (s *HashSet) ForEach(fn func(ContentHash) error) error {
	for h := range s.m {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// Sorted returns all hashes in the set sorted in ascending
// lexicographic order (bytes.Compare).
func (s *HashSet) Sorted() []ContentHash {
	out := make([]ContentHash, 0, len(s.m))
	for h := range s.m {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b ContentHash) int {
		return bytes.Compare(a[:], b[:])
	})
	return out
}

// Hashes returns the underlying map for direct read access.
// The returned map is NOT a copy; mutations are visible to the HashSet.
func (s *HashSet) Hashes() map[ContentHash]struct{} {
	return s.m
}
