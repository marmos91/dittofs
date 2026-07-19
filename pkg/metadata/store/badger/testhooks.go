package badger

// SetMaxTransactionRetriesForTest overrides the SSI conflict retry budget and
// returns a function that restores the previous value. It exists solely so
// cross-package tests (e.g. the block/engine rollup-convergence suite) can
// shrink the budget to deterministically drive WithTransaction into its
// retry-exhausted conflict path without spinning thousands of contending
// goroutines. Production code never calls it.
// InlineSyncCountForTest returns the number of explicit db.Sync() calls made on
// the durable write path (WithTransaction in relaxed mode).
// It excludes the background bounded-lag syncer, so a test can assert that a
// durable commit fsynced inline and a relaxed commit did not. Production code
// never calls it.
func (s *BadgerMetadataStore) InlineSyncCountForTest() int64 {
	return s.inlineSyncs.Load()
}

// TransactionConflictsForTest returns the number of SSI ErrConflict aborts the
// withTransaction retry loop has observed so far. It is a disk-speed-independent
// contention fingerprint: a workload of transactions touching disjoint keys
// leaves it at zero, so a test can assert that concurrent operations did not
// serialize on a shared hot key. Production code never calls it.
func (s *BadgerMetadataStore) TransactionConflictsForTest() int64 {
	return s.txnConflicts.Load()
}

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
