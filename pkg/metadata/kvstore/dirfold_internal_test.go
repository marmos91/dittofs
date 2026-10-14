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
