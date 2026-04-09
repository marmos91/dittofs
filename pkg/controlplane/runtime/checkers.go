package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/blockstoreprobe"
	"github.com/marmos91/dittofs/pkg/health"
)

// StatusCacheTTL is the TTL applied to per-entity cached health
// checkers. Five seconds is long enough that many dashboard tabs
// polling /status collapse onto one probe, short enough that an
// operator refreshing after fixing a broken store sees the new state
// within one refresh cycle.
const StatusCacheTTL = 5 * time.Second

// statusKey identifies a cached checker entry. The kind prefix keeps
// entity namespaces disjoint (e.g. a share and a metadata store that
// share a name).
type statusKey struct {
	kind string // "meta" | "adapter" | "share" | "block-local" | "block-remote"
	name string
}

// checkerCache lazily builds and memoises [health.CachedChecker]
// instances keyed by entity kind and name. The underlying probes
// resolve their entities at call time, so stop/start transitions are
// picked up on the next TTL window without explicit invalidation.
type checkerCache struct {
	ttl time.Duration

	mu       sync.Mutex
	checkers map[statusKey]*health.CachedChecker
}

func newCheckerCache(ttl time.Duration) *checkerCache {
	return &checkerCache{
		ttl:      ttl,
		checkers: make(map[statusKey]*health.CachedChecker),
	}
}

// Delete removes any cached checker for key. It is safe to call for
// keys that are not present. Runtime callers invoke this when the
// underlying entity is deleted (or renamed) so stale checkers do not
// leak for the lifetime of the process.
func (c *checkerCache) Delete(key statusKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.checkers, key)
}

// getOrBuild returns the cached checker for key, calling build to
// construct the inner checker only on the first miss. Taking a
// factory avoids allocating an unused CheckerFunc closure on every
// cache hit.
func (c *checkerCache) getOrBuild(key statusKey, build func() health.Checker) *health.CachedChecker {
	c.mu.Lock()
	defer c.mu.Unlock()

	if wrapped, ok := c.checkers[key]; ok {
		return wrapped
	}

	wrapped := health.NewCachedChecker(build(), c.ttl)
	c.checkers[key] = wrapped
	return wrapped
}

// unknownReport builds a [health.StatusUnknown] report with latency
// measured from start. Used by probes when an entity lookup fails.
func unknownReport(start time.Time, msg string) health.Report {
	end := time.Now()
	return health.Report{
		Status:    health.StatusUnknown,
		Message:   msg,
		CheckedAt: end.UTC(),
		LatencyMs: end.Sub(start).Milliseconds(),
	}
}

// unknownChecker returns a [health.Checker] that always reports
// [health.StatusUnknown] with the given message, or the context
// error when the caller's context is already canceled. Used as a
// defensive fallback when accessor methods are called on an
// uninitialized [Runtime].
func unknownChecker(msg string) health.Checker {
	return health.CheckerFunc(func(ctx context.Context) health.Report {
		reportMsg := msg
		if err := ctx.Err(); err != nil {
			reportMsg = err.Error()
		}
		return health.Report{
			Status:    health.StatusUnknown,
			Message:   reportMsg,
			CheckedAt: time.Now().UTC(),
		}
	})
}

// cachedChecker returns a TTL-cached [health.Checker] for the given
// key, built from probe on first miss. All four per-entity accessors
// share this helper so the nil-guard, cache lookup, and CheckerFunc
// wrapping live in exactly one place.
func (r *Runtime) cachedChecker(key statusKey, probe func(context.Context) health.Report) health.Checker {
	if r == nil || r.statusCheckers == nil {
		return unknownChecker("runtime not initialized")
	}
	return r.statusCheckers.getOrBuild(key, func() health.Checker {
		return health.CheckerFunc(probe)
	})
}

// InvalidateMetadataStoreChecker evicts any cached health checker
// for the named metadata store. Callers invoke this when the entity
// is deleted or renamed so the map does not grow unbounded.
func (r *Runtime) InvalidateMetadataStoreChecker(name string) {
	if r == nil || r.statusCheckers == nil {
		return
	}
	r.statusCheckers.Delete(statusKey{kind: "meta", name: name})
}

// InvalidateAdapterChecker evicts any cached health checker for the
// named adapter type.
func (r *Runtime) InvalidateAdapterChecker(adapterType string) {
	if r == nil || r.statusCheckers == nil {
		return
	}
	r.statusCheckers.Delete(statusKey{kind: "adapter", name: adapterType})
}

// InvalidateShareChecker evicts any cached health checker for the
// named share.
func (r *Runtime) InvalidateShareChecker(name string) {
	if r == nil || r.statusCheckers == nil {
		return
	}
	r.statusCheckers.Delete(statusKey{kind: "share", name: name})
}

// InvalidateBlockStoreChecker evicts any cached health checker for
// the named block store config.
func (r *Runtime) InvalidateBlockStoreChecker(kind models.BlockStoreKind, name string) {
	if r == nil || r.statusCheckers == nil {
		return
	}
	r.statusCheckers.Delete(statusKey{kind: "block-" + string(kind), name: name})
}

// MetadataStoreChecker returns a cached [health.Checker] for the
// named metadata store. The probe resolves the store lazily and
// reports [health.StatusUnknown] with a "not loaded" message when
// the store is not currently registered with the runtime.
func (r *Runtime) MetadataStoreChecker(name string) health.Checker {
	return r.cachedChecker(statusKey{kind: "meta", name: name}, func(ctx context.Context) health.Report {
		start := time.Now()
		metaStore, err := r.storesSvc.GetMetadataStore(name)
		if err != nil {
			return unknownReport(start, "metadata store "+name+" not loaded")
		}
		return metaStore.Healthcheck(ctx)
	})
}

// AdapterChecker returns a cached [health.Checker] for the named
// adapter. When no adapter of that type is running the probe reports
// [health.StatusUnknown] with a "not running" message. Resolution is
// lazy, so restarts are picked up after the TTL window closes.
func (r *Runtime) AdapterChecker(adapterType string) health.Checker {
	return r.cachedChecker(statusKey{kind: "adapter", name: adapterType}, func(ctx context.Context) health.Report {
		start := time.Now()
		adp := r.adaptersSvc.GetAdapter(adapterType)
		if adp == nil {
			return unknownReport(start, "adapter "+adapterType+" not running")
		}
		return adp.Healthcheck(ctx)
	})
}

// ShareChecker returns a cached [health.Checker] for the named share.
// The probe delegates to [Runtime.HealthcheckShare], which already
// performs the worst-of derivation across the share's metadata store
// and block store engine.
func (r *Runtime) ShareChecker(name string) health.Checker {
	return r.cachedChecker(statusKey{kind: "share", name: name}, func(ctx context.Context) health.Report {
		return r.HealthcheckShare(ctx, name)
	})
}

// BlockStoreChecker returns a cached [health.Checker] for the named
// block store config. The probe resolves the config through the
// control-plane store and runs [blockstoreprobe.Probe], so the
// /status route and the legacy /health route can't drift from each
// other. A missing config surfaces as [health.StatusUnknown] rather
// than [health.StatusUnhealthy]; the HTTP handler returns 404 for
// that case before consulting this checker.
func (r *Runtime) BlockStoreChecker(kind models.BlockStoreKind, name string) health.Checker {
	return r.cachedChecker(statusKey{kind: "block-" + string(kind), name: name}, func(ctx context.Context) health.Report {
		start := time.Now()
		if r.store == nil {
			return unknownReport(start, "control-plane store not configured")
		}
		bs, err := r.store.GetBlockStore(ctx, name, kind)
		if err != nil {
			if errors.Is(err, models.ErrStoreNotFound) {
				return unknownReport(start, "block store "+name+" not found")
			}
			// Log the raw error server-side but surface a generic
			// message in the Report so clients never see internal DB
			// error text.
			logger.Error("BlockStoreChecker: failed to load block store config",
				"name", name, "kind", string(kind), "error", err)
			return unknownReport(start, "block store lookup failed")
		}
		return blockstoreprobe.Probe(ctx, bs)
	})
}

// BlockStoreCheckerFor is the "config-in-hand" variant of
// [Runtime.BlockStoreChecker]: handlers that already hold a fetched
// [models.BlockStoreConfig] (Get, Update, Status after its 404 check,
// and the List populate loop) pass it in directly, avoiding a second
// round-trip to the control-plane store for every /status render.
//
// The returned checker is not memoised in the TTL cache because its
// closure captures the concrete config pointer; caching it would pin
// a stale snapshot. If the caller needs coalesced polling on a cold
// cache it should use [Runtime.BlockStoreChecker] instead.
func (r *Runtime) BlockStoreCheckerFor(bs *models.BlockStoreConfig) health.Checker {
	if r == nil {
		return unknownChecker("runtime not initialized")
	}
	if bs == nil {
		return unknownChecker("block store config is nil")
	}
	return health.CheckerFunc(func(ctx context.Context) health.Report {
		return blockstoreprobe.Probe(ctx, bs)
	})
}
