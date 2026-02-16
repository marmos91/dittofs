package gss

import (
	"sync"
	"testing"
)

func TestSeqWindow_AcceptNewSequenceNumbers(t *testing.T) {
	w := NewSeqWindow(128)

	// First sequence number should be accepted
	if !w.Accept(1) {
		t.Error("Accept(1) should return true for first seq")
	}

	// Second sequence number should be accepted
	if !w.Accept(2) {
		t.Error("Accept(2) should return true for new seq")
	}

	// Non-sequential but within window should be accepted
	if !w.Accept(5) {
		t.Error("Accept(5) should return true for new seq within window")
	}

	// Fill in gaps
	if !w.Accept(3) {
		t.Error("Accept(3) should return true for new seq within window")
	}
	if !w.Accept(4) {
		t.Error("Accept(4) should return true for new seq within window")
	}
}

func TestSeqWindow_RejectDuplicates(t *testing.T) {
	w := NewSeqWindow(128)

	if !w.Accept(1) {
		t.Fatal("Accept(1) should succeed first time")
	}

	// Duplicate should be rejected
	if w.Accept(1) {
		t.Error("Accept(1) should return false for duplicate")
	}

	// Accept another, then duplicate
	if !w.Accept(5) {
		t.Fatal("Accept(5) should succeed")
	}
	if w.Accept(5) {
		t.Error("Accept(5) should return false for duplicate")
	}

	// Fill in a gap and duplicate
	if !w.Accept(3) {
		t.Fatal("Accept(3) should succeed")
	}
	if w.Accept(3) {
		t.Error("Accept(3) should return false for duplicate")
	}
}

func TestSeqWindow_RejectBelowWindow(t *testing.T) {
	w := NewSeqWindow(10)

	// Accept seq 1, then advance far enough that 1 falls out
	w.Accept(1)

	// Advance to seq 15 (window is [6..15])
	w.Accept(15)

	// Seq 1 is below window (15 - 10 + 1 = 6 is the lowest valid)
	if w.Accept(1) {
		t.Error("Accept(1) should return false, below window")
	}

	// Seq 5 is below window
	if w.Accept(5) {
		t.Error("Accept(5) should return false, below window")
	}

	// Seq 6 is the edge of the window, should be accepted
	if !w.Accept(6) {
		t.Error("Accept(6) should return true, at window edge")
	}
}

func TestSeqWindow_SlideForward(t *testing.T) {
	w := NewSeqWindow(10)

	// Accept 1..5
	for i := uint32(1); i <= 5; i++ {
		if !w.Accept(i) {
			t.Errorf("Accept(%d) should succeed", i)
		}
	}

	// Jump to 20, window should slide to [11..20]
	if !w.Accept(20) {
		t.Error("Accept(20) should succeed")
	}

	// Old sequences should be rejected
	if w.Accept(5) {
		t.Error("Accept(5) should fail after window slide")
	}
	if w.Accept(10) {
		t.Error("Accept(10) should fail after window slide")
	}

	// Within new window should work
	if !w.Accept(11) {
		t.Error("Accept(11) should succeed in new window")
	}
	if !w.Accept(15) {
		t.Error("Accept(15) should succeed in new window")
	}
}

func TestSeqWindow_LargeSlide(t *testing.T) {
	w := NewSeqWindow(10)

	w.Accept(1)

	// Slide by more than window size (clears entire bitmap)
	if !w.Accept(100) {
		t.Error("Accept(100) should succeed after large slide")
	}

	// Everything old should be rejected
	if w.Accept(1) {
		t.Error("Accept(1) should fail after large slide")
	}
	if w.Accept(89) {
		t.Error("Accept(89) should fail, below window after large slide")
	}

	// Edge of new window
	if !w.Accept(91) {
		t.Error("Accept(91) should succeed in new window")
	}
}

func TestSeqWindow_MAXSEQ(t *testing.T) {
	w := NewSeqWindow(128)

	// MAXSEQ should be accepted
	if !w.Accept(MAXSEQ) {
		t.Error("Accept(MAXSEQ) should succeed")
	}

	// Above MAXSEQ should be rejected
	if w.Accept(MAXSEQ + 1) {
		t.Error("Accept(MAXSEQ+1) should fail")
	}

	// Duplicate MAXSEQ should be rejected
	if w.Accept(MAXSEQ) {
		t.Error("Accept(MAXSEQ) duplicate should fail")
	}
}

func TestSeqWindow_RejectZero(t *testing.T) {
	w := NewSeqWindow(128)

	// Zero is not a valid RPCSEC_GSS sequence number
	if w.Accept(0) {
		t.Error("Accept(0) should return false")
	}
}

func TestSeqWindow_ConcurrentAccess(t *testing.T) {
	w := NewSeqWindow(1000)

	// Run many goroutines accepting different sequence numbers
	var wg sync.WaitGroup
	const numGoroutines = 100
	const seqPerGoroutine = 10

	accepted := make([]bool, numGoroutines*seqPerGoroutine+1)
	var mu sync.Mutex

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < seqPerGoroutine; i++ {
				seq := uint32(goroutineID*seqPerGoroutine + i + 1)
				result := w.Accept(seq)
				mu.Lock()
				accepted[seq] = result
				mu.Unlock()
			}
		}(g)
	}

	wg.Wait()

	// Each unique sequence number should have been accepted exactly once
	for seq := uint32(1); seq <= uint32(numGoroutines*seqPerGoroutine); seq++ {
		if !accepted[seq] {
			t.Errorf("Sequence %d should have been accepted", seq)
		}
	}

	// Verify no duplicates accepted
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < seqPerGoroutine; i++ {
				seq := uint32(goroutineID*seqPerGoroutine + i + 1)
				if w.Accept(seq) {
					t.Errorf("Duplicate sequence %d should not be accepted", seq)
				}
			}
		}(g)
	}

	wg.Wait()
}

func TestSeqWindow_Reset(t *testing.T) {
	w := NewSeqWindow(128)

	w.Accept(1)
	w.Accept(2)
	w.Accept(3)

	w.Reset()

	// After reset, sequence numbers should be accepted again
	if !w.Accept(1) {
		t.Error("Accept(1) should succeed after Reset()")
	}
}

func TestSeqWindow_SmallWindow(t *testing.T) {
	// Window size of 1 should still work
	w := NewSeqWindow(1)

	if !w.Accept(1) {
		t.Error("Accept(1) should succeed with window size 1")
	}

	// Only the highest seq is in the window
	if !w.Accept(2) {
		t.Error("Accept(2) should succeed")
	}

	// 1 is now below the window
	if w.Accept(1) {
		t.Error("Accept(1) should fail with window size 1 after Accept(2)")
	}
}

func TestSeqWindow_WindowEdgeCases(t *testing.T) {
	w := NewSeqWindow(64) // exactly one uint64 in bitmap

	// Fill window sequentially
	for i := uint32(1); i <= 64; i++ {
		if !w.Accept(i) {
			t.Errorf("Accept(%d) should succeed during fill", i)
		}
	}

	// All should be duplicates
	for i := uint32(1); i <= 64; i++ {
		if w.Accept(i) {
			t.Errorf("Accept(%d) should fail as duplicate", i)
		}
	}

	// Accept one more, pushing 1 out of window
	if !w.Accept(65) {
		t.Error("Accept(65) should succeed")
	}

	// 1 is now out of window
	if w.Accept(1) {
		t.Error("Accept(1) should fail after sliding past")
	}

	// 2 should still be in window (edge: 65-64+1 = 2)
	if w.Accept(2) {
		// 2 was already seen, so this should be a duplicate
		t.Error("Accept(2) should fail as duplicate")
	}
}
