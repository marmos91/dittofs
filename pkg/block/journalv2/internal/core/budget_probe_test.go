package core

// Reclaimers is a test probe for how many journals are registered to be
// asked for an idle descriptor: the gauge of the registry Shutdown removes a
// shut journal from. A journal left registered would pin its whole cache and
// be asked on every Reclaim forever. Nothing in the package's own surface
// needs the count, so it lives here rather than on DescriptorBudget.
func Reclaimers(b *DescriptorBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reclaimers)
}
