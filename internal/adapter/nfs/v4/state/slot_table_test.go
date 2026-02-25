package state

import (
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// NewSlotTable Tests
// ============================================================================

func TestNewSlotTable(t *testing.T) {
	t.Run("normal creation", func(t *testing.T) {
		st := NewSlotTable(8)
		if st.MaxSlots() != 8 {
			t.Errorf("MaxSlots() = %d, want 8", st.MaxSlots())
		}
		if st.GetHighestSlotID() != 7 {
			t.Errorf("GetHighestSlotID() = %d, want 7", st.GetHighestSlotID())
		}
		if st.GetTargetHighestSlotID() != 7 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 7", st.GetTargetHighestSlotID())
		}
	})

	t.Run("zero slots clamped to MinSlots", func(t *testing.T) {
		st := NewSlotTable(0)
		if st.MaxSlots() != MinSlots {
			t.Errorf("MaxSlots() = %d, want %d (MinSlots)", st.MaxSlots(), MinSlots)
		}
	})

	t.Run("exceeds DefaultMaxSlots clamped", func(t *testing.T) {
		st := NewSlotTable(DefaultMaxSlots + 100)
		if st.MaxSlots() != DefaultMaxSlots {
			t.Errorf("MaxSlots() = %d, want %d (DefaultMaxSlots)", st.MaxSlots(), DefaultMaxSlots)
		}
	})

	t.Run("single slot", func(t *testing.T) {
		st := NewSlotTable(1)
		if st.MaxSlots() != 1 {
			t.Errorf("MaxSlots() = %d, want 1", st.MaxSlots())
		}
		if st.GetHighestSlotID() != 0 {
			t.Errorf("GetHighestSlotID() = %d, want 0", st.GetHighestSlotID())
		}
	})
}

// ============================================================================
// ValidateSequence Tests - New Request
// ============================================================================

func TestValidateSequence_NewRequest(t *testing.T) {
	t.Run("first request on fresh slot", func(t *testing.T) {
		st := NewSlotTable(4)

		// Fresh slot: SeqID=0, expected next = 1
		result, slot, err := st.ValidateSequence(0, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != SeqNew {
			t.Errorf("result = %d, want SeqNew (%d)", result, SeqNew)
		}
		if slot == nil {
			t.Fatal("slot should not be nil for SeqNew")
		}
	})

	t.Run("subsequent request after complete", func(t *testing.T) {
		st := NewSlotTable(4)

		// ValidateSequence atomically marks slot in-use for SeqNew
		_, _, _ = st.ValidateSequence(0, 1)
		st.CompleteSlotRequest(0, 1, true, []byte("reply1"))

		// Next request: seqID=2
		result, slot, err := st.ValidateSequence(0, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != SeqNew {
			t.Errorf("result = %d, want SeqNew", result)
		}
		if slot == nil {
			t.Fatal("slot should not be nil")
		}
	})

	t.Run("different slots independent", func(t *testing.T) {
		st := NewSlotTable(4)

		// Both slots start fresh, both expect seqID=1
		result0, _, err0 := st.ValidateSequence(0, 1)
		if err0 != nil || result0 != SeqNew {
			t.Errorf("slot 0: result=%d, err=%v; want SeqNew, nil", result0, err0)
		}

		result1, _, err1 := st.ValidateSequence(1, 1)
		if err1 != nil || result1 != SeqNew {
			t.Errorf("slot 1: result=%d, err=%v; want SeqNew, nil", result1, err1)
		}
	})
}

// ============================================================================
// ValidateSequence Tests - Retry
// ============================================================================

func TestValidateSequence_Retry(t *testing.T) {
	st := NewSlotTable(4)

	// Set up: complete request with seqID=5, cache reply
	st.CompleteSlotRequest(0, 5, true, []byte("cached-data"))

	// Retry: seqID=5 (same as cached)
	result, slot, err := st.ValidateSequence(0, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != SeqRetry {
		t.Errorf("result = %d, want SeqRetry (%d)", result, SeqRetry)
	}
	if slot == nil {
		t.Fatal("slot should not be nil for SeqRetry")
	}
	if string(slot.CachedReply) != "cached-data" {
		t.Errorf("CachedReply = %q, want %q", slot.CachedReply, "cached-data")
	}
}

// ============================================================================
// ValidateSequence Tests - Retry Uncached
// ============================================================================

func TestValidateSequence_RetryUncached(t *testing.T) {
	st := NewSlotTable(4)

	// Set up: complete request with seqID=5, cacheThis=false
	st.CompleteSlotRequest(0, 5, false, nil)

	// Retry: seqID=5 (same as cached, but no cached reply)
	_, _, err := st.ValidateSequence(0, 5)
	if err == nil {
		t.Fatal("expected error for uncached retry")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected *NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_RETRY_UNCACHED_REP {
		t.Errorf("status = %d, want NFS4ERR_RETRY_UNCACHED_REP (%d)",
			stateErr.Status, types.NFS4ERR_RETRY_UNCACHED_REP)
	}
}

// ============================================================================
// ValidateSequence Tests - Misordered
// ============================================================================

func TestValidateSequence_Misordered(t *testing.T) {
	st := NewSlotTable(4)

	// Set up slot at seqID=5
	st.CompleteSlotRequest(0, 5, true, []byte("reply"))

	t.Run("behind (old seqid)", func(t *testing.T) {
		_, _, err := st.ValidateSequence(0, 3)
		if err == nil {
			t.Fatal("expected error for old seqid")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_SEQ_MISORDERED {
			t.Errorf("status = %d, want NFS4ERR_SEQ_MISORDERED (%d)",
				stateErr.Status, types.NFS4ERR_SEQ_MISORDERED)
		}
	})

	t.Run("gap (ahead by more than 1)", func(t *testing.T) {
		_, _, err := st.ValidateSequence(0, 7)
		if err == nil {
			t.Fatal("expected error for gap seqid")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_SEQ_MISORDERED {
			t.Errorf("status = %d, want NFS4ERR_SEQ_MISORDERED (%d)",
				stateErr.Status, types.NFS4ERR_SEQ_MISORDERED)
		}
	})
}

// ============================================================================
// ValidateSequence Tests - Bad Slot
// ============================================================================

func TestValidateSequence_BadSlot(t *testing.T) {
	st := NewSlotTable(4) // slots 0-3

	t.Run("slotID equals maxSlots", func(t *testing.T) {
		_, _, err := st.ValidateSequence(4, 1)
		if err == nil {
			t.Fatal("expected error for out-of-range slot")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_BADSLOT {
			t.Errorf("status = %d, want NFS4ERR_BADSLOT (%d)",
				stateErr.Status, types.NFS4ERR_BADSLOT)
		}
	})

	t.Run("slotID far out of range", func(t *testing.T) {
		_, _, err := st.ValidateSequence(100, 1)
		if err == nil {
			t.Fatal("expected error for far out-of-range slot")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_BADSLOT {
			t.Errorf("status = %d, want NFS4ERR_BADSLOT (%d)",
				stateErr.Status, types.NFS4ERR_BADSLOT)
		}
	})
}

// ============================================================================
// ValidateSequence Tests - Slot In-Use
// ============================================================================

func TestValidateSequence_SlotInUse(t *testing.T) {
	st := NewSlotTable(4)

	// ValidateSequence(0, 1) returns SeqNew and atomically marks slot in-use
	result, _, err := st.ValidateSequence(0, 1)
	if err != nil || result != SeqNew {
		t.Fatalf("initial validate failed: result=%d, err=%v", result, err)
	}

	t.Run("retransmission of in-flight request", func(t *testing.T) {
		// Same seqID=1 while slot is in-use should return NFS4ERR_DELAY
		// (the client is retransmitting the request that's still processing)
		_, _, err := st.ValidateSequence(0, 1)
		if err == nil {
			t.Fatal("expected error for retransmission while in flight")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_DELAY {
			t.Errorf("status = %d, want NFS4ERR_DELAY (%d)",
				stateErr.Status, types.NFS4ERR_DELAY)
		}
	})

	t.Run("retry of previous completed while in flight", func(t *testing.T) {
		// seqID=0 (the previous completed seqid) while slot is in use
		_, _, err := st.ValidateSequence(0, 0)
		if err == nil {
			t.Fatal("expected error for retry while in flight")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_DELAY {
			t.Errorf("status = %d, want NFS4ERR_DELAY (%d)",
				stateErr.Status, types.NFS4ERR_DELAY)
		}
	})
}

// ============================================================================
// ValidateSequence Tests - SeqID Wraparound
// ============================================================================

func TestValidateSequence_SeqIDWrap(t *testing.T) {
	st := NewSlotTable(4)

	// Set slot seqID to 0xFFFFFFFF via CompleteSlotRequest
	st.CompleteSlotRequest(0, 0xFFFFFFFF, true, []byte("max-reply"))

	// Verify retry of 0xFFFFFFFF works before advancing
	result, slot, err := st.ValidateSequence(0, 0xFFFFFFFF)
	if err != nil {
		t.Fatalf("unexpected error on retry at max: %v", err)
	}
	if result != SeqRetry {
		t.Errorf("result = %d, want SeqRetry (%d)", result, SeqRetry)
	}
	if string(slot.CachedReply) != "max-reply" {
		t.Errorf("CachedReply = %q, want %q", slot.CachedReply, "max-reply")
	}

	// Expected next: 0xFFFFFFFF + 1 = 0 (uint32 natural overflow)
	// In v4.1, seqid=0 IS valid (unlike v4.0 where 0 is reserved)
	result2, slot2, err2 := st.ValidateSequence(0, 0)
	if err2 != nil {
		t.Fatalf("unexpected error on wrap: %v", err2)
	}
	if result2 != SeqNew {
		t.Errorf("result = %d, want SeqNew (%d)", result2, SeqNew)
	}
	if slot2 == nil {
		t.Fatal("slot should not be nil")
	}

	// Complete wrapped request and verify we can continue
	st.CompleteSlotRequest(0, 0, true, []byte("wrap-reply"))

	// Retry of wrapped seqID=0 works
	result3, slot3, err3 := st.ValidateSequence(0, 0)
	if err3 != nil {
		t.Fatalf("unexpected error on retry after wrap: %v", err3)
	}
	if result3 != SeqRetry {
		t.Errorf("result = %d, want SeqRetry (%d)", result3, SeqRetry)
	}
	if string(slot3.CachedReply) != "wrap-reply" {
		t.Errorf("CachedReply = %q, want %q", slot3.CachedReply, "wrap-reply")
	}

	// Next request after wrap: seqID=1
	result4, _, err4 := st.ValidateSequence(0, 1)
	if err4 != nil {
		t.Fatalf("unexpected error on post-wrap next: %v", err4)
	}
	if result4 != SeqNew {
		t.Errorf("result = %d, want SeqNew (%d)", result4, SeqNew)
	}
}

// ============================================================================
// CompleteSlotRequest Tests
// ============================================================================

func TestCompleteSlotRequest(t *testing.T) {
	t.Run("cache reply with cacheThis=true", func(t *testing.T) {
		st := NewSlotTable(4)

		// ValidateSequence atomically marks slot in-use
		_, _, _ = st.ValidateSequence(0, 1)

		originalReply := []byte("original-reply-data")
		st.CompleteSlotRequest(0, 1, true, originalReply)

		// Verify via retry
		result, slot, err := st.ValidateSequence(0, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != SeqRetry {
			t.Errorf("result = %d, want SeqRetry", result)
		}
		if string(slot.CachedReply) != "original-reply-data" {
			t.Errorf("CachedReply = %q, want %q", slot.CachedReply, "original-reply-data")
		}

		// Verify it's a copy, not a reference
		originalReply[0] = 'X'
		if slot.CachedReply[0] == 'X' {
			t.Error("CachedReply should be a copy, not a reference to caller's buffer")
		}
	})

	t.Run("no cache with cacheThis=false", func(t *testing.T) {
		st := NewSlotTable(4)

		// ValidateSequence atomically marks slot in-use
		_, _, _ = st.ValidateSequence(0, 1)

		st.CompleteSlotRequest(0, 1, false, nil)

		// Retry should fail with RETRY_UNCACHED_REP
		_, _, err := st.ValidateSequence(0, 1)
		if err == nil {
			t.Fatal("expected error for uncached replay")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("expected *NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_RETRY_UNCACHED_REP {
			t.Errorf("status = %d, want NFS4ERR_RETRY_UNCACHED_REP",
				stateErr.Status)
		}
	})

	t.Run("out of range slotID no panic", func(t *testing.T) {
		st := NewSlotTable(4)
		// Should not panic for out-of-range slot IDs
		st.CompleteSlotRequest(10, 1, true, []byte("data"))
		st.CompleteSlotRequest(100, 1, true, []byte("data"))
	})

	t.Run("clears InUse flag", func(t *testing.T) {
		st := NewSlotTable(4)

		// ValidateSequence atomically marks slot in-use
		_, _, _ = st.ValidateSequence(0, 1)

		// While in-use, same seqID should return DELAY (retransmission)
		_, _, err := st.ValidateSequence(0, 1)
		if err == nil {
			t.Fatal("expected error while in use")
		}

		// Complete the request
		st.CompleteSlotRequest(0, 1, true, []byte("reply"))

		// Now next seqID should succeed
		result, _, err2 := st.ValidateSequence(0, 2)
		if err2 != nil {
			t.Fatalf("unexpected error after complete: %v", err2)
		}
		if result != SeqNew {
			t.Errorf("result = %d, want SeqNew", result)
		}
	})
}

// ============================================================================
// SetTargetHighestSlotID Tests
// ============================================================================

func TestSetTargetHighestSlotID(t *testing.T) {
	t.Run("within range", func(t *testing.T) {
		st := NewSlotTable(8) // slots 0-7
		st.SetTargetHighestSlotID(3)
		if got := st.GetTargetHighestSlotID(); got != 3 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 3", got)
		}
	})

	t.Run("clamped to maxSlots-1", func(t *testing.T) {
		st := NewSlotTable(8) // slots 0-7
		st.SetTargetHighestSlotID(100)
		if got := st.GetTargetHighestSlotID(); got != 7 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 7 (maxSlots-1)", got)
		}
	})

	t.Run("set to zero", func(t *testing.T) {
		st := NewSlotTable(8)
		st.SetTargetHighestSlotID(0)
		if got := st.GetTargetHighestSlotID(); got != 0 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 0", got)
		}
	})

	t.Run("set to exactly maxSlots-1", func(t *testing.T) {
		st := NewSlotTable(8)
		st.SetTargetHighestSlotID(7)
		if got := st.GetTargetHighestSlotID(); got != 7 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 7", got)
		}
	})

	t.Run("set to maxSlots (clamped)", func(t *testing.T) {
		st := NewSlotTable(8)
		st.SetTargetHighestSlotID(8)
		if got := st.GetTargetHighestSlotID(); got != 7 {
			t.Errorf("GetTargetHighestSlotID() = %d, want 7", got)
		}
	})
}

// ============================================================================
// Concurrent Access Tests
// ============================================================================

func TestSlotTable_Concurrent(t *testing.T) {
	st := NewSlotTable(16)
	numGoroutines := 10
	opsPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(goroutineID int) {
			defer wg.Done()

			// Each goroutine uses a different slot to avoid contention
			// on the slot-level (but share the table-level mutex)
			slotID := uint32(goroutineID % 16)

			for i := 0; i < opsPerGoroutine; i++ {
				seqID := uint32(i + 1)

				// ValidateSequence atomically validates and marks in-use
				result, _, err := st.ValidateSequence(slotID, seqID)
				if err != nil {
					// Another goroutine may have advanced the slot;
					// errors are acceptable in concurrent access
					continue
				}

				if result == SeqNew {
					// Complete (slot already marked in-use by ValidateSequence)
					st.CompleteSlotRequest(slotID, seqID, true, []byte("reply"))
				}
			}
		}(g)
	}

	wg.Wait()

	// Verify no panics occurred and slot table is still consistent
	_ = st.MaxSlots()
	_ = st.GetHighestSlotID()
	_ = st.GetTargetHighestSlotID()
}

// TestSlotTable_ConcurrentMixedOps tests concurrent operations on the same
// slot from different goroutines to verify mutex correctness.
func TestSlotTable_ConcurrentMixedOps(t *testing.T) {
	st := NewSlotTable(4)

	var wg sync.WaitGroup
	wg.Add(3)

	// Goroutine 1: repeatedly set target
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			st.SetTargetHighestSlotID(uint32(i % 4))
		}
	}()

	// Goroutine 2: repeatedly get values
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = st.GetHighestSlotID()
			_ = st.GetTargetHighestSlotID()
			_ = st.MaxSlots()
		}
	}()

	// Goroutine 3: validate and complete on slot 0
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			result, _, err := st.ValidateSequence(0, uint32(i))
			if err != nil {
				continue
			}
			if result == SeqNew {
				// Slot already marked in-use by ValidateSequence
				st.CompleteSlotRequest(0, uint32(i), i%2 == 0, []byte("data"))
			}
		}
	}()

	wg.Wait()
}

// ============================================================================
// Full Workflow Tests
// ============================================================================

func TestSlotTable_FullWorkflow(t *testing.T) {
	st := NewSlotTable(4)

	// Step 1: First request on slot 0 (seqID=1)
	// ValidateSequence atomically marks slot in-use for SeqNew
	result, _, err := st.ValidateSequence(0, 1)
	if err != nil || result != SeqNew {
		t.Fatalf("step 1: result=%d, err=%v; want SeqNew, nil", result, err)
	}

	// Step 2: Complete with cached reply
	st.CompleteSlotRequest(0, 1, true, []byte("response-1"))

	// Step 3: Retry (seqID=1 again)
	result, slot, err := st.ValidateSequence(0, 1)
	if err != nil || result != SeqRetry {
		t.Fatalf("step 3: result=%d, err=%v; want SeqRetry, nil", result, err)
	}
	if string(slot.CachedReply) != "response-1" {
		t.Errorf("step 3: CachedReply = %q, want %q", slot.CachedReply, "response-1")
	}

	// Step 4: Next request (seqID=2)
	result, _, err = st.ValidateSequence(0, 2)
	if err != nil || result != SeqNew {
		t.Fatalf("step 4: result=%d, err=%v; want SeqNew, nil", result, err)
	}
	st.CompleteSlotRequest(0, 2, false, nil)

	// Step 5: Retry seqID=2 without cache -> RETRY_UNCACHED_REP
	_, _, err = st.ValidateSequence(0, 2)
	if err == nil {
		t.Fatal("step 5: expected error for uncached retry")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok || stateErr.Status != types.NFS4ERR_RETRY_UNCACHED_REP {
		t.Errorf("step 5: got status=%d, want NFS4ERR_RETRY_UNCACHED_REP", stateErr.Status)
	}

	// Step 6: Next request (seqID=3)
	result, _, err = st.ValidateSequence(0, 3)
	if err != nil || result != SeqNew {
		t.Fatalf("step 6: result=%d, err=%v; want SeqNew, nil", result, err)
	}
}

// TestSlotTable_MultipleSlots verifies that multiple slots operate independently.
func TestSlotTable_MultipleSlots(t *testing.T) {
	st := NewSlotTable(4)

	// Use slot 0 and slot 2 independently
	// Slot 0: seqID progression 1, 2, 3
	for seqID := uint32(1); seqID <= 3; seqID++ {
		result, _, err := st.ValidateSequence(0, seqID)
		if err != nil || result != SeqNew {
			t.Fatalf("slot 0 seqID=%d: result=%d, err=%v", seqID, result, err)
		}
		st.CompleteSlotRequest(0, seqID, true, []byte("slot0"))
	}

	// Slot 2: seqID progression 1, 2
	for seqID := uint32(1); seqID <= 2; seqID++ {
		result, _, err := st.ValidateSequence(2, seqID)
		if err != nil || result != SeqNew {
			t.Fatalf("slot 2 seqID=%d: result=%d, err=%v", seqID, result, err)
		}
		st.CompleteSlotRequest(2, seqID, true, []byte("slot2"))
	}

	// Verify slot 0 is at seqID=3 (next expected: 4)
	result, _, err := st.ValidateSequence(0, 4)
	if err != nil || result != SeqNew {
		t.Errorf("slot 0 seqID=4: result=%d, err=%v; want SeqNew", result, err)
	}

	// Verify slot 2 is at seqID=2 (next expected: 3)
	result2, _, err2 := st.ValidateSequence(2, 3)
	if err2 != nil || result2 != SeqNew {
		t.Errorf("slot 2 seqID=3: result=%d, err=%v; want SeqNew", result2, err2)
	}
}
