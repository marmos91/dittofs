package basestore

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestDeltaApplyAndGet exercises the accumulate → apply → read round-trip that
// every store shares: a QuotaDelta records owner changes across user and group
// scopes, the QuotaCache folds them, and Get reports the result.
func TestDeltaApplyAndGet(t *testing.T) {
	c := NewQuotaCache()
	var d QuotaDelta

	// Create a 1000-byte file owned by uid 7 / gid 3.
	d.Add("/s", 7, 3, 1000, 1)
	c.Apply(d.Map())

	if got := c.Get("/s", metadata.QuotaScopeUser, 7); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("user usage = %+v, want {1000 1}", got)
	}
	if got := c.Get("/s", metadata.QuotaScopeGroup, 3); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("group usage = %+v, want {1000 1}", got)
	}
	// The share total tracks the user-scope entries only: counting the group
	// scope too would double every file.
	if got := c.Share("/s"); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("share usage = %+v, want {1000 1}", got)
	}
	// A different share sharing the store sees none of it.
	if got := c.Share("/other"); got != (metadata.UsageStat{}) {
		t.Fatalf("other share usage = %+v, want zero", got)
	}
	// Missing identity reads back zero.
	if got := c.Get("/s", metadata.QuotaScopeUser, 99); got != (metadata.UsageStat{}) {
		t.Fatalf("missing usage = %+v, want zero", got)
	}
}

// TestApplyDeletesAtZero verifies the clamp-to-zero / delete-if-zero defensive
// logic: removing the file empties the bucket, and an over-decrement never
// leaves a negative total.
func TestApplyDeletesAtZero(t *testing.T) {
	c := NewQuotaCache()

	var create QuotaDelta
	create.Add("/s", 7, 3, 1000, 1)
	c.Apply(create.Map())

	// Delete the file: bucket reaches zero and is removed.
	var del QuotaDelta
	del.Add("/s", 7, 3, -1000, -1)
	c.Apply(del.Map())

	if got := c.Get("/s", metadata.QuotaScopeUser, 7); got != (metadata.UsageStat{}) {
		t.Fatalf("usage after delete = %+v, want zero (bucket removed)", got)
	}
	if got := c.Share("/s"); got != (metadata.UsageStat{}) {
		t.Fatalf("share usage after delete = %+v, want zero (bucket removed)", got)
	}

	// Over-decrement (accounting drift) clamps to zero rather than going
	// negative.
	c.Apply(map[QuotaKey]metadata.UsageStat{
		{Share: "/s", Scope: metadata.QuotaScopeUser, ID: 7}: {Bytes: -500, Files: -1},
	})
	if got := c.Get("/s", metadata.QuotaScopeUser, 7); got.Bytes < 0 || got.Files < 0 {
		t.Fatalf("usage clamped negative = %+v", got)
	}
}

// TestApplyPerShareIsOrderIndependent pins that a share's total is summed
// across its owners before the clamp runs. Applying owner-by-owner would let a
// negative intermediate clamp to zero and lose the co-owner's bytes, and map
// iteration order decides whether that happens.
func TestApplyPerShareIsOrderIndependent(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := NewQuotaCache()
		var seed QuotaDelta
		seed.Add("/s", 7, 3, 100, 1)
		c.Apply(seed.Map())

		// uid 7 drops more than the share holds while uid 8 adds: the share
		// total must land on 100 - 150 + 100 = 50, whichever owner is folded
		// in first.
		var d QuotaDelta
		d.Add("/s", 7, 3, -150, -1)
		d.Add("/s", 8, 3, 100, 1)
		c.Apply(d.Map())

		if got := c.Share("/s").Bytes; got != 50 {
			t.Fatalf("share total = %d, want 50 (iteration %d)", got, i)
		}
	}
}

// TestRebuildKeepsConcurrentCommit pins that a rebuild does not discard a
// transaction that commits while it is scanning.
//
// A rebuild reads the durable rows with no lock held and then replaces the
// cache wholesale. A commit landing in that window is missing from the scan and
// has already been applied to the buckets the Seed is about to overwrite, so
// without the capture its bytes vanish until the next mutation touches that
// owner — on an endpoint whose whole purpose is to make the counter trustworthy.
func TestRebuildKeepsConcurrentCommit(t *testing.T) {
	c := NewQuotaCache()

	// A share already holding one 1000-byte file owned by uid 7 / gid 3.
	var seeded QuotaDelta
	seeded.Add("/s", 7, 3, 1000, 1)
	c.Apply(seeded.Map())

	// Seed takes ownership of the map it is handed and mutates it when it
	// replays, so every rebuild builds its own — exactly as the backends do.
	scan := func() map[QuotaKey]*metadata.UsageStat {
		return map[QuotaKey]*metadata.UsageStat{
			{Share: "/s", Scope: metadata.QuotaScopeUser, ID: 7}:  {Bytes: 1000, Files: 1},
			{Share: "/s", Scope: metadata.QuotaScopeGroup, ID: 3}: {Bytes: 1000, Files: 1},
		}
	}

	// The rebuild starts and reads the rows: it sees only that one file.
	c.BeginRebuild()
	scanned := scan()

	// A second file commits before the scan's result is installed.
	var mid QuotaDelta
	mid.Add("/s", 7, 3, 500, 1)
	c.Apply(mid.Map())

	c.Seed(scanned, nil)

	if got := c.Share("/s"); got.Bytes != 1500 || got.Files != 2 {
		t.Fatalf("share usage after rebuild = %+v, want {1500 2} — the commit that landed mid-scan was dropped", got)
	}
	if got := c.Get("/s", metadata.QuotaScopeUser, 7); got.Bytes != 1500 || got.Files != 2 {
		t.Fatalf("user usage after rebuild = %+v, want {1500 2}", got)
	}

	// The capture ends with the Seed: a later rebuild must not replay it again.
	c.BeginRebuild()
	c.Seed(scan(), nil)
	if got := c.Share("/s"); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("share usage after a second rebuild = %+v, want {1000 1} — the capture replayed twice", got)
	}
}

// TestDriftReportsShareTotal pins that the comparison covers the share's own
// total and not only the per-owner buckets.
//
// Apply clamps each identity bucket on its own but moves the share total by the
// raw per-share sum, so a delta that drives one owner negative leaves the two
// permanently apart. The share total is what GetUsedBytesForShare, statfs and
// the share-wide quota gate read, so a report that skipped it would tell an
// operator the counters agree while the number they are looking at is wrong —
// and wrong downward, which lets writes past the share quota.
func TestDriftReportsShareTotal(t *testing.T) {
	c := NewQuotaCache()

	// A chown away from an owner whose bucket is already empty: the -500 clamps
	// to zero on that bucket but lands raw on the share total.
	var d QuotaDelta
	d.Add("/s", 1000, 1000, -500, -1)
	d.Add("/s", 2000, 2000, 500, 1)
	c.Apply(d.Map())

	if got := c.Get("/s", metadata.QuotaScopeUser, 2000); got.Bytes != 500 {
		t.Fatalf("user 2000 usage = %+v, want 500 bytes", got)
	}
	if got := c.Share("/s").Bytes; got != 0 {
		t.Fatalf("share total = %d, want 0 — the setup must reproduce the divergence", got)
	}

	// The rows say the share holds one 500-byte file owned by uid/gid 2000.
	derived := map[QuotaKey]*metadata.UsageStat{
		{Share: "/s", Scope: metadata.QuotaScopeUser, ID: 2000}:  {Bytes: 500, Files: 1},
		{Share: "/s", Scope: metadata.QuotaScopeGroup, ID: 2000}: {Bytes: 500, Files: 1},
	}

	drift := c.Drift(derived)
	if len(drift) != 1 {
		t.Fatalf("drift = %+v, want exactly the share row — the per-owner buckets agree", drift)
	}
	got := drift[0]
	if got.Scope != QuotaDriftScopeShare || got.Share != "/s" {
		t.Fatalf("drift row = %+v, want the share total of /s", got)
	}
	if got.Counter.Bytes != 0 || got.Derived.Bytes != 500 {
		t.Fatalf("drift row = %+v, want counter 0 against derived 500 — both numbers must travel", got)
	}
}

// TestDriftIsEmptyWhenCountersAgree pins the other direction: a cache built
// from the same rows the scan reports must produce no rows at all, share total
// included. Without it the share row added above would fire on every healthy
// store.
func TestDriftIsEmptyWhenCountersAgree(t *testing.T) {
	c := NewQuotaCache()
	var d QuotaDelta
	d.Add("/s", 7, 3, 1000, 1)
	d.Add("/s", 8, 3, 2000, 1)
	c.Apply(d.Map())

	derived := map[QuotaKey]*metadata.UsageStat{
		{Share: "/s", Scope: metadata.QuotaScopeUser, ID: 7}:  {Bytes: 1000, Files: 1},
		{Share: "/s", Scope: metadata.QuotaScopeUser, ID: 8}:  {Bytes: 2000, Files: 1},
		{Share: "/s", Scope: metadata.QuotaScopeGroup, ID: 3}: {Bytes: 3000, Files: 2},
	}
	if drift := c.Drift(derived); len(drift) != 0 {
		t.Fatalf("drift = %+v, want none", drift)
	}
}
