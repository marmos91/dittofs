package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// checkUsage asserts RFC 16 §4.4 over the fixture's share: the folded
// totals plus the unfolded deltas equal, for the share, every principal and
// every project, the sum of the files' Charged and their count.
func (f *cfix) checkUsage() {
	f.t.Helper()
	if err := f.s.Check(context.Background()); err != nil {
		f.t.Error(err)
	}
}

// RFC 16 §4.4: the fold moves deltas into the shard's totals in batches
// within the key budget, and usage reads the same before, between and after.
func TestFoldUsage(t *testing.T) {
	var h *holdKV
	f := newContentFix(t, func(db kv.KV) kv.KV { h = &holdKV{KV: db}; return h })
	ctx := context.Background()
	for i := range 12 {
		f.exist(f.file(0), write(uint64(i+1), 0, int64(i+1)*mib))
	}
	f.checkUsage()
	f.s.SetKeyBudget(2 * (1 + foldPartyKeys)) // two deltas a batch
	c := f.s.Capacity()
	batches := 0
	for more := true; more; batches++ {
		var err error
		if more, err = c.FoldUsage(ctx, f.share, 0); err != nil {
			t.Fatal(err)
		}
		f.checkUsage()
	}
	if batches < 10 {
		t.Errorf("%d batches for 25 deltas at two a batch", batches)
	}
	if u, err := c.Usage(ctx, f.share); err != nil || u.Bytes != 78*mib || u.Files != 13 {
		t.Errorf("share usage %+v %v, want 78 MiB in 13 files", u, err)
	}

	// A delta written while a fold runs is folded by a later batch, once.
	f.s.SetKeyBudget(1000)
	late := f.file(0)
	h.mu.Lock()
	h.hook = func() { f.exist(late, write(1, 0, mib)) }
	h.mu.Unlock()
	if _, err := c.FoldUsage(ctx, f.share, 0); err != nil {
		t.Fatal(err)
	}
	f.checkUsage()
	if _, err := c.FoldUsage(ctx, f.share, 0); err != nil {
		t.Fatal(err)
	}
	f.checkUsage()
	_ = f.s.kv.View(ctx, func(r kv.Reader) error {
		for _, err := range r.Scan(usageDeltasPrefix(f.share, 0), nil) {
			if err == nil {
				t.Error("a delta left after folding everything")
			}
		}
		return nil
	})
}

// A delta round-trips with every party it names, and one that cancels out
// is not written.
func TestUsageDelta(t *testing.T) {
	alice := entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{1}}
	bob := entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{2}}
	staff := entity.Principal{Kind: entity.PrincipalGroup, ID: entity.PrincipalID{3}}
	old := entity.File{Owner: alice, Group: staff, Project: 7, Charged: 100}
	moved := old
	moved.Owner = bob
	d := usageChange(&old, &moved)
	if d.Share != (entity.Usage{}) || d.Principals[alice] != (entity.Usage{Bytes: -100, Files: -1}) ||
		d.Principals[bob] != (entity.Usage{Bytes: 100, Files: 1}) || d.Principals[staff] != (entity.Usage{}) {
		t.Errorf("chown delta %+v", d)
	}
	back, err := decodeUsageDelta(encodeUsageDelta(d))
	if err != nil || back.Principals[alice] != d.Principals[alice] || back.Principals[bob] != d.Principals[bob] || len(back.Principals) != 2 {
		t.Errorf("round trip %+v %v", back, err)
	}
	if same := usageChange(&old, &old); !same.empty() {
		t.Errorf("an unchanged file wrote %+v", same)
	}
}
