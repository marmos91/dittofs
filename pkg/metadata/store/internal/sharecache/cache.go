// Package sharecache caches decoded ShareOptions for the permission hot path,
// shared by every metadata backend.
package sharecache

import (
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/gencache"
)

// Cache holds decoded ShareOptions keyed by share name, so the permission funnel
// every read, write, create and setattr traverses (checkFilePermissionsFile →
// GetShareOptions) skips the backend read and the options decode on every op.
// Server pprof of a warm random-read run showed Badger's GetShareOptions →
// decodeShareData at 17.4% of server CPU with its read transaction the top mutex
// contender at 14.5%; the SQL backends pay a query per op for the same
// near-static record.
//
// Shares are FEW and rarely written, so this stays unbounded (Cap 0) — the cap
// is a deliberate choice here, not an omission. A stale entry is a WRONG
// permission decision, so every share-record write site must Invalidate; the
// generation guard that makes that safe lives in gencache.
type Cache = gencache.Cache[*metadata.ShareOptions]

// Clone returns a caller-owned copy of opts, so neither the caller nor a
// concurrent reader can mutate the shared cache entry.
//
// decision: every ShareOptions field is a scalar today, so a struct copy is a
// full copy and this reads as if it does nothing. It is kept as the single
// place the cache hands out ownership. Add a reference-bearing field to
// ShareOptions and this must deepen with it: a shallow copy would alias that
// field into the cache, and the permission answer served to every later caller
// of the share would follow whatever one caller wrote.
func Clone(opts *metadata.ShareOptions) *metadata.ShareOptions {
	if opts == nil {
		return nil
	}
	cp := *opts
	return &cp
}
