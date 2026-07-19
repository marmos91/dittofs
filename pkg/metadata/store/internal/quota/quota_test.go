package quota

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestDeltaApplyAndGet exercises the accumulate → apply → read round-trip that
// every store shares: a Delta records owner changes across user and group
// scopes, the Cache folds them, and Get reports the result.
func TestDeltaApplyAndGet(t *testing.T) {
	c := NewCache()
	var d Delta

	// Create a 1000-byte file owned by uid 7 / gid 3.
	d.Add(7, 3, 1000, 1)
	c.Apply(d.Map())

	if got := c.Get(metadata.QuotaScopeUser, 7); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("user usage = %+v, want {1000 1}", got)
	}
	if got := c.Get(metadata.QuotaScopeGroup, 3); got.Bytes != 1000 || got.Files != 1 {
		t.Fatalf("group usage = %+v, want {1000 1}", got)
	}
	// Missing identity reads back zero.
	if got := c.Get(metadata.QuotaScopeUser, 99); got != (metadata.UsageStat{}) {
		t.Fatalf("missing usage = %+v, want zero", got)
	}
}

// TestApplyDeletesAtZero verifies the clamp-to-zero / delete-if-zero defensive
// logic: removing the file empties the bucket, and an over-decrement never
// leaves a negative total.
func TestApplyDeletesAtZero(t *testing.T) {
	c := NewCache()

	var create Delta
	create.Add(7, 3, 1000, 1)
	c.Apply(create.Map())

	// Delete the file: bucket reaches zero and is removed.
	var del Delta
	del.Add(7, 3, -1000, -1)
	c.Apply(del.Map())

	if got := c.Get(metadata.QuotaScopeUser, 7); got != (metadata.UsageStat{}) {
		t.Fatalf("usage after delete = %+v, want zero (bucket removed)", got)
	}

	// Over-decrement (accounting drift) clamps to zero rather than going
	// negative.
	c.Apply(map[Key]metadata.UsageStat{
		{metadata.QuotaScopeUser, 7}: {Bytes: -500, Files: -1},
	})
	if got := c.Get(metadata.QuotaScopeUser, 7); got.Bytes < 0 || got.Files < 0 {
		t.Fatalf("usage clamped negative = %+v", got)
	}
}
