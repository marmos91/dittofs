package basestore

import (
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/quota"
)

// Base holds the usage accounting state shared byte-for-byte across all
// metadata store backends. The total logical bytes used by regular files, and
// per-identity (uid/gid) usage for quota enforcement. Each backend
// embeds Base and is responsible for seeding it at the startup. The seeding
// mechanism differs per backend (SQL SUM + GROUP BY, KV full scan, or none)
// and stays outside this type
type Base struct {
	// usedBytes tracks the total logical bytes used by regular files. Updated
	// atomically on every size-changing operation.
	usedBytes atomic.Int64

	// quota tracks per-identity usage (bytes + file count) for regular files,
	// keyed by owner uid / gid. Updated from each comitted transaction's deltas
	// Guarded by quotaMu.
	quotaMu sync.Mutex
	quota   *quota.Cache
}

// NewBaseStore returns a zero usage Base ready to be embedded. Backends still
// seed it from their own durable state at startup.
func NewBaseStore() *Base {
	return &Base{
		quota: quota.NewCache(),
	}
}

// GetUsedBytes returns the current total logical bytes used by regular files.
// This is an O(1) atomic read, safe for concurrent access without locks.
func (b *Base) GetUsedBytes() int64 {
	return b.usedBytes.Load()
}

// GetQuotaUsage returns per-identity usage for the given scope and id.
// O(1) map read under quotaMu. A missing key returns a zero UsageStat.
func (b *Base) GetQuotaUsage(scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	b.quotaMu.Lock()
	defer b.quotaMu.Unlock()
	return b.quota.Get(scope, id), nil
}

// ApplyQuotaDelta folds a per-identity usage delta into the in-memory usage
// cache. Called post-commit (matching usedBytes).
func (b *Base) ApplyQuotaDelta(delta map[quota.Key]metadata.UsageStat) {
	if len(delta) == 0 {
		return
	}
	b.quotaMu.Lock()
	defer b.quotaMu.Unlock()
	b.quota.Apply(delta)
}

// AddUsedBytes atomically adds delta (positive or negative) to the used-bytes
// counter. Called post-commit with a transaction's accumulated size delta.
func (b *Base) AddUsedBytes(delta int64) {
	if delta != 0 {
		b.usedBytes.Add(delta)
	}
}

// SetUsedBytes atomically overwrites the used-bytes counter. For backends
// that recompute the total from durable state (startup seeding, snapshot
// restore) instead of accumulating deltas.
func (b *Base) SetUsedBytes(n int64) {
	b.usedBytes.Store(n)
}

// ResetUsage clears both the used-bytes counter and the per-identity quota
// cache. Named to avoid colliding with each backend's own Reset(ctx) (part
// of metadata.Resetable) a same name method on Base would be shadowed
// entirely by the backend's Reset, not merged with it.
func (b *Base) ResetUsage() {
	b.usedBytes.Store(0)
	b.quotaMu.Lock()
	b.quota.Reset()
	b.quotaMu.Unlock()
}

// Seed replaces the quota cache with per-identity usage pre-aggregated from
// durable state. Does not touch usedBytes callers seeding both should also
// call SetUsedBytes.
func (b *Base) Seed(user, group map[uint32]*metadata.UsageStat) {
	b.quotaMu.Lock()
	defer b.quotaMu.Unlock()
	b.quota.Seed(user, group)
}
