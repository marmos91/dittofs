// Package basestore holds the pieces every metadata store backend shares:
// per-identity usage accounting, the statfs assembly used by the SQL
// backends, the default filesystem capabilities, and file-handle minting.
//
// It deliberately holds no retry policy. The SQL family retries on
// busy/serialization errors through internal/txretry; the KV family retries
// on Badger's SSI conflicts inside its own loop. Those budgets must never be
// shared.
//
// Quota accounting: each backend keeps a QuotaCache of per-share and
// per-share-per-user/group byte and file counts (for statfs and quota
// enforcement) and accumulates changes
// made inside a transaction in a QuotaDelta, folding the delta into the cache
// exactly once after the transaction commits so a conflict-retry never
// double-counts.
//
// QuotaCache performs no locking of its own; callers guard it with their
// existing mutex.
package basestore

import "github.com/marmos91/dittofs/pkg/metadata"

// QuotaKey identifies a per-identity usage bucket: an owner id within a scope
// (user or group), within one share. The share dimension is load-bearing: a
// single store instance backs every share that names the same metadata store
// config, so an unqualified (scope, id) bucket would fold co-located shares'
// bytes into each other's quota checks.
type QuotaKey struct {
	Share string
	Scope metadata.QuotaScope
	ID    uint32
}

// QuotaCache holds usage split two ways: per (share, scope, identity) for
// per-user/per-group quotas, and per share for the share-wide quota gate and
// statfs. It does no locking; callers hold their own mutex across
// Get/Share/Apply/Seed/Reset.
type QuotaCache struct {
	byIdentity map[QuotaKey]*metadata.UsageStat
	byShare    map[string]*metadata.UsageStat
	// captured accumulates every delta applied while a rebuild is in flight.
	// A rebuild reads the durable rows with no lock held, so a transaction that
	// commits between its scan and its Seed would be scanned out and then have
	// its applied delta overwritten. Seed folds this back in. nil when no
	// rebuild is in flight.
	captured map[QuotaKey]metadata.UsageStat
}

// NewQuotaCache returns an empty, ready-to-use QuotaCache.
func NewQuotaCache() *QuotaCache {
	return &QuotaCache{
		byIdentity: make(map[QuotaKey]*metadata.UsageStat),
		byShare:    make(map[string]*metadata.UsageStat),
	}
}

// Reset empties the cache.
func (c *QuotaCache) Reset() {
	c.byIdentity = make(map[QuotaKey]*metadata.UsageStat)
	c.byShare = make(map[string]*metadata.UsageStat)
	c.captured = nil
}

// BeginRebuild marks the start of a rebuild: until the next Seed, every
// applied delta is also recorded, so a commit that lands while the rebuild is
// scanning survives the Seed that replaces the buckets.
//
// Startup rebuilds do not need it — nothing is committing yet — and calling
// Seed without it behaves exactly as before.
func (c *QuotaCache) BeginRebuild() {
	c.captured = make(map[QuotaKey]metadata.UsageStat)
}

// Seed replaces the cache contents with usage pre-aggregated from durable rows
// at startup. byShare may be nil, in which case it is derived from the
// user-scope entries of byIdentity — every regular file has exactly one owner
// uid, so those buckets already partition the share's bytes and inodes.
func (c *QuotaCache) Seed(byIdentity map[QuotaKey]*metadata.UsageStat, byShare map[string]*metadata.UsageStat) {
	if byIdentity == nil {
		byIdentity = make(map[QuotaKey]*metadata.UsageStat)
	}
	if byShare == nil {
		byShare = make(map[string]*metadata.UsageStat)
		for k, u := range byIdentity {
			if k.Scope != metadata.QuotaScopeUser {
				continue
			}
			cur := byShare[k.Share]
			if cur == nil {
				cur = &metadata.UsageStat{}
				byShare[k.Share] = cur
			}
			cur.Bytes += u.Bytes
			cur.Files += u.Files
		}
	}
	c.byIdentity = byIdentity
	c.byShare = byShare

	// Fold back anything that committed while the rebuild was scanning; its
	// bytes are in the cache's buckets but not in the scan that just replaced
	// them.
	if c.captured != nil {
		replay := c.captured
		c.captured = nil
		c.Apply(replay)
	}
}

// Get returns the usage for one identity within one share. A missing key
// returns a zero UsageStat.
func (c *QuotaCache) Get(share string, scope metadata.QuotaScope, id uint32) metadata.UsageStat {
	if u, ok := c.byIdentity[QuotaKey{Share: share, Scope: scope, ID: id}]; ok {
		return *u
	}
	return metadata.UsageStat{}
}

// Share returns the total usage of one share's regular files. A share with no
// files (or an unknown one) returns a zero UsageStat.
func (c *QuotaCache) Share(share string) metadata.UsageStat {
	if u, ok := c.byShare[share]; ok {
		return *u
	}
	return metadata.UsageStat{}
}

// DropShare forgets every bucket belonging to a share. Called when the share is
// deleted, so its usage cannot outlive it.
func (c *QuotaCache) DropShare(share string) {
	for k := range c.byIdentity {
		if k.Share == share {
			delete(c.byIdentity, k)
		}
	}
	delete(c.byShare, share)
}

// Apply folds a usage delta into the cache. Buckets that drop to zero or below
// are removed; a negative accumulation is clamped to zero so a quota enforcer
// never reads a too-permissive total.
//
// The per-share totals are moved by the user-scope entries only: a file
// contributes one user-scope entry and one group-scope entry for the same
// bytes, so counting both would double the share total.
func (c *QuotaCache) Apply(delta map[QuotaKey]metadata.UsageStat) {
	// Record the delta for a rebuild in flight, so replacing the buckets does
	// not drop a commit the rebuild's scan happened to miss.
	if c.captured != nil {
		for k, d := range delta {
			u := c.captured[k]
			u.Bytes += d.Bytes
			u.Files += d.Files
			c.captured[k] = u
		}
	}

	// Per-share movements are summed across the share's owners before being
	// applied, so the result does not depend on map iteration order: applying
	// them one owner at a time lets the clamp below fire on an intermediate
	// value when two owners in one share move in opposite directions.
	perShare := make(map[string]metadata.UsageStat)
	for k, d := range delta {
		applyStat(c.byIdentity, k, d)
		if k.Scope == metadata.QuotaScopeUser {
			cur := perShare[k.Share]
			cur.Bytes += d.Bytes
			cur.Files += d.Files
			perShare[k.Share] = cur
		}
	}
	for share, d := range perShare {
		applyStat(c.byShare, share, d)
	}
}

// applyStat folds one delta into a keyed usage map, clamping to zero and
// removing emptied buckets.
func applyStat[K comparable](m map[K]*metadata.UsageStat, key K, d metadata.UsageStat) {
	cur := m[key]
	if cur == nil {
		cur = &metadata.UsageStat{}
		m[key] = cur
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
		delete(m, key)
	}
}

// QuotaDelta accumulates per-identity usage changes made inside a single
// transaction, keyed by owner identity. It is folded into a QuotaCache exactly once
// after the transaction commits. The zero value is ready to use.
type QuotaDelta struct {
	m map[QuotaKey]metadata.UsageStat
}

// Add records a usage change for a file's owner identity within its share,
// across both the user and group scopes. bytes is the size delta; files is the
// inode delta (+1 on create, -1 on delete, 0 for an in-place resize). A no-op
// change is dropped.
func (d *QuotaDelta) Add(share string, uid, gid uint32, bytes, files int64) {
	if bytes == 0 && files == 0 {
		return
	}
	if d.m == nil {
		d.m = make(map[QuotaKey]metadata.UsageStat)
	}
	uk := QuotaKey{Share: share, Scope: metadata.QuotaScopeUser, ID: uid}
	u := d.m[uk]
	u.Bytes += bytes
	u.Files += files
	d.m[uk] = u
	gk := QuotaKey{Share: share, Scope: metadata.QuotaScopeGroup, ID: gid}
	g := d.m[gk]
	g.Bytes += bytes
	g.Files += files
	d.m[gk] = g
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

// Charged reports whether an inode contributes to the usage counters. Only
// regular files hold logical bytes, and only while at least one directory
// entry still names them.
//
// The link count is the deciding term because unlinking the last name does not
// remove the inode: the row survives with nlink=0 so fstat(2) on a descriptor
// that is still open keeps reporting the file. Keying usage off the row's
// existence instead would charge the share for that inode forever, since
// nothing ever deletes it.
func Charged(fileType metadata.FileType, nlink uint32) bool {
	return fileType == metadata.FileTypeRegular && nlink > 0
}
