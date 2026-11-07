//go:build engine_conformance

package conformance

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// write writes p at off of file id and returns its verifier.
func write(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, p []byte) vfserr.Verifier {
	t.Helper()
	v, err := f.E.Write(t.Context(), f.Share.ID, id, off, p, engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatalf("write %d bytes at %d: %v", len(p), off, err)
	}
	return v
}

// read reads n bytes at off of file id, failing on a short read.
func read(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	got, err := f.E.Read(t.Context(), f.Share.ID, id, off, p)
	if err != nil || got != n {
		t.Fatalf("read %d bytes at %d: %d, %v", n, off, got, err)
	}
	return p
}

// eventually waits, in real time, for cond to hold: what the engine's own
// goroutines do after the test clock moves happens in real time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// durable reports whether a crash now would leave p in some file of the
// journal's directory.
func durable(f *enginetest.Fixture, p []byte) bool {
	names, err := f.Seam.List()
	if err != nil {
		return false
	}
	for _, name := range names {
		if image, err := f.Seam.Durable(name); err == nil && bytes.Contains(image, p) {
			return true
		}
	}
	return false
}

// A write is the journal's before any stability point: Overlay, Allocation
// and reads reflect it, and after a crash recovery re-applies its existence
// before the file is served.
func TestRFC8A_JournalAuthority(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	h, id := f.File("a")
	p := []byte("the journal is the authority")
	write(t, f, id, 0, p)

	served := func(when string) {
		t.Helper()
		sp, err := f.E.Allocation(ctx, f.Share.ID, id, 0)
		if err != nil || sp.Class == entity.Hole || sp.Offset > 0 || sp.Offset+sp.Length < int64(len(p)) {
			t.Errorf("%s: Allocation(0) = %+v, %v; want data over [0, %d)", when, sp, err, len(p))
		}
		if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
			t.Errorf("%s: read %q, want %q", when, got, p)
		}
	}
	if o, pending, err := f.E.Overlay(ctx, f.Share.ID, id); err != nil || !pending || o.Size != int64(len(p)) {
		t.Errorf("before the crash: Overlay = %+v, pending %v, %v; want size %d pending", o, pending, err, len(p))
	}
	served("before the crash")

	// The sync bound is shorter than the existence age, so the write is made
	// durable and its existence is still uncommitted when the crash comes:
	// only recovery can commit it.
	f.Clock.Advance(journal.SyncBound)
	eventually(t, "the write is durable within the sync bound", func() bool { return durable(f, p) })
	f.Crash()
	if got := f.Committed(h).Size; got != int64(len(p)) {
		t.Errorf("served after the crash with committed size %d, want %d re-applied", got, len(p))
	}
	served("after the crash")
}

// Two writes in one process answer one verifier; a write after a restart
// answers another.
func TestRFC8A_Verifier(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	v1 := write(t, f, id, 0, []byte("one"))
	if v2 := write(t, f, id, 3, []byte("two")); v2 != v1 {
		t.Errorf("two writes in one process: verifiers %x and %x differ", v1, v2)
	}
	f.Restart()
	if v3 := write(t, f, id, 6, []byte("three")); v3 == v1 {
		t.Errorf("a write after a restart answers the verifier %x from before it", v3)
	}
}

// A Commit over an append answers after the journal sync and before any
// metadata transaction; a crash before the group commit leaves the size and
// mtime to recovery, which commits them before the file is served.
func TestRFC8A_StableWriteLazyExistence(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	h, id := f.File("a")
	p := []byte("appended, then committed")
	at := f.Clock.Now()

	f.FS.Counts.Take()
	write(t, f, id, 0, p)
	if _, _, err := f.E.Commit(ctx, f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if c := f.FS.Counts.Take(); c.Sets+c.Deletes > 0 {
		t.Errorf("Write and Commit wrote metadata: %+v; existence waits for the group commit", c)
	}

	// The crash drops every unsynced record: what survives was synced before
	// Commit answered.
	f.Crash()
	if c := f.Committed(h); c.Size != int64(len(p)) || !c.Modify.Equal(at) {
		t.Errorf("after the crash: committed size %d, mtime %v; want size %d, mtime %v", c.Size, c.Modify, len(p), at)
	}
	if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
		t.Errorf("after the crash: read %q, want %q", got, p)
	}
}

// TestRFC8A_AttachAboveTheFloor, adapted: a share whose files' versions reach
// far above the journal's counter is bound, and a write over its content is
// given a version above them. The store stamps its own versions from the
// clock in nanoseconds, so the floor is set relative to the store's, not at
// 9·10⁸. With no offload or eviction in this part, a read proves nothing.
func TestRFC8A_AttachAboveTheFloor(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.FS.Create(t, f.FS.Admin, f.SecondRoot, "imported", kvstore.Regular(0o644))

	base, err := f.FS.Store.Content().VersionFloor(ctx, []entity.ShareID{f.Second.ID})
	if err != nil {
		t.Fatal(err)
	}

	imported := entity.VersionOf(base.Counter() + 1e12)
	pending := []kvstore.PendingExistence{
		{
			Share: f.Second.ID,
			File:  file.ID,
			Ops: []kvstore.PendingOp{
				{
					Version: imported,
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: 4096},
					Mtime:   f.Clock.Now(),
					Ctime:   f.Clock.Now(),
				},
			},
		},
	}

	failed, err := f.FS.Store.Existence().Commit(ctx, pending, nil)
	if err != nil || failed[file.ID] != nil {
		t.Fatalf("commit the imported content: %v, %v", err, failed[file.ID])
	}

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, err = f.E.Write(ctx, f.Second.ID, file.ID, 0, []byte("new"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatalf("write over the imported content: %v", err)
	}

	h := f.HandleOf(f.Second.Tag)
	changes := 0

	for c, err := range h.Since(ctx, journal.NewFileID(file.ID), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}

		changes++

		if c.Version.Counter <= imported.Counter() {
			t.Errorf("the write's version %d is not above the imported %d", c.Version.Counter, imported.Counter())
		}
	}

	if changes == 0 {
		t.Error("the journal holds no change of the write")
	}
}

// lostJournal reports whether f's engine raises journal_lost for its device.
func lostJournal(f *enginetest.Fixture) bool {
	for _, c := range f.E.Stats().Journals[f.Dir].Conditions {
		if c.Name == "journal_lost" {
			return true
		}
	}

	return false
}

// TestRFC8A_JournalGone, adapted: two shares written without offload, the
// journal directory deleted, and a restart refuses both shares by name with
// the device, raises journal_lost and creates no journal. The operator's
// acknowledgement that serves them again is not built here.
func TestRFC8A_JournalGone(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("f")
	write(t, f, file, 0, []byte("unoffloaded"))

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	opts := engine.WriteOpts{
		Done: func() {
		},
	}

	if _, err := f.E.Write(ctx, f.Second.ID, entity.NewFileID(), 0, []byte("second"), opts); err != nil {
		t.Fatalf("write to the second share: %v", err)
	}

	f.RestartAfter(func() {
		if err := os.RemoveAll(f.Dir); err != nil {
			t.Fatalf("delete the journal: %v", err)
		}
	})

	for _, sh := range []engine.Share{f.Share, f.Second} {
		if got := f.E.Stats().Shares[sh.ID]; got.Device != f.Dir || got.Refused == nil {
			t.Errorf("share %v: device %q, refused %v; want refused on %q", sh.ID, got.Device, got.Refused, f.Dir)
		}
	}

	if !lostJournal(f) {
		t.Error("journal_lost is not raised")
	}

	if _, err := os.Stat(f.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted journal directory: %v, want it still gone", err)
	}
}

// TestRFC8A_AttachAfterDeviceLoss, adapted: the device's journal destroyed
// and initialised again, its share is refused by name until the store
// records the new journal, as the operator's step would, and then attaches
// to it with its first version above the limit it had. The rewrite of the
// shards' journal identity and a share whose removal reads offloaded are not
// built here.
func TestRFC8A_AttachAfterDeviceLoss(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("f")
	write(t, f, file, 0, []byte("lost"))

	old, err := f.FS.Store.VersionLimit(ctx, f.Share.ID)
	if err != nil {
		t.Fatal(err)
	}

	var id journal.JournalID

	f.RestartAfter(func() {
		id = f.Reinit()
	})

	if got := f.E.Stats().Shares[f.Share.ID]; got.Device != f.Dir || got.Refused == nil {
		t.Fatalf("before the journal is recorded: device %q, refused %v; want refused on %q", got.Device, got.Refused, f.Dir)
	}

	f.RestartAfter(func() {
		if err := f.FS.Store.RecordJournal(ctx, id, f.Node, entity.JournalDir(f.Dir)); err != nil {
			t.Fatalf("record the new journal: %v", err)
		}
	})

	if got := f.E.Stats().Shares[f.Share.ID]; got.Refused != nil {
		t.Fatalf("after the journal is recorded, the share is refused: %v", got.Refused)
	}

	_, again := f.File("again")
	write(t, f, again, 0, []byte("after"))

	changes := 0

	for c, err := range f.Handle().Since(ctx, journal.NewFileID(again), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}

		changes++

		if c.Version.Counter <= old.Counter() {
			t.Errorf("the first write's version %d is not above the old limit %d", c.Version.Counter, old.Counter())
		}
	}

	if changes == 0 {
		t.Error("the journal holds no change of the write")
	}
}
