package metadata

import (
	"testing"
)

func TestRangesOverlap(t *testing.T) {
	tests := []struct {
		name     string
		o1, l1   uint64
		o2, l2   uint64
		expected bool
	}{
		// Non-overlapping ranges
		{
			name: "adjacent ranges - no overlap",
			o1:   0, l1: 10,
			o2: 10, l2: 10,
			expected: false,
		},
		{
			name: "separated ranges - no overlap",
			o1:   0, l1: 10,
			o2: 20, l2: 10,
			expected: false,
		},
		// Overlapping ranges
		{
			name: "partial overlap at end",
			o1:   0, l1: 10,
			o2: 5, l2: 10,
			expected: true,
		},
		{
			name: "partial overlap at start",
			o1:   5, l1: 10,
			o2: 0, l2: 10,
			expected: true,
		},
		{
			name: "one range inside another",
			o1:   0, l1: 20,
			o2: 5, l2: 10,
			expected: true,
		},
		{
			name: "exact same range",
			o1:   0, l1: 10,
			o2: 0, l2: 10,
			expected: true,
		},
		// Unbounded ranges (length=0)
		{
			name: "both unbounded - overlap",
			o1:   0, l1: 0,
			o2: 100, l2: 0,
			expected: true,
		},
		{
			name: "first unbounded overlapping bounded",
			o1:   0, l1: 0,
			o2: 50, l2: 10,
			expected: true,
		},
		{
			name: "second unbounded overlapping bounded",
			o1:   50, l1: 10,
			o2: 0, l2: 0,
			expected: true,
		},
		{
			name: "first unbounded after bounded - overlap",
			o1:   100, l1: 0,
			o2: 50, l2: 100,
			expected: true,
		},
		// Edge cases
		{
			name: "zero length bounded range",
			o1:   0, l1: 10,
			o2: 5, l2: 0, // unbounded starting at 5
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RangesOverlap(tt.o1, tt.l1, tt.o2, tt.l2)
			if result != tt.expected {
				t.Errorf("RangesOverlap(%d, %d, %d, %d) = %v, want %v",
					tt.o1, tt.l1, tt.o2, tt.l2, result, tt.expected)
			}

			// Test symmetry: overlap(a,b) == overlap(b,a)
			reversed := RangesOverlap(tt.o2, tt.l2, tt.o1, tt.l1)
			if reversed != tt.expected {
				t.Errorf("RangesOverlap (reversed)(%d, %d, %d, %d) = %v, want %v",
					tt.o2, tt.l2, tt.o1, tt.l1, reversed, tt.expected)
			}
		})
	}
}

func TestIsLockConflicting(t *testing.T) {
	tests := []struct {
		name     string
		existing FileLock
		request  FileLock
		expected bool
	}{
		// Same session - never conflicts
		{
			name:     "same session shared locks - no conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			request:  FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			expected: false,
		},
		{
			name:     "same session exclusive locks - no conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			request:  FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			expected: false,
		},
		{
			name:     "same session upgrade shared to exclusive - no conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			request:  FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			expected: false,
		},
		// Different sessions - shared locks on same range
		{
			name:     "different sessions shared locks - no conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			request:  FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: false},
			expected: false,
		},
		// Different sessions - exclusive lock conflicts
		{
			name:     "different sessions existing exclusive - conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			request:  FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: false},
			expected: true,
		},
		{
			name:     "different sessions request exclusive - conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			request:  FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: true},
			expected: true,
		},
		{
			name:     "different sessions both exclusive - conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			request:  FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: true},
			expected: true,
		},
		// Non-overlapping ranges
		{
			name:     "different sessions exclusive non-overlapping - no conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 50, Exclusive: true},
			request:  FileLock{SessionID: 2, Offset: 50, Length: 50, Exclusive: true},
			expected: false,
		},
		// Partial overlap
		{
			name:     "partial overlap with exclusive - conflict",
			existing: FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			request:  FileLock{SessionID: 2, Offset: 50, Length: 100, Exclusive: false},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsLockConflicting(&tt.existing, &tt.request)
			if result != tt.expected {
				t.Errorf("IsLockConflicting() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCheckIOConflict(t *testing.T) {
	tests := []struct {
		name      string
		lock      FileLock
		sessionID uint64
		offset    uint64
		length    uint64
		isWrite   bool
		expected  bool
	}{
		// Same session - no conflict
		{
			name:      "same session read through exclusive - no conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			sessionID: 1, offset: 0, length: 50, isWrite: false,
			expected: false,
		},
		{
			name:      "same session write through exclusive - no conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			sessionID: 1, offset: 0, length: 50, isWrite: true,
			expected: false,
		},
		// Different session reads
		{
			name:      "different session read through shared - no conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			sessionID: 2, offset: 0, length: 50, isWrite: false,
			expected: false,
		},
		{
			name:      "different session read through exclusive - conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			sessionID: 2, offset: 0, length: 50, isWrite: false,
			expected: true,
		},
		// Different session writes
		{
			name:      "different session write through shared - conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false},
			sessionID: 2, offset: 0, length: 50, isWrite: true,
			expected: true,
		},
		{
			name:      "different session write through exclusive - conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true},
			sessionID: 2, offset: 0, length: 50, isWrite: true,
			expected: true,
		},
		// Non-overlapping ranges
		{
			name:      "non-overlapping ranges write - no conflict",
			lock:      FileLock{SessionID: 1, Offset: 0, Length: 50, Exclusive: true},
			sessionID: 2, offset: 50, length: 50, isWrite: true,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckIOConflict(&tt.lock, tt.sessionID, tt.offset, tt.length, tt.isWrite)
			if result != tt.expected {
				t.Errorf("CheckIOConflict() = %v, want %v", result, tt.expected)
			}
		})
	}
}
