package sql

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// quotaUsageColumns is the row shape of the durable counters, shared by the
// read and the rebuild so the two cannot describe different tables.
const quotaUsageColumns = "share_name, scope, identity_id, bytes, files"

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
		`INSERT INTO quota_usage (%s) VALUES (%s, %s, %s, %s, %s)
		 ON CONFLICT (share_name, scope, identity_id) DO UPDATE
		 SET bytes = quota_usage.bytes + EXCLUDED.bytes,
		     files = quota_usage.files + EXCLUDED.files`,
		quotaUsageColumns,
		c.D.Placeholder(1), c.D.Placeholder(2), c.D.Placeholder(3),
		c.D.Placeholder(4), c.D.Placeholder(5),
	)
	for k, d := range delta {
		if d.Bytes == 0 && d.Files == 0 {
			continue
		}
		if _, err := c.X.Exec(ctx, stmt, k.Share, int(k.Scope), int64(k.ID), d.Bytes, d.Files); err != nil {
			return c.D.MapError(err, "persist quota counters", "")
		}
	}
	return nil
}

// ReadQuotaCounters loads the durable counters into usage buckets. This is the
// open-time path: it reads one row per bucket, so it is bounded by how many
// distinct owners the store has rather than by how many files they own.
//
// Rows are returned as stored, raw and signed: clamping a bucket at zero is the
// cache's job (QuotaCache.Seed), not this reader's — see PersistQuotaDelta for
// why the stored rows are left unclamped.
func (c *Core) ReadQuotaCounters(ctx context.Context) (map[basestore.QuotaKey]*metadata.UsageStat, error) {
	rows, err := c.X.Query(ctx, "SELECT "+quotaUsageColumns+" FROM quota_usage")
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
		key := basestore.QuotaKey{
			Share: share,
			Scope: metadata.QuotaScope(scope),
			ID:    uint32(id),
		}
		byIdentity[key] = &stat
	}
	if err := rows.Err(); err != nil {
		return nil, c.D.MapError(err, "read quota counters", "")
	}
	return byIdentity, nil
}

// RebuildQuotaCounters re-derives every counter from the inode rows, replacing
// what is stored. This is the realign an operator invokes: counters maintained
// incrementally have no self-correction, so it is the only way back from a drift
// bug. It must run inside a caller's transaction.
//
// Unlike the KV backends this needs no lock. The delete and the re-aggregate run
// inside that transaction, so a concurrent writer either commits before it (and
// is aggregated) or after it (and increments the rebuilt row).
func (c *Core) RebuildQuotaCounters(ctx context.Context) error {
	if _, err := c.X.Exec(ctx, "DELETE FROM quota_usage"); err != nil {
		return c.D.MapError(err, "rebuild quota counters", "")
	}
	// nlink > 0 excludes an inode that is unlinked but still held open, which
	// keeps its row and none of the bytes. Both the scope and the owner column
	// are internal constants, never user input.
	seed := func(column string, scope metadata.QuotaScope) error {
		stmt := fmt.Sprintf(
			`INSERT INTO quota_usage (%s)
			 SELECT share_name, %d, %s, COALESCE(SUM(size), 0), COUNT(*)
			 FROM inodes WHERE file_type = %d AND nlink > 0
			 GROUP BY share_name, %s`,
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
