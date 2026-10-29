package kvstoretest

import (
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func (f *fs) release(file entity.FileID, v uint64) error {
	return f.s.Existence().Remove(ctx, f.share.ID, file, 0, kvstore.RemoveOp{Version: entity.VersionOf(v), Kind: entity.RemoveRelease}, nil)
}

func (f *fs) newOpen(file entity.FileID) entity.OpenRecord {
	f.t.Helper()
	id, err := f.s.NextID(ctx, kvstore.IDOpen)
	must(f.t, err)
	return entity.OpenRecord{ID: entity.OpenID(id), File: file, Client: 1, Principal: f.admin.Principal, Access: 0x3}
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
	if orphan, err := opens.Delete(ctx, f.share.ID, file.ID, a.ID); err != nil || orphan != (kvstore.Orphaned{}) {
		t.Fatalf("first close: orphaned %v, %v", orphan, err)
	}
	if err := f.release(file.ID, 1); !errors.Is(err, kvstore.ErrHeld) {
		t.Fatalf("release with one durable open: %v", err)
	}
	if orphan, err := opens.Delete(ctx, f.share.ID, file.ID, b.ID); err != nil || orphan != (kvstore.Orphaned{File: file.ID}) {
		t.Fatalf("last close: orphaned %v, %v", orphan, err)
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
	if orphan, err := opens.Delete(ctx, f.share.ID, rootID, r.ID); err != nil || orphan != (kvstore.Orphaned{}) {
		t.Errorf("close of the root's open: orphaned %v, %v", orphan, err)
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

// Two last closes of an unlinked file, committing concurrently: one of
// them names the file orphaned. The opens are persistent, so neither close
// writes a key the other reads.
func testLastClosesRace(t *testing.T, kvo Opener) {
	f, h := newHeldFS(t, kvo.backends()[0], entity.FoldIdentity)
	opens := f.s.Opens()
	_, file := f.create(f.admin, f.root, "a", regular(0o644))
	a, b := f.newOpen(file.ID), f.newOpen(file.ID)
	a.Durability, b.Durability = entity.DurabilityPersistent, entity.DurabilityPersistent
	_, err := f.s.Namespace().Unlink(ctx, f.admin, f.root, []byte("a"), a, b)
	must(t, err)
	var oa, ob kvstore.Orphaned
	ea, eb := race(h,
		func() (err error) { oa, err = opens.Delete(ctx, f.share.ID, file.ID, a.ID); return },
		func() (err error) { ob, err = opens.Delete(ctx, f.share.ID, file.ID, b.ID); return })
	must(t, ea)
	must(t, eb)
	if oa == (kvstore.Orphaned{}) && ob == (kvstore.Orphaned{}) {
		t.Fatal("neither last close named the file orphaned")
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

// A change to an open's locks lands whole in one transaction: its deletes,
// its writes and the open's new record together, nothing for an open that is
// missing or not persistent, and nothing for one past the backend's limits.
// Locks list in offset order, two at one offset being two keys, any 64-bit
// offset and a zero length kept as asked; they never read as opens, and
// closing the open drops them.
func testDurableLockChanges(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	opens := f.s.Opens()
	_, file := f.create(f.admin, f.root, "a", regular(0o644))
	p := f.newOpen(file.ID)
	p.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, p))
	plain := f.newOpen(file.ID)
	must(t, opens.Put(ctx, f.share.ID, plain))
	lock := func(id entity.LockID, off, length uint64) entity.LockRecord {
		return entity.LockRecord{ID: id, Owner: entity.LockOwner{Client: 1, Owner: []byte("x")}, Offset: off, Length: length}
	}
	locks := func(open entity.OpenID) []entity.LockRecord {
		got, err := opens.Locks(ctx, f.share.ID, file.ID, open)
		must(t, err)
		return got
	}
	record := func() entity.OpenRecord {
		ops, err := opens.Of(ctx, f.share.ID, file.ID)
		must(t, err)
		for _, op := range ops {
			if op.ID == p.ID {
				return op
			}
		}
		t.Fatalf("p is not stored: %+v", ops)
		return entity.OpenRecord{}
	}

	if err := opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, nil, []entity.LockRecord{lock(0, 0, 1)}, nil); err == nil {
		t.Error("a lock with no ID was stored")
	}
	a, b := lock(1, 100, 10), lock(1, 0, 10)
	a.Open = plain.ID // a record's Open is not read: it is stored under p
	must(t, opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, nil, []entity.LockRecord{a, b}, nil))
	b2, c := lock(1, 0, 20), lock(2, 0, 5)
	far := lock(3, 1<<63, 0) // an SMB lock past 2^63, of zero length
	rec := p
	rec.LockSeq[0] = 0x13
	f.c.Take()
	must(t, opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, &rec, []entity.LockRecord{b2, c, far}, []entity.LockRecord{a, b}))
	if n := f.c.Take(); n.Sets != 4 || n.Deletes != 2 {
		t.Errorf("the change wrote %+v, want the record, 3 locks and 2 deletes", n)
	}
	got := locks(p.ID)
	if len(got) != 3 || got[0].ID != 1 || got[0].Length != 20 || got[1].ID != 2 || got[2].ID != 3 || got[2].Offset != 1<<63 || got[2].Length != 0 {
		t.Fatalf("locks after the change %+v, want b rewritten and c added in offset order, a gone, the far lock as asked", got)
	}
	if r := record(); r.LockSeq[0] != 0x13 {
		t.Errorf("the open's record after the change: LockSeq[0] = %#x, want 0x13", r.LockSeq[0])
	}
	if ops, err := opens.Of(ctx, f.share.ID, file.ID); err != nil || len(ops) != 2 {
		t.Errorf("the locks read as opens: %+v, %v", ops, err)
	}

	other := rec
	other.ID = plain.ID
	if err := opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, &other, nil, []entity.LockRecord{c}); err == nil {
		t.Error("another open's record written with p's locks")
	}
	if err := opens.SetLocks(ctx, f.share.ID, file.ID, plain.ID, nil, []entity.LockRecord{b}, nil); err == nil {
		t.Error("locks stored under an open that is not persistent")
	}
	if err := opens.SetLocks(ctx, f.share.ID, file.ID, plain.ID+1000, nil, []entity.LockRecord{b}, nil); !merrs.IsNotFoundError(err) {
		t.Errorf("locks under an open with no record: %v, want not found", err)
	}
	if got := locks(plain.ID); len(got) != 0 {
		t.Errorf("a refused change stored %+v", got)
	}

	// Writes past the transaction's byte limit, with a delete and a new
	// record: none of them lands.
	l := f.c.Limits()
	owner := min(l.Value/2, 1<<16)
	var big []entity.LockRecord
	for i := range l.TxnBytes/owner + 1 {
		r := lock(entity.LockID(i+10), 1000, 1)
		r.Owner.Owner = make([]byte, owner)
		big = append(big, r)
	}
	rec2 := rec
	rec2.LockSeq[0] = 0x14
	if err := opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, &rec2, big, []entity.LockRecord{c}); !errors.Is(err, kv.ErrTooLarge) {
		t.Fatalf("a change past the limits: %v, want kv.ErrTooLarge", err)
	}
	if got := locks(p.ID); len(got) != 3 || got[1].ID != 2 {
		t.Errorf("locks after a change past the limits %+v, want them as they were", got)
	}
	if r := record(); r.LockSeq[0] != 0x13 {
		t.Errorf("the open's record after a change past the limits: LockSeq[0] = %#x, want 0x13", r.LockSeq[0])
	}

	_, err := opens.Delete(ctx, f.share.ID, file.ID, p.ID)
	must(t, err)
	if got := locks(p.ID); len(got) != 0 {
		t.Errorf("locks after their open closed %+v", got)
	}
}

// A lock change committing inside a close leaves no lock row beneath the
// closed open: the close retries or fails, and the open and its locks go
// together.
func testCloseRacesLockChange(t *testing.T, kvo Opener) {
	f, h := newHeldFS(t, kvo.backends()[0], entity.FoldIdentity)
	opens := f.s.Opens()
	_, file := f.create(f.admin, f.root, "a", regular(0o644))
	p := f.newOpen(file.ID)
	p.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, p))
	l := entity.LockRecord{ID: 1, Owner: entity.LockOwner{Client: 1, Owner: []byte("x")}, Length: 10}
	closeErr, lockErr := race(h,
		func() error { _, err := opens.Delete(ctx, f.share.ID, file.ID, p.ID); return err },
		func() error { return opens.SetLocks(ctx, f.share.ID, file.ID, p.ID, nil, []entity.LockRecord{l}, nil) })
	must(t, lockErr)
	must(t, closeErr)
	got, err := opens.Locks(ctx, f.share.ID, file.ID, p.ID)
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("locks left beneath the closed open: %+v", got)
	}
}

// RFC 14 §9.4: delete pending is stored only while the file has a
// persistent open, and goes with the unlink of the entry it names, not
// with the unlink of another name of the file, and with the close of the
// file's last persistent open.
func testDeletePending(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, opens := f.s.Namespace(), f.s.Opens()
	h, file := f.create(f.admin, f.root, "a", regular(0o644))
	must(t, ns.Link(ctx, f.admin, f.root, []byte("b"), h))
	plain := f.newOpen(file.ID)
	must(t, opens.Put(ctx, f.share.ID, plain))
	_, rootID, err := f.root.Parse()
	must(t, err)
	dp := entity.DeleteRecord{Parent: rootID, Name: []byte("a"), By: f.admin.Principal, Access: 0x10000}
	if err := opens.SetDeletePending(ctx, f.share.ID, file.ID, plain.ID, dp); err == nil {
		t.Error("delete pending stored for an open that is not persistent")
	}
	p := f.newOpen(file.ID)
	p.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, p))
	must(t, opens.SetDeletePending(ctx, f.share.ID, file.ID, p.ID, dp))
	if got, ok, err := opens.DeletePending(ctx, f.share.ID, file.ID); err != nil || !ok || string(got.Name) != "a" || got.By != dp.By {
		t.Fatalf("delete pending %+v, %v, %v", got, ok, err)
	}
	_, err = ns.Unlink(ctx, f.admin, f.root, []byte("b"))
	must(t, err)
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, file.ID); err != nil || !ok {
		t.Errorf("the unlink of another name dropped it: %v, %v", ok, err)
	}
	_, err = ns.Unlink(ctx, f.admin, f.root, []byte("a"))
	must(t, err)
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, file.ID); err != nil || ok {
		t.Errorf("the unlink of its entry kept it: %v, %v", ok, err)
	}

	_, other := f.create(f.admin, f.root, "c", regular(0o644))
	q := f.newOpen(other.ID)
	q.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, q))
	must(t, opens.SetDeletePending(ctx, f.share.ID, other.ID, q.ID, entity.DeleteRecord{Parent: rootID, Name: []byte("c")}))
	must(t, opens.ClearDeletePending(ctx, f.share.ID, other.ID))
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, other.ID); err != nil || ok {
		t.Errorf("after a clear: %v, %v", ok, err)
	}

	// It goes with the file's last persistent open, a volatile one left or
	// not: nothing durable holds the promise past that close.
	q2 := f.newOpen(other.ID)
	q2.Durability = entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, q2))
	must(t, opens.Put(ctx, f.share.ID, f.newOpen(other.ID)))
	must(t, opens.SetDeletePending(ctx, f.share.ID, other.ID, q.ID, entity.DeleteRecord{Parent: rootID, Name: []byte("c")}))
	_, err = opens.Delete(ctx, f.share.ID, other.ID, q.ID)
	must(t, err)
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, other.ID); err != nil || !ok {
		t.Errorf("a close while another persistent open holds the file dropped it: %v, %v", ok, err)
	}
	_, err = opens.Delete(ctx, f.share.ID, other.ID, q2.ID)
	must(t, err)
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, other.ID); err != nil || ok {
		t.Errorf("the last persistent open's close kept it: %v, %v", ok, err)
	}
}

// Two last persistent closes of a file with delete pending, committing
// concurrently: delete pending goes with whichever commits second.
func testDeletePendingRace(t *testing.T, kvo Opener) {
	f, h := newHeldFS(t, kvo.backends()[0], entity.FoldIdentity)
	opens := f.s.Opens()
	_, file := f.create(f.admin, f.root, "a", regular(0o644))
	_, rootID, err := f.root.Parse()
	must(t, err)
	a, b := f.newOpen(file.ID), f.newOpen(file.ID)
	a.Durability, b.Durability = entity.DurabilityPersistent, entity.DurabilityPersistent
	must(t, opens.Put(ctx, f.share.ID, a))
	must(t, opens.Put(ctx, f.share.ID, b))
	must(t, opens.SetDeletePending(ctx, f.share.ID, file.ID, a.ID, entity.DeleteRecord{Parent: rootID, Name: []byte("a")}))
	ea, eb := race(h,
		func() error { _, err := opens.Delete(ctx, f.share.ID, file.ID, a.ID); return err },
		func() error { _, err := opens.Delete(ctx, f.share.ID, file.ID, b.ID); return err })
	must(t, ea)
	must(t, eb)
	if _, ok, err := opens.DeletePending(ctx, f.share.ID, file.ID); err != nil || ok {
		t.Fatalf("delete pending outlived both persistent opens: %v, %v", ok, err)
	}
}

// RFC 14 §4.5: the NSM state number is one per installation, durable, and
// never goes down.
func testNSMState(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	if n, err := s.NSMState(ctx); err != nil || n != 0 {
		t.Fatalf("before any raise: %d, %v", n, err)
	}
	must(t, s.RaiseNSMState(ctx, 3))
	for _, n := range []uint32{3, 1} {
		if err := s.RaiseNSMState(ctx, n); !errors.Is(err, kvstore.ErrNSMNotRaised) {
			t.Errorf("raise to %d: %v", n, err)
		}
	}
	again, err := kvstore.Open(ctx, c)
	must(t, err)
	if n, err := again.NSMState(ctx); err != nil || n != 3 {
		t.Errorf("after a restart: %d, %v", n, err)
	}
}

// RFC 14 §2.11: a client, open or lock ID from NextID is never zero and never
// issued twice, by this process or a later one over the same store, which
// may abandon a reserved range but never reissue it. A kind with no
// allocator is refused.
func testNextID(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	kinds := []kvstore.IDKind{kvstore.IDClient, kvstore.IDOpen, kvstore.IDLock}
	seen := map[kvstore.IDKind]map[uint64]bool{}
	draw := func(s *kvstore.Store, n int) {
		t.Helper()
		for _, k := range kinds {
			if seen[k] == nil {
				seen[k] = map[uint64]bool{}
			}
			for range n {
				id, err := s.NextID(ctx, k)
				if err != nil || id == 0 || seen[k][id] {
					t.Fatalf("kind %d: id %d, %v", k, id, err)
				}
				seen[k][id] = true
			}
		}
	}
	draw(s, 1100) // past one reserved range
	again, err := kvstore.Open(ctx, c)
	must(t, err)
	draw(again, 3)
	for _, k := range []kvstore.IDKind{0, 4} {
		if _, err := s.NextID(ctx, k); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("kind %d: %v", k, err)
		}
	}
}
