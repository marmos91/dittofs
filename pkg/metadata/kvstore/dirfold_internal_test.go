package kvstore

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// RFC 7 §4.1's underflow rule: a fold that takes Subdirs below zero stores
// it as the deltas sum to, asks for a recount and wedges nothing, the link
// count reading 2 meanwhile; the recount restores the count from the
// entries, deltas still unfolded included.
func TestSubdirsUnderflowRecounts(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	ns := f.s.Namespace()
	dir := entity.NewFileID()
	d := entity.File{ID: dir, Share: f.share, Type: entity.Directory, Mode: 0o755, Number: 98, Nlink: 1, Parent: dir, Version: entity.VersionOf(1)}
	sub := entity.NewFileID()
	if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		if err := putFile(tx, entity.JournalVersion{}, d); err != nil {
			return err
		}
		// One real subdirectory, and a corrupt delta taking away three.
		if err := tx.Set(entryKey(f.share, dir, 1, []byte("s")), encodeEntry(entity.Entry{Parent: dir, Name: []byte("s"), Child: sub, Type: entity.Directory})); err != nil {
			return err
		}
		var u [32]byte
		u[0] = 1
		v := entity.VersionOf(2)
		if err := tx.Set(deltaFloorKey(f.share, dir, v, u), encodeEmpty()); err != nil {
			return err
		}
		return tx.Set(deltaKey(f.share, dir, u), encodeDelta(delta{Modify: time.Unix(1, 0), Change: time.Unix(1, 0), Version: v, Subdirs: -3}))
	}); err != nil {
		t.Fatal(err)
	}
	more, recount, err := ns.FoldDirectory(ctx, f.share, dir)
	if err != nil || more || !recount {
		t.Fatalf("fold of an underflowing directory: more %v, recount %v, %v", more, recount, err)
	}
	if got := f.get(dir); got.Subdirs != -3 || got.LinkCount() != 2 {
		t.Errorf("after the fold: Subdirs %d stored, link count %d", got.Subdirs, got.LinkCount())
	}
	if err := ns.RecountSubdirs(ctx, f.share, dir); err != nil {
		t.Fatal(err)
	}
	if got := f.get(dir); got.Subdirs != 1 || got.LinkCount() != 3 {
		t.Errorf("after the recount: Subdirs %d, link count %d", got.Subdirs, got.LinkCount())
	}
}

// A fold writes within the key budget whatever K is: each delta costs its
// key and its version-index entry, and the record its own key and the two
// version-index keys it moves between.
func TestFoldDirectoryWithinBudget(t *testing.T) {
	for _, k := range []int{7, 8, 9} {
		var w *writesKV
		f := newContentFix(t, func(db kv.KV) kv.KV { w = &writesKV{KV: db}; return w })
		ctx := context.Background()
		dir := entity.NewFileID()
		d := entity.File{ID: dir, Share: f.share, Type: entity.Directory, Mode: 0o755, Number: 98, Nlink: 2, Parent: dir, Version: entity.VersionOf(1)}
		if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
			if err := putFile(tx, entity.JournalVersion{}, d); err != nil {
				return err
			}
			for i := range 12 {
				var u [32]byte
				u[0] = byte(i + 1)
				v := entity.VersionOf(uint64(i + 2))
				if err := tx.Set(deltaFloorKey(f.share, dir, v, u), encodeEmpty()); err != nil {
					return err
				}
				if err := tx.Set(deltaKey(f.share, dir, u), encodeDelta(delta{Modify: time.Unix(int64(i), 0), Change: time.Unix(int64(i), 0), Version: v})); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.s.SetKeyBudget(k)
		w.max = 0
		folds := 0
		for more := true; more; folds++ {
			var err error
			if more, _, err = f.s.Namespace().FoldDirectory(ctx, f.share, dir); err != nil {
				t.Fatal(err)
			}
		}
		if w.max > k {
			t.Errorf("K=%d: a fold wrote %d keys", k, w.max)
		}
		if per := (k - 3) / 2; folds < 12/per {
			t.Errorf("K=%d: %d folds for 12 deltas, %d a fold", k, folds, per)
		}
		if got := f.get(dir); got.Version != entity.VersionOf(13) {
			t.Errorf("K=%d: version %d after the folds, want 13", k, got.Version.Counter())
		}
	}
}
