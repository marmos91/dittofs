package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// atimeReader mirrors the READ handler's LastAccessTime step (read.go step 12)
// and the CLOSE flush (close.go step 4) against a real metadata service, so the
// tests below observe the store writes those paths actually issue. The SMB READ
// handler itself needs session/tree/blockstore plumbing that these tests do not
// exercise.
type atimeReader struct {
	t        *testing.T
	meta     *metadata.Service
	authCtx  *metadata.AuthContext
	handle   metadata.FileHandle
	openFile *OpenFile
	writes   int // store writes issued so far
}

func newAtimeReader(t *testing.T) *atimeReader {
	t.Helper()
	h, authCtx, handle, openFile := setupTimestampTest(t)
	return &atimeReader{
		t:        t,
		meta:     h.Registry.GetMetadataService(),
		authCtx:  authCtx,
		handle:   handle,
		openFile: openFile,
	}
}

// setAtime writes an access time straight to the metadata store.
func (r *atimeReader) setAtime(t time.Time) {
	r.t.Helper()
	if _, err := r.meta.SetFileAttributes(r.authCtx, r.handle, &metadata.SetAttrs{Atime: &t}); err != nil {
		r.t.Fatalf("SetFileAttributes: %v", err)
	}
}

// read performs the atime bump a successful READ at time now would perform.
func (r *atimeReader) read(now time.Time) {
	r.t.Helper()
	if r.openFile.IsAtimeFrozen() {
		return
	}
	if noteSmbAccess(r.openFile, now) {
		r.setAtime(now)
		r.writes++
	}
}

// closeHandle performs the CLOSE-time flush of a coalesced access time.
func (r *atimeReader) closeHandle() {
	r.t.Helper()
	if pending := takeSmbPendingAtime(r.openFile); !pending.IsZero() && !r.openFile.IsAtimeFrozen() {
		if !r.file().Atime.Before(pending) {
			return // the store already holds a newer access time
		}
		r.setAtime(pending)
		r.writes++
	}
}

func (r *atimeReader) file() *metadata.File {
	r.t.Helper()
	file, err := r.meta.GetFile(r.authCtx.Context, r.handle)
	if err != nil {
		r.t.Fatalf("GetFile: %v", err)
	}
	return file
}

// storedAtime is the LastAccessTime that actually reached the metadata store.
func (r *atimeReader) storedAtime() time.Time {
	r.t.Helper()
	return r.file().Atime
}

// visibleAtime is the LastAccessTime QUERY_INFO reports: the stored value with
// the handle's coalesced access time overlaid.
func (r *atimeReader) visibleAtime() time.Time {
	r.t.Helper()
	file := r.file()
	applySmbPendingAtime(r.openFile, file)
	return file.Atime
}

// TestSmbAtime_ReadsWithinWindowWriteOnce asserts a burst of READs on one handle
// costs a single metadata store write instead of one per READ.
func TestSmbAtime_ReadsWithinWindowWriteOnce(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	for i := range 8 {
		r.read(base.Add(time.Duration(i) * 10 * time.Millisecond))
	}

	if r.writes != 1 {
		t.Fatalf("store writes = %d, want 1 for 8 reads inside the window", r.writes)
	}
	// The store holds only the first read's time: the other seven were elided,
	// not merely overwritten.
	if got := r.storedAtime(); !got.Equal(base) {
		t.Fatalf("stored atime = %v, want the first read's time %v", got, base)
	}
}

// TestSmbAtime_NewestAccessIsVisible asserts a coalesced access time is still
// reported by QUERY_INFO even though it has not reached the store.
func TestSmbAtime_NewestAccessIsVisible(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	last := base.Add(50 * time.Millisecond)
	r.read(base)
	r.read(last)

	if got := r.visibleAtime(); !got.Equal(last) {
		t.Fatalf("visible atime = %v, want the newest read %v", got, last)
	}
	// A newer stored value (another handle's explicit set) still wins.
	newer := last.Add(time.Hour)
	r.setAtime(newer)
	if got := r.visibleAtime(); !got.Equal(newer) {
		t.Fatalf("visible atime = %v, want the newer stored value %v", got, newer)
	}
}

// TestSmbAtime_FlushedOnClose asserts the coalesced access time is persisted
// when the handle goes away, so it is not silently lost.
func TestSmbAtime_FlushedOnClose(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	last := base.Add(100 * time.Millisecond)
	r.read(base)
	r.read(last)
	if got := r.storedAtime(); !got.Equal(base) {
		t.Fatalf("stored atime before close = %v, want %v", got, base)
	}

	r.closeHandle()
	if got := r.storedAtime(); !got.Equal(last) {
		t.Fatalf("stored atime after close = %v, want the newest read %v", got, last)
	}
	// Nothing left to flush, so a second close writes nothing.
	writesAfterClose := r.writes
	r.closeHandle()
	if r.writes != writesAfterClose {
		t.Fatalf("store writes = %d after a second close, want %d", r.writes, writesAfterClose)
	}
}

// TestSmbAtime_WindowExpiryWritesAgain asserts atime keeps advancing durably on
// a long-lived handle rather than being pinned to the first read.
func TestSmbAtime_WindowExpiryWritesAgain(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	r.read(base)
	r.read(base.Add(smbAtimeUpdateWindow / 2))
	later := base.Add(smbAtimeUpdateWindow + time.Second)
	r.read(later)

	if r.writes != 2 {
		t.Fatalf("store writes = %d, want 2 (one per elapsed window)", r.writes)
	}
	if got := r.storedAtime(); !got.Equal(later) {
		t.Fatalf("stored atime = %v, want the post-window read %v", got, later)
	}
	// The window write cleared the pending time, so CLOSE has nothing to add.
	r.closeHandle()
	if r.writes != 2 {
		t.Fatalf("store writes = %d after close, want 2", r.writes)
	}
}

// TestSmbAtime_OutOfOrderAccessDoesNotLowerAtime asserts a READ whose sampled
// time lands out of order (concurrent pipelines on one handle) cannot move the
// access time backwards.
func TestSmbAtime_OutOfOrderAccessDoesNotLowerAtime(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	newest := base.Add(500 * time.Millisecond)
	r.read(base)
	r.read(newest)
	r.read(base.Add(100 * time.Millisecond)) // sampled earlier, arrives later

	if got := r.visibleAtime(); !got.Equal(newest) {
		t.Fatalf("visible atime = %v, want the newest access %v", got, newest)
	}
	r.closeHandle()
	if got := r.storedAtime(); !got.Equal(newest) {
		t.Fatalf("stored atime after close = %v, want %v", got, newest)
	}
}

// TestSmbAtime_CloseDoesNotLowerAnotherHandlesAtime asserts the CLOSE flush
// cannot move LastAccessTime backwards over a newer value another handle (or an
// NFS client) persisted while this handle was coalescing.
func TestSmbAtime_CloseDoesNotLowerAnotherHandlesAtime(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	r.read(base)
	r.read(base.Add(50 * time.Millisecond)) // held on the handle, not in the store

	// Another handle explicitly sets a newer LastAccessTime.
	other := base.Add(time.Hour)
	r.setAtime(other)

	r.closeHandle()
	if got := r.storedAtime(); !got.Equal(other) {
		t.Fatalf("stored atime after close = %v, want the other handle's newer value %v", got, other)
	}
}

// TestSmbAtime_FrozenSuppressesEverything asserts SET_INFO(-1) still stops
// READ-driven LastAccessTime updates, including the CLOSE flush.
func TestSmbAtime_FrozenSuppressesEverything(t *testing.T) {
	r := newAtimeReader(t)

	base := time.Now()
	r.read(base)
	r.read(base.Add(10 * time.Millisecond))

	r.openFile.AtimeFrozen = true
	r.read(base.Add(time.Hour))
	r.closeHandle()

	if r.writes != 1 {
		t.Fatalf("store writes = %d, want 1 (only the pre-freeze read)", r.writes)
	}
	if got := r.storedAtime(); !got.Equal(base) {
		t.Fatalf("stored atime = %v, want %v", got, base)
	}
}
