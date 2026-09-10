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
