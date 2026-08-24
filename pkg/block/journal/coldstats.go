package journal

// ColdExtents reports the ranges this store knows about whose bytes are
// durable on the remote store but no longer resident locally. Reading one
// requires the remote, so a store with no cold extent can serve every byte it
// holds with the remote unreachable.
//
// A cold range is distinct from an absent one: an absent range is a true hole
// and reads as zeros by definition, while a cold range is served by fetching.
// Only cold ranges are counted, so a sparse file contributes nothing.
//
// The tally is bytes and ranges, not blocks. A journal interval is a byte
// range, split by partial overwrite and clamped by hydrate, so it does not
// stand in one-to-one for a manifest chunk row. Counting chunk rows instead
// would mean a metadata walk; the byte figure answers "how much would break
// offline" directly and costs an in-memory scan.
//
// The scan holds each shard's lock only for that shard's slice of the index,
// so it never blocks the whole store at once. It is O(live intervals), which
// is why callers should treat it as a periodic gauge rather than something to
// call per request.
func (s *Store) ColdExtents() (bytes int64, extents int64) {
	if s.closed.Load() {
		return 0, 0
	}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, fi := range sh.index {
			for _, iv := range fi.ivs {
				if !iv.cold {
					continue
				}
				bytes += iv.length
				extents++
			}
		}
		sh.mu.Unlock()
	}
	return bytes, extents
}
