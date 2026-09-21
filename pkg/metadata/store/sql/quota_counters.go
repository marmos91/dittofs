package sql

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// quotaUsageColumns is the row shape of the durable counters, shared by the
// write and the rebuild so the two cannot describe different tables.
const quotaUsageColumns = "share_name, scope, identity_id, stripe, bytes, files"

// quotaStripes is how many rows one logical bucket is split across.
//
// An increment locks its row until the transaction commits, and that commit
// carries the WAL fsync. Files in a share commonly share a single owner uid, so
// one row per bucket would put every write to that share behind one lock held
// across an fsync: per-share write throughput would fall to one writer at a
// time however many clients were writing, because that bucket is the whole
// share's write path.
//
// ponytail: fixed width, so a bucket costs this many rows to read at open
// however little it is contended. Make it adaptive only once an open is
// measurably waiting on this read rather than on the per-share work around it.
const quotaStripes = 16

// quotaStripeRR spreads transactions across stripes. Process-wide rather than
// per-store: it only has to avoid collisions, and which store a transaction
// belongs to does not change what it collides with.
var quotaStripeRR atomic.Uint64

// PersistQuotaDelta folds one transaction's usage delta into the durable
// counters, from inside that same transaction. The counters therefore commit
// with the rows that moved them or not at all, which is what lets an open read
// them instead of re-aggregating the inodes table.
//
// The increment is written as SQL (bytes = quota_usage.bytes + EXCLUDED.bytes)
// rather than read into Go and written back. Postgres runs these transactions at
// READ COMMITTED, where a read-then-write pair loses the update when two writers
// touch a bucket concurrently: both read the same value and the second overwrites
// the first. The SQL-side increment re-evaluates against the row version it
// actually locked, so it is safe at that isolation level.
//
// Values are stored raw and signed. A bucket is not clamped here, because the
// clamp is a property of the total a reader sees, not of any one increment.
func (c *Core) PersistQuotaDelta(ctx context.Context, delta map[basestore.QuotaKey]metadata.UsageStat) error {
	if len(delta) == 0 {
		return nil
	}
	stmt := fmt.Sprintf(
		`INSERT INTO quota_usage (%s) VALUES (%s, %s, %s, %s, %s, %s)
		 ON CONFLICT (share_name, scope, identity_id, stripe) DO UPDATE
		 SET bytes = quota_usage.bytes + EXCLUDED.bytes,
		     files = quota_usage.files + EXCLUDED.files`,
		quotaUsageColumns,
		c.D.Placeholder(1), c.D.Placeholder(2), c.D.Placeholder(3),
		c.D.Placeholder(4), c.D.Placeholder(5), c.D.Placeholder(6),
	)

	// One stripe for the whole delta: a transaction charging a uid and a gid
	// locks two rows either way, and splitting them across stripes would only
	// widen the set it holds to commit.
	stripe := int(quotaStripeRR.Add(1) % quotaStripes)

	// Rows are locked in a fixed order. Map iteration is randomized, so two
	// transactions touching the same buckets could otherwise take them in
	// opposite orders and deadlock — retried rather than surfaced, but the retry
	// re-runs the whole file transaction, and this is the write path.
	for _, k := range sortedQuotaKeys(delta) {
		d := delta[k]
		if d.Bytes == 0 && d.Files == 0 {
			continue
		}
		if _, err := c.X.Exec(ctx, stmt, k.Share, int(k.Scope), int64(k.ID), stripe, d.Bytes, d.Files); err != nil {
			return c.D.MapError(err, "persist quota counters", "")
		}
	}
	return nil
}

// sortedQuotaKeys returns a delta's keys in a deterministic order, so every
// transaction acquires the same rows in the same sequence.
func sortedQuotaKeys(delta map[basestore.QuotaKey]metadata.UsageStat) []basestore.QuotaKey {
	keys := make([]basestore.QuotaKey, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Share != keys[j].Share {
			return keys[i].Share < keys[j].Share
		}
		if keys[i].Scope != keys[j].Scope {
			return keys[i].Scope < keys[j].Scope
		}
		return keys[i].ID < keys[j].ID
	})
	return keys
}

// ReadQuotaCounters loads the durable counters into usage buckets, summing each
// bucket's stripes. This is the open-time path: it reads one row per bucket per
// stripe, so it is bounded by how many distinct owners the store has rather than
// by how many files they own.
//
// Clamping and dropping emptied buckets is left to QuotaCache.Seed, which owns
// that rule — see PersistQuotaDelta for why the stored rows are unclamped.
func (c *Core) ReadQuotaCounters(ctx context.Context) (map[basestore.QuotaKey]*metadata.UsageStat, error) {
	rows, err := c.X.Query(ctx,
		`SELECT share_name, scope, identity_id, COALESCE(SUM(bytes), 0), COALESCE(SUM(files), 0)
		 FROM quota_usage GROUP BY share_name, scope, identity_id`)
	if err != nil {
		return nil, c.D.MapError(err, "read quota counters", "")
	}
	defer rows.Close()

	byIdentity := make(map[basestore.QuotaKey]*metadata.UsageStat)
	for rows.Next() {
		var (
			share string
			scope int
			id    int64
			stat  metadata.UsageStat
		)
		if err := rows.Scan(&share, &scope, &id, &stat.Bytes, &stat.Files); err != nil {
			return nil, c.D.MapError(err, "read quota counters", "")
		}
		byIdentity[basestore.QuotaKey{
			Share: share,
			Scope: metadata.QuotaScope(scope),
			ID:    uint32(id),
		}] = &stat
	}
	if err := rows.Err(); err != nil {
		return nil, c.D.MapError(err, "read quota counters", "")
	}
	return byIdentity, nil
}

// RebuildQuotaCounters re-derives every counter from the inode rows, replacing
// what is stored. This is the realign an operator invokes: counters maintained
// incrementally have no self-correction, so it is the only way back from a drift
// bug, and it is deliberately never run on its own.
//
// The re-aggregate adds to the row rather than replacing it. The DELETE locks
// only rows that exist when it runs, so a writer creating the first file for a
// new identity inserts a row the DELETE never saw and the aggregate's own
// snapshot may already count — replacing would drop that write, and inserting
// plainly would fail on the duplicate key. This is the one tool a drifted store
// has left, so it must not be the thing that breaks under load.
//
// ponytail: the DELETE takes a lock on every bucket and holds it across both
// aggregate scans, so writers across the whole database queue behind an operator
// realign. Rebuilding into a side table and swapping would avoid it; do that
// only once realign is something run against a busy store rather than a repair
// reached for when the numbers are already wrong.
func (c *Core) RebuildQuotaCounters(ctx context.Context) error {
	if _, err := c.X.Exec(ctx, "DELETE FROM quota_usage"); err != nil {
		return c.D.MapError(err, "rebuild quota counters", "")
	}
	// file_type is metadata.FileTypeRegular; nlink > 0 excludes an inode that is
	// unlinked but still held open, which keeps its row and none of the bytes.
	// The column names are fixed internal constants, never user input.
	seed := func(column string, scope metadata.QuotaScope) error {
		stmt := fmt.Sprintf(
			`INSERT INTO quota_usage (%s)
			 SELECT share_name, %d, %s, 0, COALESCE(SUM(size), 0), COUNT(*)
			 FROM inodes WHERE file_type = %d AND nlink > 0
			 GROUP BY share_name, %s
			 ON CONFLICT (share_name, scope, identity_id, stripe) DO UPDATE
			 SET bytes = quota_usage.bytes + EXCLUDED.bytes,
			     files = quota_usage.files + EXCLUDED.files`,
			quotaUsageColumns, int(scope), column,
			int(metadata.FileTypeRegular), column,
		)
		if _, err := c.X.Exec(ctx, stmt); err != nil {
			return c.D.MapError(err, "rebuild quota counters", "")
		}
		return nil
	}
	if err := seed("uid", metadata.QuotaScopeUser); err != nil {
		return err
	}
	return seed("gid", metadata.QuotaScopeGroup)
}
