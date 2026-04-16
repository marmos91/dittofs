package handlers

import (
	"sync"
	"testing"
	"time"
)

// TestBumpBootVerifier_ChangesValue verifies BumpBootVerifier replaces the
// current verifier with a fresh time-derived value.
func TestBumpBootVerifier_ChangesValue(t *testing.T) {
	before := bootVerifierBytes()
	// Tiny sleep so UnixNano advances across implementations (non-hermetic time).
	time.Sleep(time.Millisecond)
	BumpBootVerifier()
	after := bootVerifierBytes()
	if before == after {
		t.Fatalf("BumpBootVerifier did not change verifier: before=%x after=%x", before, after)
	}
}

// TestBumpBootVerifier_ConcurrentReadsAreConsistent verifies that concurrent
// bumps and reads are race-free and each read returns a valid 8-byte snapshot.
// Run with -race to validate the atomic pointer swap.
func TestBumpBootVerifier_ConcurrentReadsAreConsistent(t *testing.T) {
	const (
		readers    = 8
		bumpers    = 2
		iterations = 200
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader goroutines: repeatedly load and check the snapshot is an 8-byte value.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := bootVerifierBytes()
				// Trivial usage to prevent the compiler from eliding the load.
				_ = v[0]
			}
		}()
	}

	// Bumper goroutines: churn the verifier.
	for i := 0; i < bumpers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				BumpBootVerifier()
			}
		}()
	}

	// Let readers see many bumps, then signal shutdown.
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After all bumps, one more read must still return a non-zero value
	// (init() ran; no code path stores a zero verifier).
	v := bootVerifierBytes()
	var zero [8]byte
	if v == zero {
		t.Fatalf("verifier unexpectedly zero after concurrent bumps")
	}
}
