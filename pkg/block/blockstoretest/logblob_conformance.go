package blockstoretest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
)

// LogBlobFactory creates a fresh *logblob.Manager for a single subtest along
// with the directory it was opened on and a cleanup closure. The Manager MUST
// be created with SizeCap ≤ 256 so that rotation can be triggered by small
// test payloads.
type LogBlobFactory func(t *testing.T) (mgr *logblob.Manager, dir string, cleanup func())

// LogBlobConformance runs the logblob Manager conformance suite against any
// *logblob.Manager returned by factory.
//
// Scenarios exercised:
//   - AppendReadAtRoundTrip
//   - Rotate
//   - EvictSealed
//   - EvictActive_Refused
//   - EvictUnsynced_Refused
//   - ReadAtEvicted
//   - RecoverActiveTorn
//   - RecoverSealedTorn
//   - RecoverAckedBoundary
func LogBlobConformance(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	t.Run("AppendReadAtRoundTrip", func(t *testing.T) { lbcAppendReadAt(t, factory) })
	t.Run("Rotate", func(t *testing.T) { lbcRotate(t, factory) })
	t.Run("EvictSealed", func(t *testing.T) { lbcEvictSealed(t, factory) })
	t.Run("EvictActive_Refused", func(t *testing.T) { lbcEvictActiveRefused(t, factory) })
	t.Run("EvictUnsynced_Refused", func(t *testing.T) { lbcEvictUnsyncedRefused(t, factory) })
	t.Run("ReadAtEvicted", func(t *testing.T) { lbcReadAtEvicted(t, factory) })
	t.Run("RecoverActiveTorn", func(t *testing.T) { lbcRecoverActiveTorn(t, factory) })
	t.Run("RecoverSealedTorn", func(t *testing.T) { lbcRecoverSealedTorn(t, factory) })
	t.Run("RecoverAckedBoundary", func(t *testing.T) { lbcRecoverAckedBoundary(t, factory) })
}

// lbcAppendReadAt verifies that a single Append followed by ReadAt returns
// the original bytes at the reported offset.
func lbcAppendReadAt(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	data := []byte("lbc-append-readat-roundtrip")
	loc, err := m.Append(ctx, data)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	dst := make([]byte, loc.RawLength)
	n, err := m.ReadAt(ctx, loc, dst)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(data) || !bytes.Equal(dst[:n], data) {
		t.Errorf("ReadAt = %q (%d bytes), want %q", dst[:n], n, data)
	}
}

// lbcRotate verifies that Rotate seals the active blob and the next Append
// lands in a new blob with a higher ID.
func lbcRotate(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	loc1, err := m.Append(ctx, []byte("before-rotate"))
	if err != nil {
		t.Fatalf("Append(before): %v", err)
	}
	if err := m.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	loc2, err := m.Append(ctx, []byte("after-rotate"))
	if err != nil {
		t.Fatalf("Append(after): %v", err)
	}
	if loc1.LogBlobID == loc2.LogBlobID {
		t.Errorf("Rotate did not produce a new blob: both use %q", loc1.LogBlobID)
	}
	if loc2.LogBlobID <= loc1.LogBlobID {
		t.Errorf("new blobID %q not > old %q", loc2.LogBlobID, loc1.LogBlobID)
	}

	// Both still readable.
	for _, tc := range []struct {
		loc  block.LocalChunkLocation
		want []byte
	}{
		{loc1, []byte("before-rotate")},
		{loc2, []byte("after-rotate")},
	} {
		dst := make([]byte, tc.loc.RawLength)
		if _, err := m.ReadAt(ctx, tc.loc, dst); err != nil {
			t.Errorf("ReadAt(%s): %v", tc.loc.LogBlobID, err)
		} else if !bytes.Equal(dst, tc.want) {
			t.Errorf("ReadAt(%s) = %q, want %q", tc.loc.LogBlobID, dst, tc.want)
		}
	}
}

// lbcEvictSealed verifies that a sealed blob can be evicted: the .blob file
// is removed from disk and ReadAt returns ErrEvicted.
//
// With SizeCap=128, a 100-byte payload fills the active blob beyond the cap on
// the second append, which triggers rotation before the second write.
func lbcEvictSealed(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, dir, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	// First append: goes into blob 0. Its 100 bytes exceed SizeCap=128 on the
	// next call, so blob 0 gets sealed before the second append lands.
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("S"), 100))
	if err != nil {
		t.Fatalf("Append(0): %v", err)
	}
	blob0 := loc0.LogBlobID

	// Second append triggers rotation; blob0 is now sealed.
	if _, err := m.Append(ctx, bytes.Repeat([]byte("T"), 100)); err != nil {
		t.Fatalf("Append(1): %v", err)
	}

	// Evict blob0.
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob: %v", err)
	}

	// File must be gone.
	blobPath := filepath.Join(dir, blob0+".blob")
	if _, statErr := os.Stat(blobPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("evicted blob file still present at %s (stat err: %v)", blobPath, statErr)
	}

	// ReadAt on the evicted blob must return ErrEvicted.
	dst := make([]byte, loc0.RawLength)
	_, readErr := m.ReadAt(ctx, loc0, dst)
	if !errors.Is(readErr, logblob.ErrEvicted) {
		t.Errorf("ReadAt after eviction: got %v, want ErrEvicted", readErr)
	}
}

// lbcEvictActiveRefused verifies that EvictBlob on the active blob returns
// ErrActiveBlob.
func lbcEvictActiveRefused(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	loc, err := m.Append(ctx, []byte("active"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	evictErr := m.EvictBlob(ctx, loc.LogBlobID, func(string) bool { return true })
	if !errors.Is(evictErr, logblob.ErrActiveBlob) {
		t.Errorf("EvictBlob(active): got %v, want ErrActiveBlob", evictErr)
	}
}

// lbcEvictUnsyncedRefused verifies that EvictBlob with a synced function that
// returns false returns ErrUnsyncedBytes.
func lbcEvictUnsyncedRefused(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("U"), 100))
	if err != nil {
		t.Fatalf("Append(0): %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, bytes.Repeat([]byte("V"), 100)); err != nil {
		t.Fatalf("Append(1): %v", err)
	}

	evictErr := m.EvictBlob(ctx, blob0, func(string) bool { return false })
	if !errors.Is(evictErr, logblob.ErrUnsyncedBytes) {
		t.Errorf("EvictBlob(unsynced): got %v, want ErrUnsyncedBytes", evictErr)
	}
}

// lbcReadAtEvicted verifies that ReadAt after EvictBlob returns ErrEvicted.
func lbcReadAtEvicted(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("R"), 100))
	if err != nil {
		t.Fatalf("Append(0): %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, bytes.Repeat([]byte("Q"), 100)); err != nil {
		t.Fatalf("Append(1): %v", err)
	}
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob: %v", err)
	}

	dst := make([]byte, loc0.RawLength)
	_, readErr := m.ReadAt(ctx, loc0, dst)
	if !errors.Is(readErr, logblob.ErrEvicted) {
		t.Errorf("ReadAt after eviction: got %v, want ErrEvicted", readErr)
	}
}

// lbcRecoverActiveTorn injects garbage past the valid tail of the active blob,
// calls Recover to the valid offset, then verifies that Append resumes at that
// offset and prior reads succeed.
func lbcRecoverActiveTorn(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, dir, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	p1 := bytes.Repeat([]byte("A"), 50)
	p2 := bytes.Repeat([]byte("B"), 50)

	loc1, err := m.Append(ctx, p1)
	if err != nil {
		t.Fatalf("Append(1): %v", err)
	}
	loc2, err := m.Append(ctx, p2)
	if err != nil {
		t.Fatalf("Append(2): %v", err)
	}
	mark := loc2.RawOffset + loc2.RawLength // = 100

	blobPath := filepath.Join(dir, loc1.LogBlobID+".blob")
	f, openErr := os.OpenFile(blobPath, os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatalf("OpenFile: %v", openErr)
	}
	if _, werr := f.WriteAt(bytes.Repeat([]byte("Z"), 20), mark); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteAt garbage: %v", werr)
	}
	_ = f.Close()

	if err := m.Recover(ctx, loc1.LogBlobID, mark); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Append must resume at mark.
	afterLoc, err := m.Append(ctx, []byte("after"))
	if err != nil {
		t.Fatalf("Append(after): %v", err)
	}
	if afterLoc.RawOffset != mark {
		t.Errorf("RawOffset after Recover = %d, want %d", afterLoc.RawOffset, mark)
	}

	// Prior reads must succeed.
	for _, tc := range []struct {
		loc  block.LocalChunkLocation
		want []byte
	}{
		{loc1, p1},
		{loc2, p2},
	} {
		dst := make([]byte, tc.loc.RawLength)
		if _, err := m.ReadAt(ctx, tc.loc, dst); err != nil {
			t.Errorf("ReadAt(%s): %v", tc.loc.LogBlobID, err)
		} else if !bytes.Equal(dst, tc.want) {
			t.Errorf("ReadAt(%s) = %q, want %q", tc.loc.LogBlobID, dst, tc.want)
		}
	}

	// Reading in the former garbage zone must fail.
	garbageLoc := block.LocalChunkLocation{
		LogBlobID: loc1.LogBlobID,
		RawOffset: mark + 5,
		RawLength: 5,
	}
	dst := make([]byte, 5)
	_, readErr := m.ReadAt(ctx, garbageLoc, dst)
	if readErr == nil {
		t.Error("ReadAt beyond Recover boundary: expected error, got nil")
	}
}

// lbcRecoverSealedTorn injects garbage past the valid tail of a sealed blob,
// calls Recover to the valid offset, then verifies that prior reads succeed
// and the file size matches the recovery offset.
func lbcRecoverSealedTorn(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, dir, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	p1 := bytes.Repeat([]byte("C"), 50)
	p2 := bytes.Repeat([]byte("D"), 50)

	loc1, err := m.Append(ctx, p1)
	if err != nil {
		t.Fatalf("Append(1): %v", err)
	}
	loc2, err := m.Append(ctx, p2)
	if err != nil {
		t.Fatalf("Append(2): %v", err)
	}
	blob0 := loc1.LogBlobID
	validOffset := loc2.RawOffset + loc2.RawLength // = 100

	// Seal blob0 by rotating.
	if err := m.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Inject garbage past validOffset.
	blobPath := filepath.Join(dir, blob0+".blob")
	f, openErr := os.OpenFile(blobPath, os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatalf("OpenFile: %v", openErr)
	}
	if _, werr := f.WriteAt(bytes.Repeat([]byte("X"), 30), validOffset); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteAt garbage: %v", werr)
	}
	_ = f.Close()

	// Recover sealed blob.
	if err := m.Recover(ctx, blob0, validOffset); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Reads before offset must succeed.
	for _, tc := range []struct {
		loc  block.LocalChunkLocation
		want []byte
	}{
		{loc1, p1},
		{loc2, p2},
	} {
		dst := make([]byte, tc.loc.RawLength)
		if _, err := m.ReadAt(ctx, tc.loc, dst); err != nil {
			t.Errorf("ReadAt(%s): %v", tc.loc.LogBlobID, err)
		} else if !bytes.Equal(dst, tc.want) {
			t.Errorf("ReadAt(%s) = %q, want %q", tc.loc.LogBlobID, dst, tc.want)
		}
	}

	// File size must equal validOffset.
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if fi.Size() != validOffset {
		t.Errorf("file size = %d, want %d", fi.Size(), validOffset)
	}
}

// lbcRecoverAckedBoundary verifies the acked/unacked boundary: data at or
// before the mark survives, data after is discarded.
func lbcRecoverAckedBoundary(t *testing.T, factory LogBlobFactory) {
	t.Helper()
	m, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	acked := bytes.Repeat([]byte("E"), 50)
	unacked := bytes.Repeat([]byte("F"), 50)

	locAcked, err := m.Append(ctx, acked)
	if err != nil {
		t.Fatalf("Append(acked): %v", err)
	}
	mark := locAcked.RawOffset + locAcked.RawLength // = 50

	locUnacked, err := m.Append(ctx, unacked)
	if err != nil {
		t.Fatalf("Append(unacked): %v", err)
	}

	blobID := locAcked.LogBlobID
	if err := m.Recover(ctx, blobID, mark); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Acked data must still be readable.
	dst := make([]byte, locAcked.RawLength)
	if _, err := m.ReadAt(ctx, locAcked, dst); err != nil {
		t.Fatalf("ReadAt(acked): %v", err)
	}
	if !bytes.Equal(dst, acked) {
		t.Errorf("ReadAt(acked) = %q, want %q", dst, acked)
	}

	// Unacked data is now beyond truncation: reading must fail or return a
	// short read (file ends at mark).
	dstU := make([]byte, locUnacked.RawLength)
	n, readErr := m.ReadAt(ctx, locUnacked, dstU)
	if readErr == nil && n == len(unacked) {
		t.Error("ReadAt(unacked) after Recover: expected error or short read, got full success")
	}

	// Next Append must resume at mark.
	postLoc, err := m.Append(ctx, []byte("post-recover"))
	if err != nil {
		t.Fatalf("Append(post): %v", err)
	}
	if postLoc.RawOffset != mark {
		t.Errorf("Append after Recover: RawOffset = %d, want %d", postLoc.RawOffset, mark)
	}
}
