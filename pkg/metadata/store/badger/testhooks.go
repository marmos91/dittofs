package badger

// SetMaxTransactionRetriesForTest overrides the SSI conflict retry budget and
// returns a function that restores the previous value. It exists solely so
// cross-package tests (e.g. the block/engine rollup-convergence suite) can
// shrink the budget to deterministically drive WithTransaction into its
// retry-exhausted conflict path without spinning thousands of contending
// goroutines. Production code never calls it.
func SetMaxTransactionRetriesForTest(n int) func() {
	prev := maxTransactionRetries.Load()
	// Clamp to at least 1: a value <= 0 would make WithTransaction run zero
	// attempts and return nil without ever executing the closure (lastErr stays
	// nil), silently swallowing the work.
	if n < 1 {
		n = 1
	}
	maxTransactionRetries.Store(int32(n))
	return func() { maxTransactionRetries.Store(prev) }
}
