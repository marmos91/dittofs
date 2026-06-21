package badger

// SetMaxTransactionRetriesForTest overrides the SSI conflict retry budget and
// returns a function that restores the previous value. It exists solely so
// cross-package tests (e.g. the block/engine rollup-convergence suite) can
// shrink the budget to deterministically drive WithTransaction into its
// retry-exhausted conflict path without spinning thousands of contending
// goroutines. Production code never calls it.
func SetMaxTransactionRetriesForTest(n int) func() {
	prev := maxTransactionRetries
	maxTransactionRetries = n
	return func() { maxTransactionRetries = prev }
}
