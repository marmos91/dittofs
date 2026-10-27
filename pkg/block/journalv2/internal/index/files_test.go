package index

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// fid returns a test FileID. Every one of them falls in the same shard.
func fid(n byte) core.FileID { return core.NewFileID([16]byte{15: n}) }

// present reports whether the map holds an entry for id.
func present(fs *files, id core.FileID) bool {
	return fs.view(id, func(*file) {})
}

// TestTagOutlivesChangeVersion is the two lifetimes of a file entry. Once
// nothing is held, the change-version entry is dropped, but the tag stays
// while a segment still holds a record of the file, and only the segment's
// unlink drops the entry whole. Dropping the whole entry with its held set
// would let another share claim a file whose records are still on disk; never
// dropping it would keep a tag with nothing on disk to rebuild it from.
func TestTagOutlivesChangeVersion(t *testing.T) {
	var fs files
	id, seg := fid(1), core.SegmentID(3)
	err := fs.update(id, func(f *file) error {
		if err := f.recorded(seg, 7, id); err != nil {
			return err
		}
		f.held.fill(write(0, 8, rec(seg, 0), 5))
		f.write(write(8, 8, rec(seg, 64), 6))
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	fs.view(id, func(f *file) {
		if v, ok := f.changeVersion(); !ok || v != ver(6) {
			t.Fatalf("change version = %v, %v, want %v, true", v, ok, ver(6))
		}
	})

	err = fs.update(id, func(f *file) error {
		f.held.markOffloaded(ext(0, 16), ver(0), ver(6))
		_, err := f.held.release(ext(0, 16), nil)
		return err
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !fs.view(id, func(f *file) {
		if v, ok := f.changeVersion(); ok {
			t.Fatalf("change version = %v with nothing held, want no entry", v)
		}
		if tag, ok := f.owner(); !ok || tag != 7 {
			t.Fatalf("owner = %d, %v, want 7, true", tag, ok)
		}
	}) {
		t.Fatal("entry dropped while a segment holds a record of the file")
	}

	if err := fs.update(id, func(f *file) error { f.segmentGone(seg); return nil }); err != nil {
		t.Fatalf("segmentGone: %v", err)
	}
	if present(&fs, id) {
		t.Fatal("entry kept with nothing held and no record on disk")
	}
	if n := fs.len(); n != 0 {
		t.Fatalf("len() = %d, want 0", n)
	}
}

// TestHeldContentKeepsEntryWithoutSegments checks the other order: a file
// whose last segment is gone but which still holds content keeps its entry
// and change version. A drop keyed on segments alone would lose held content.
func TestHeldContentKeepsEntryWithoutSegments(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		_ = f.recorded(2, 1, id)
		f.write(write(0, 4, rec(2, 0), 9))
		f.segmentGone(2)
		return nil
	})
	if !fs.view(id, func(f *file) {
		if _, ok := f.held.lookup(0); !ok {
			t.Fatal("held content lost")
		}
		if v, ok := f.changeVersion(); !ok || v != ver(9) {
			t.Fatalf("change version = %v, %v, want %v, true", v, ok, ver(9))
		}
	}) {
		t.Fatal("entry dropped while it holds content")
	}
}

// TestSegmentNotedTwiceIsGoneOnce: every record appended notes its segment,
// so one segment is noted many times for a file. If each note were kept, one
// unlink would remove one of them and the entry, tag and all, would outlive
// its last record for good.
func TestSegmentNotedTwiceIsGoneOnce(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		_ = f.recorded(4, 1, id)
		return f.recorded(4, 1, id)
	})
	_ = fs.update(id, func(f *file) error { f.segmentGone(4); return nil })
	if present(&fs, id) {
		t.Fatal("entry kept after the only segment holding its records was unlinked")
	}
}

// TestWriteNeverLowersChangeVersion: the change version is the highest
// version of any change, so an older write held after a newer one keeps it.
// Assigning instead of raising would let a stale fill through.
func TestWriteNeverLowersChangeVersion(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 6))
		f.write(write(8, 4, rec(1, 64), 5))
		return nil
	})
	fs.view(id, func(f *file) {
		if v, _ := f.changeVersion(); v != ver(6) {
			t.Fatalf("change version = %v, want %v", v, ver(6))
		}
	})
}

// TestRecordedReportsOtherTagAndKeepsTheRecord: a record is noted after its
// I/O, so one noted through a tag that does not own the file is already on
// disk. It is reported, the owner stays, and its segment still counts: one
// left unnoted would let the owner's last segment end the tag while that
// record remains, and a later claim by a third tag would succeed.
func TestRecordedReportsOtherTagAndKeepsTheRecord(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error { return f.recorded(1, 4, id) })
	err := fs.update(id, func(f *file) error { return f.recorded(2, 5, id) })
	if !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("recorded through another tag = %v, want ErrOtherShare", err)
	}
	_ = fs.update(id, func(f *file) error { f.segmentGone(1); return nil })
	if !fs.view(id, func(f *file) {
		if tag, ok := f.owner(); !ok || tag != 4 {
			t.Fatalf("owner = %d, %v, want 4, true", tag, ok)
		}
	}) {
		t.Fatal("entry dropped while segment 2 holds a record of the file")
	}
}

// TestPanicLeavesFileUsable: a panic inside an update, recovered by its
// caller, must still release the file's lock, or every later call on the
// file waits forever.
func TestPanicLeavesFileUsable(t *testing.T) {
	var fs files
	id := fid(1)
	func() {
		defer func() { _ = recover() }()
		_ = fs.update(id, func(f *file) error { f.write(write(0, 4, rec(1, 0), 1)); panic("fn") })
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fs.view(id, func(*file) {})
		_ = fs.update(id, func(*file) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a panic in an update left the file locked")
	}
}

// TestReadClaimsNothing: a read appends nothing and claims nothing, so a view
// of an unknown file must not create an entry, and an update that leaves
// nothing behind, a refused one included, must not leave one either.
func TestReadClaimsNothing(t *testing.T) {
	var fs files
	id := fid(1)
	if fs.view(id, func(*file) { t.Fatal("view ran on a file with no entry") }) {
		t.Fatal("view reported an entry for an unknown file")
	}
	refused := errors.New("refused")
	if err := fs.update(id, func(*file) error { return refused }); err != refused {
		t.Fatalf("update = %v, want %v", err, refused)
	}
	if present(&fs, id) || fs.len() != 0 {
		t.Fatal("an update that left nothing kept an entry")
	}
}

// TestClaimRefusesOtherTag: a file's tag is checked per file, and a call
// through another share's tag is refused naming both, leaving the owner in
// place. A claim that overwrote the tag would hand one share's file to another.
func TestClaimRefusesOtherTag(t *testing.T) {
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error {
		return f.recorded(1, 4, id)
	})
	err := fs.update(id, func(f *file) error { return f.claim(5, id) })
	var other *core.OtherShareError
	if !errors.As(err, &other) || !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("claim by another tag = %v, want an OtherShareError", err)
	}
	if other.File != id || other.Tag != 5 || other.Owner != 4 {
		t.Fatalf("refusal = %+v, want file %v, tag 5, owner 4", other, id)
	}
	if err := fs.update(id, func(f *file) error { return f.claim(4, id) }); err != nil {
		t.Fatalf("claim by the owner = %v, want nil", err)
	}
	fs.view(id, func(f *file) {
		if tag, ok := f.owner(); !ok || tag != 4 {
			t.Fatalf("owner = %d, %v, want 4, true", tag, ok)
		}
	})
}

// TestReadOtherFileWhileWriting: while one file's update is stopped inside
// its lock, a read of another file must complete, and so must a write to it.
// A store-wide index lock blocks both. Both files fall in one shard, so a
// shard lock held across fn would block them too.
func TestReadOtherFileWhileWriting(t *testing.T) {
	var fs files
	a, b := fid(1), fid(2)
	_ = fs.update(a, func(f *file) error { f.write(write(0, 4, rec(1, 0), 1)); return nil })

	inside, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = fs.update(b, func(f *file) error {
			f.write(write(0, 4, rec(1, 64), 2))
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	defer close(release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fs.view(a, func(f *file) { f.held.lookup(0) })
		_ = fs.update(a, func(f *file) error { f.write(write(4, 4, rec(1, 128), 3)); return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read and a write of one file waited on a write to another")
	}
}

// TestSameFileReadSeesWholeWrite is the consistent view: a writer replaces
// every piece of the file inside one update, and a reader copying the pieces
// out must see them all at one version. Locking per piece, or copying outside
// the lock, lets a reader see a mix.
func TestSameFileReadSeesWholeWrite(t *testing.T) {
	var fs files
	id := fid(1)
	const pieces, rounds = 4, 2000
	writeAll := func(v uint64) {
		_ = fs.update(id, func(f *file) error {
			for i := range int64(pieces) {
				f.write(write(i*8, 4, rec(1, uint32(v*64+uint64(i)*8)), v))
			}
			return nil
		})
	}
	writeAll(1)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []Piece
			for {
				select {
				case <-stop:
					return
				default:
				}
				fs.view(id, func(f *file) { buf = f.held.pieces(buf[:0]) })
				if len(buf) != pieces {
					errs <- fmt.Errorf("read %d pieces, want %d", len(buf), pieces)
					return
				}
				for _, p := range buf {
					if p.Version != buf[0].Version {
						errs <- fmt.Errorf("one read saw versions %v and %v", buf[0].Version, p.Version)
						return
					}
				}
			}
		}()
	}
	for v := uint64(2); v <= rounds; v++ {
		writeAll(v)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestConcurrentFiles runs readers and writers on one file every writer
// shares and on one that comes and goes: each writer holds and releases its
// own byte of the churn file, whose entry is dropped whenever all of them
// happen to be released and created again by the next write. So writers
// regularly find an entry another has just dropped, and each checks, after
// its write, that the write is there. A write landing in a dropped entry is
// lost; that check and the final state catch it.
func TestConcurrentFiles(t *testing.T) {
	var fs files
	shared, churn := fid(1), fid(2)
	const workers, rounds = 8, 300
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Add(2)
		go func() { // writer
			defer wg.Done()
			off := int64(w)
			for r := range rounds {
				seg := core.SegmentID(w*rounds + r + 1)
				_ = fs.update(churn, func(f *file) error {
					_ = f.recorded(seg, 1, churn)
					f.held.fill(write(off, 1, rec(seg, 0), 1))
					return nil
				})
				held := false
				fs.view(churn, func(f *file) { _, held = f.held.lookup(off) })
				if !held {
					errs <- fmt.Errorf("writer %d: its write at %d is not held: it landed in a dropped entry", w, off)
					return
				}
				_ = fs.update(churn, func(f *file) error {
					_, err := f.held.release(ext(off, 1), nil)
					f.segmentGone(seg)
					return err
				})
				_ = fs.update(shared, func(f *file) error {
					f.write(write(int64(w*rounds+r)*4, 4, rec(seg, 0), uint64(r+1)))
					return nil
				})
			}
		}()
		go func() { // reader of both files
			defer wg.Done()
			var buf []Placed
			var gaps []core.Extent
			for range rounds {
				fs.view(shared, func(f *file) { buf, gaps = f.held.span(ext(0, 1<<20), buf[:0], gaps[:0]) })
				fs.view(churn, func(f *file) { f.held.lookup(0) })
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if present(&fs, churn) {
		t.Fatal("churn file kept an entry with nothing held and no record on disk")
	}
	fs.view(shared, func(f *file) {
		if n := f.held.len(); n != workers*rounds {
			t.Fatalf("shared file holds %d pieces, want %d: a write was lost", n, workers*rounds)
		}
	})
	if n := fs.len(); n != 1 {
		t.Fatalf("len() = %d, want 1", n)
	}
}

// TestViewAllocatesNothing: a read of an existing file goes through view on
// every request, so its closure must not escape to the heap.
func TestViewAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	var fs files
	id := fid(1)
	_ = fs.update(id, func(f *file) error { f.write(write(0, 4, rec(1, 0), 1)); return nil })
	var got Placed
	allocs := testing.AllocsPerRun(1000, func() {
		fs.view(id, func(f *file) { got, _ = f.held.lookup(2) })
	})
	if allocs != 0 {
		t.Fatalf("view allocated %v times per call, want 0", allocs)
	}
	_ = got
}
