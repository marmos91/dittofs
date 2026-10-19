package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Check finds a durable open whose index entry is gone, which a new primary
// would never close, and an index entry whose open is gone.
func TestCheckDurableOpenIndex(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(0)
	op := entity.Open{ID: 7, File: file, Durability: entity.DurabilityPersistent}
	put := func() {
		if err := f.s.Opens().Put(ctx, f.share, op); err != nil {
			t.Fatal(err)
		}
	}
	put()
	if err := f.s.Check(ctx); err != nil {
		t.Fatalf("a consistent store: %v", err)
	}
	for name, key := range map[string][]byte{
		"index entry lost": durableOpenIndexKey(f.share, f.get(file).Shard, file, op.ID),
		"open lost":        openKey(f.share, file, op.ID),
	} {
		if err := f.s.kv.Update(ctx, func(tx kv.Txn) error { return tx.Delete(key) }); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Check(ctx); err == nil {
			t.Errorf("%s: Check found nothing", name)
		}
		put()
	}
}
