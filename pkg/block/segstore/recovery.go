package segstore

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Crash recovery (tail-scan, CRC validation, torn-write truncation, Version-LSN
// recompute, orphan sweep) is not yet implemented. Until it lands, Open refuses
// to reopen a directory that already holds segments rather than silently
// starting a fresh cache over stale data.
//
// The eventual flow: per shard locate the one unsealed (active) segment, tail-
// scan it from offset 0 validating each record's header then payload CRC, and
// truncate + fsync at the first invalid or torn record; replay the valid
// records into a fresh interval index and .idx; recompute the global Version
// LSN as max(observed)+1. Sealed segments are trusted via their header's sealed
// bit ("header is truth") without a full re-scan. An age-gated orphan sweep
// deletes any on-disk segment unreferenced by the rebuilt bookkeeping.

// scanSegmentIDs returns the IDs of every well-formed <id>.seg file in dir.
func scanSegmentIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("segstore: readdir %q: %w", dir, err)
	}
	var ids []uint64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segSuffix) {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), segSuffix)
		id, err := strconv.ParseUint(stem, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}
