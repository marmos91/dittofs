// Package basestore holds the pieces every metadata store backend shares:
// per-identity usage accounting, the statfs assembly used by the SQL
// backends, the default filesystem capabilities, and file-handle minting.
//
// It deliberately holds no retry policy. The SQL family retries on
// busy/serialization errors through internal/txretry; the KV family retries
// on Badger's SSI conflicts inside its own loop. Those budgets must never be
// shared.
//
// Quota accounting: each backend keeps a QuotaCache of per-user/per-group byte
// and file counts (for statfs and quota enforcement) and accumulates changes
// made inside a transaction in a QuotaDelta, folding the delta into the cache
// exactly once after the transaction commits so a conflict-retry never
// double-counts.
//
// QuotaCache performs no locking of its own; callers guard it with their
// existing mutex.
package basestore

import "github.com/marmos91/dittofs/pkg/metadata"

// QuotaKey identifies a per-identity usage bucket: an owner id within a scope
// (user or group).
type QuotaKey struct {
	Scope metadata.QuotaScope
	ID    uint32
}

// QuotaCache holds per-identity usage split into user and group scopes. It does no
// locking; callers hold their own mutex across Get/Apply/Seed/Reset.
type QuotaCache struct {
	user  map[uint32]*metadata.UsageStat
	group map[uint32]*metadata.UsageStat
}

// NewQuotaCache returns an empty, ready-to-use QuotaCache.
func NewQuotaCache() *QuotaCache {
	return &QuotaCache{
		user:  make(map[uint32]*metadata.UsageStat),
		group: make(map[uint32]*metadata.UsageStat),
	}
}

// Reset empties the cache.
func (c *QuotaCache) Reset() {
	c.user = make(map[uint32]*metadata.UsageStat)
	c.group = make(map[uint32]*metadata.UsageStat)
}

// Seed replaces the cache contents with per-identity usage pre-aggregated from
// durable rows at startup. The maps are adopted as-is.
func (c *QuotaCache) Seed(user, group map[uint32]*metadata.UsageStat) {
	c.user = user
	c.group = group
}

// Get returns the usage for one identity. A missing key returns a zero
// UsageStat.
func (c *QuotaCache) Get(scope metadata.QuotaScope, id uint32) metadata.UsageStat {
	m := c.user
	if scope == metadata.QuotaScopeGroup {
		m = c.group
	}
	if u, ok := m[id]; ok {
		return *u
	}
	return metadata.UsageStat{}
}

// Apply folds a per-identity usage delta into the cache. Buckets that drop to
// zero or below are removed; a negative accumulation is clamped to zero so a
// quota enforcer never reads a too-permissive total.
func (c *QuotaCache) Apply(delta map[QuotaKey]metadata.UsageStat) {
	for k, d := range delta {
		m := c.user
		if k.Scope == metadata.QuotaScopeGroup {
			m = c.group
		}
		cur := m[k.ID]
		if cur == nil {
			cur = &metadata.UsageStat{}
			m[k.ID] = cur
		}
		cur.Bytes += d.Bytes
		cur.Files += d.Files
		if cur.Bytes < 0 {
			cur.Bytes = 0
		}
		if cur.Files < 0 {
			cur.Files = 0
		}
		if cur.Bytes == 0 && cur.Files == 0 {
			delete(m, k.ID)
		}
	}
}

// QuotaDelta accumulates per-identity usage changes made inside a single
// transaction, keyed by owner identity. It is folded into a QuotaCache exactly once
// after the transaction commits. The zero value is ready to use.
type QuotaDelta struct {
	m map[QuotaKey]metadata.UsageStat
}

// Add records a usage change for a file's owner identity across both the user
// and group scopes. bytes is the size delta; files is the inode delta (+1 on
// create, -1 on delete, 0 for an in-place resize). A no-op change is dropped.
func (d *QuotaDelta) Add(uid, gid uint32, bytes, files int64) {
	if bytes == 0 && files == 0 {
		return
	}
	if d.m == nil {
		d.m = make(map[QuotaKey]metadata.UsageStat)
	}
	u := d.m[QuotaKey{metadata.QuotaScopeUser, uid}]
	u.Bytes += bytes
	u.Files += files
	d.m[QuotaKey{metadata.QuotaScopeUser, uid}] = u
	g := d.m[QuotaKey{metadata.QuotaScopeGroup, gid}]
	g.Bytes += bytes
	g.Files += files
	d.m[QuotaKey{metadata.QuotaScopeGroup, gid}] = g
}

// AddKeyed merges a single pre-keyed usage change (used when folding the
// per-identity totals freed by a share delete). A no-op change is dropped.
func (d *QuotaDelta) AddKeyed(k QuotaKey, s metadata.UsageStat) {
	if s.Bytes == 0 && s.Files == 0 {
		return
	}
	if d.m == nil {
		d.m = make(map[QuotaKey]metadata.UsageStat)
	}
	cur := d.m[k]
	cur.Bytes += s.Bytes
	cur.Files += s.Files
	d.m[k] = cur
}

// Map returns the accumulated per-identity deltas for folding into a QuotaCache.
// It may be nil when nothing was accumulated.
func (d *QuotaDelta) Map() map[QuotaKey]metadata.UsageStat {
	return d.m
}
