package kvstoretest

import (
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func (f *fs) release(file entity.FileID, v uint64) error {
	return f.s.Existence().Remove(ctx, f.share.ID, file, 0, kvstore.RemoveOp{Version: entity.VersionOf(v), Kind: entity.RemoveRelease}, nil)
}

func (f *fs) newOpen(file entity.FileID) entity.Open {
	f.t.Helper()
	id, err := f.s.NextOpenID(ctx)
	must(f.t, err)
	return entity.Open{ID: id, File: file, Client: 1, Principal: f.admin.Principal, Access: 0x3}
}

func (f *fs) durable(shard uint64) []kvstore.DurableOpen {
	f.t.Helper()
	return all(f.t, f.s.Opens().Durable(ctx, f.share.ID, shard))
}

// RFC 14 §9.1 and §9.2: an open is the second holder of a file. An unlink
// that takes the file's last entry writes its opens durable in the same
// transaction, with their index entries; the release is then refused while
// one remains; the last one's delete reports the file orphaned, and the
// release goes through. An unlink that leaves an entry writes no open.
func testDurableOpens(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, opens := f.s.Namespace(), f.s.Opens()
	h, file := f.create(f.admin, f.root, "a", regular(0o644))
	must(t, ns.Link(ctx, f.admin, f.root, []byte("b"), h))
	a, b := f.newOpen(file.ID), f.newOpen(file.ID)

	// Not the last entry: nothing is written.
	if _, err := ns.Unlink(ctx, f.admin, f.root, []byte("a"), a, b); err != nil {
		t.Fatal(err)
	}
	if got, err := opens.Of(ctx, f.share.ID, file.ID); err != nil || len(got) != 0 {
		t.Fatalf("opens after unlinking one of two names: %+v, %v", got, err)
	}

	// The last entry: both become durable, with their index entries.
	o, err := ns.Unlink(ctx, f.admin, f.root, []byte("b"), a, b)
	must(t, err)
	if o.File != file.ID {
		t.Fatalf("unlink of the last name: %+v", o)
	}
	got, err := opens.Of(ctx, f.share.ID, file.ID)
	must(t, err)
	if len(got) != 2 || got[0].ID != a.ID || got[0].Principal != a.Principal || got[0].Access != a.Access || got[1].ID != b.ID {
		t.Fatalf("durable opens %+v", got)
	}
	if d := f.durable(file.Shard); len(d) != 2 || d[0] != (kvstore.DurableOpen{File: file.ID, Open: a.ID}) {
		t.Errorf("index %+v", d)
	}

	if err := f.release(file.ID, 1); !errors.Is(err, kvstore.ErrHeld) {
		t.Fatalf("release with two durable opens: %v", err)
	}
	if orphan, err := opens.Delete(ctx, f.share.ID, file.ID, a.ID); err != nil || orphan {
		t.Fatalf("first close: orphan %v, %v", orphan, err)
	}
	if err := f.release(file.ID, 1); !errors.Is(err, kvstore.ErrHeld) {
		t.Fatalf("release with one durable open: %v", err)
	}
	if orphan, err := opens.Delete(ctx, f.share.ID, file.ID, b.ID); err != nil || !orphan {
		t.Fatalf("last close: orphan %v, %v", orphan, err)
	}
	if d := f.durable(file.Shard); len(d) != 0 {
		t.Errorf("index after both closes %+v", d)
	}
	must(t, f.release(file.ID, 1))
	if err := opens.Put(ctx, f.share.ID, f.newOpen(file.ID)); !errors.Is(err, entity.ErrStale) {
		t.Errorf("an open of a released file: %v", err)
	}
	if _, err := opens.Delete(ctx, f.share.ID, file.ID, a.ID); err == nil {
		t.Error("a close of a released file's open")
	}

	// A rename over a file takes its last entry the same way.
	_, over := f.create(f.admin, f.root, "c", regular(0o644))
	f.create(f.admin, f.root, "d", regular(0o644))
	c := f.newOpen(over.ID)
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("d"), f.root, []byte("c"), c); err != nil {
		t.Fatal(err)
	}
	if got, err := opens.Of(ctx, f.share.ID, over.ID); err != nil || len(got) != 1 || got[0].ID != c.ID {
		t.Errorf("durable opens of a file renamed over %+v, %v", got, err)
	}

	// The root has no entry, and is never orphaned.
	_, rootID, err := f.root.Parse()
	must(t, err)
	r := f.newOpen(rootID)
	r.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, r))
	if orphan, err := opens.Delete(ctx, f.share.ID, rootID, r.ID); err != nil || orphan {
		t.Errorf("close of the root's open: orphan %v, %v", orphan, err)
	}
}

// RFC 14 §9.1: an open by handle of a file with no entry, and the release
// of that file, race: whichever commits second loses, in both orders.
func testOpenRacesRelease(t *testing.T, kvo Opener) {
	for _, openFirst := range []bool{true, false} {
		s, h := heldStore(t, kvo.backends()[0])
		f := setupFS(t, s, nil, entity.FoldIdentity)
		_, file := f.create(f.admin, f.root, "a", regular(0o644))
		_, err := f.s.Namespace().Unlink(ctx, f.admin, f.root, []byte("a"))
		must(t, err)
		op := f.newOpen(file.ID)
		put := func() error { return f.s.Opens().Put(ctx, f.share.ID, op) }
		release := func() error { return f.release(file.ID, 1) }
		if openFirst {
			outer, inner := race(h, release, put)
			if inner != nil || !errors.Is(outer, kvstore.ErrHeld) {
				t.Errorf("open first: open %v, release %v", inner, outer)
			}
		} else {
			outer, inner := race(h, put, release)
			if inner != nil || !errors.Is(outer, entity.ErrStale) {
				t.Errorf("release first: release %v, open %v", inner, outer)
			}
		}
	}
}

// RFC 14 §9.2: a clone holds its source with a durable open of its own
// kind, naming the destination and the clone's version, listed in the
// source shard's index; the clone's last batch closes it.
func testCloneHold(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	_, src := f.create(f.admin, f.root, "src", regular(0o644))
	_, dst := f.create(f.admin, f.root, "dst", regular(0o644))
	ex := f.s.Existence()
	failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: f.share.ID, File: src.ID,
		Ops: []kvstore.PendingOp{{Kind: kvstore.OpWrite, Version: entity.VersionOf(1), Extent: entity.ExtentOf(0, 4096)}}}})
	must(t, err)
	must(t, failed[src.ID])
	op := kvstore.CloneOp{Share: f.share.ID, File: dst.ID, Version: entity.VersionOf(5),
		Spec: entity.CloneSpec{SrcShare: f.share.ID, Src: src.ID, Len: 4096}}
	must(t, ex.Clone(ctx, op, nil))
	got, err := f.s.Opens().Of(ctx, f.share.ID, src.ID)
	must(t, err)
	if len(got) != 1 || got[0].Kind != entity.OpenCloneSource || got[0].CloneDst != dst.ID ||
		got[0].CloneVersion != entity.VersionOf(5) || got[0].Client != 0 || got[0].ID == 0 {
		t.Fatalf("the clone's hold %+v", got)
	}
	if d := f.durable(src.Shard); !slices.Contains(d, kvstore.DurableOpen{File: src.ID, Open: got[0].ID}) {
		t.Errorf("index %+v", d)
	}
	must(t, f.s.Content().Resume(ctx, f.share.ID, dst.ID))
	if got, err := f.s.Opens().Of(ctx, f.share.ID, src.ID); err != nil || len(got) != 0 {
		t.Errorf("the hold after the clone is done %+v, %v", got, err)
	}
	if d := f.durable(src.Shard); len(d) != 0 {
		t.Errorf("index after the clone is done %+v", d)
	}
	if _, err := f.s.Opens().Get(ctx, f.share.ID, src.ID, got[0].ID); !merrs.IsNotFoundError(err) {
		t.Errorf("the hold read after it closed: %v", err)
	}
}
