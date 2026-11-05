//go:build engine_conformance

package conformance

import (
	"bytes"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
