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
