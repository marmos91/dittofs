// Package quota holds the shared per-identity usage accounting used by every
// metadata store backend. Each backend keeps a Cache of per-user/per-group
// byte and file counts (for statfs and quota enforcement) and accumulates
// changes made inside a transaction in a Delta, folding the Delta into the
// Cache exactly once after the transaction commits so a conflict-retry never
// double-counts.
//
// Cache performs no locking of its own; callers guard it with their existing
// mutex.
package quota

import "github.com/marmos91/dittofs/pkg/metadata"

// Key identifies a per-identity usage bucket: an owner id within a scope
// (user or group).
type Key struct {
	Scope metadata.QuotaScope
	ID    uint32
}

// Cache holds per-identity usage split into user and group scopes. It does no
// locking; callers hold their own mutex across Get/Apply/Seed/Reset.
type Cache struct {
	user  map[uint32]*metadata.UsageStat
	group map[uint32]*metadata.UsageStat
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{
		user:  make(map[uint32]*metadata.UsageStat),
		group: make(map[uint32]*metadata.UsageStat),
	}
}

// Reset empties the cache.
func (c *Cache) Reset() {
	c.user = make(map[uint32]*metadata.UsageStat)
	c.group = make(map[uint32]*metadata.UsageStat)
}

// Seed replaces the cache contents with per-identity usage pre-aggregated from
// durable rows at startup. The maps are adopted as-is.
func (c *Cache) Seed(user, group map[uint32]*metadata.UsageStat) {
	c.user = user
	c.group = group
}

// Get returns the usage for one identity. A missing key returns a zero
// UsageStat.
func (c *Cache) Get(scope metadata.QuotaScope, id uint32) metadata.UsageStat {
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
func (c *Cache) Apply(delta map[Key]metadata.UsageStat) {
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

// Delta accumulates per-identity usage changes made inside a single
// transaction, keyed by owner identity. It is folded into a Cache exactly once
// after the transaction commits. The zero value is ready to use.
type Delta struct {
	m map[Key]metadata.UsageStat
}

// Add records a usage change for a file's owner identity across both the user
// and group scopes. bytes is the size delta; files is the inode delta (+1 on
// create, -1 on delete, 0 for an in-place resize). A no-op change is dropped.
func (d *Delta) Add(uid, gid uint32, bytes, files int64) {
	if bytes == 0 && files == 0 {
		return
	}
	if d.m == nil {
		d.m = make(map[Key]metadata.UsageStat)
	}
	u := d.m[Key{metadata.QuotaScopeUser, uid}]
	u.Bytes += bytes
	u.Files += files
	d.m[Key{metadata.QuotaScopeUser, uid}] = u
	g := d.m[Key{metadata.QuotaScopeGroup, gid}]
	g.Bytes += bytes
	g.Files += files
	d.m[Key{metadata.QuotaScopeGroup, gid}] = g
}

// AddKeyed merges a single pre-keyed usage change (used when folding the
// per-identity totals freed by a share delete). A no-op change is dropped.
func (d *Delta) AddKeyed(k Key, s metadata.UsageStat) {
	if s.Bytes == 0 && s.Files == 0 {
		return
	}
	if d.m == nil {
		d.m = make(map[Key]metadata.UsageStat)
	}
	cur := d.m[k]
	cur.Bytes += s.Bytes
	cur.Files += s.Files
	d.m[k] = cur
}

// Map returns the accumulated per-identity deltas for folding into a Cache.
// It may be nil when nothing was accumulated.
func (d *Delta) Map() map[Key]metadata.UsageStat {
	return d.m
}
