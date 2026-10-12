package kvstore

// SetKeyBudget forces a batch's key budget, so a test can run removals in
// batches of a few keys.
func (s *Store) SetKeyBudget(k int) { s.k = k }
