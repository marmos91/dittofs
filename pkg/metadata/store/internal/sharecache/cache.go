// Package sharecache caches decoded ShareOptions for the permission hot path,
// shared by every metadata backend.
package sharecache

import (
	"slices"

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

// Clone returns a caller-owned deep copy of opts: the struct is copied and
// every reference-bearing field (three string slices and the IdentityMapping
// pointee, itself holding *uint32/*uint32/*string) is cloned so neither the
// caller nor a concurrent reader can mutate the shared cache entry. A shallow
// *opts would alias those slices/pointers into the cache.
func Clone(opts *metadata.ShareOptions) *metadata.ShareOptions {
	if opts == nil {
		return nil
	}
	cp := *opts
	cp.AllowedClients = slices.Clone(opts.AllowedClients)
	cp.DeniedClients = slices.Clone(opts.DeniedClients)
	cp.AllowedAuthMethods = slices.Clone(opts.AllowedAuthMethods)
	cp.IdentityMapping = cloneIdentityMapping(opts.IdentityMapping)
	return &cp
}

// cloneIdentityMapping deep-copies the mapping and its pointer fields.
func cloneIdentityMapping(m *metadata.IdentityMapping) *metadata.IdentityMapping {
	if m == nil {
		return nil
	}
	cp := *m
	if m.AnonymousUID != nil {
		v := *m.AnonymousUID
		cp.AnonymousUID = &v
	}
	if m.AnonymousGID != nil {
		v := *m.AnonymousGID
		cp.AnonymousGID = &v
	}
	if m.AnonymousSID != nil {
		v := *m.AnonymousSID
		cp.AnonymousSID = &v
	}
	return &cp
}
