package logblob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
)

// TestAppendReadAtRoundTrip: single Append followed by a ReadAt returns the
// original bytes at the reported offset.
func TestAppendReadAtRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	data := []byte("hello, log blob!")
	ctx := context.Background()

	loc, err := m.Append(ctx, data)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if loc.RawLength != int64(len(data)) {
		t.Errorf("RawLength = %d, want %d", loc.RawLength, len(data))
	}
	if loc.RawOffset != 0 {
		t.Errorf("RawOffset = %d, want 0 (first append)", loc.RawOffset)
	}
	if loc.LogBlobID == "" {
		t.Error("LogBlobID must not be empty")
	}

	dst := make([]byte, loc.RawLength)
	n, err := m.ReadAt(ctx, loc, dst)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(data) {
		t.Errorf("ReadAt n = %d, want %d", n, len(data))
	}
	if !bytes.Equal(dst, data) {
		t.Errorf("ReadAt data = %q, want %q", dst, data)
	}
}

// TestManyInterleavedAppends: many sequential Appends; each ReadAt using the
// returned LocalChunkLocation returns the exact bytes written.
func TestManyInterleavedAppends(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	const n = 100
	locs := make([]block.LocalChunkLocation, n)
	payloads := make([][]byte, n)

	for i := 0; i < n; i++ {
		payloads[i] = []byte(fmt.Sprintf("payload-%04d-data", i))
		locs[i], err = m.Append(ctx, payloads[i])
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		dst := make([]byte, locs[i].RawLength)
		nn, err := m.ReadAt(ctx, locs[i], dst)
		if err != nil {
			t.Fatalf("ReadAt[%d]: %v", i, err)
		}
		if nn != len(payloads[i]) {
			t.Errorf("ReadAt[%d] n = %d, want %d", i, nn, len(payloads[i]))
		}
		if !bytes.Equal(dst, payloads[i]) {
			t.Errorf("ReadAt[%d] = %q, want %q", i, dst, payloads[i])
		}
	}
}

// TestOffsetMonotonicity: each successive Append starts where the previous one
// ended (within the same blob).
func TestOffsetMonotonicity(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	var prevEnd int64
	prevBlob := ""

	for i := 0; i < 10; i++ {
		p := []byte(fmt.Sprintf("item%04d", i))
		loc, err := m.Append(ctx, p)
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
		if loc.RawLength != int64(len(p)) {
			t.Errorf("[%d] RawLength = %d, want %d", i, loc.RawLength, len(p))
		}
		if prevBlob == "" || loc.LogBlobID == prevBlob {
			// Same blob: offset must be contiguous.
			if i > 0 && loc.LogBlobID == prevBlob && loc.RawOffset != prevEnd {
				t.Errorf("[%d] RawOffset = %d, want %d (contiguous)", i, loc.RawOffset, prevEnd)
			}
			prevEnd = loc.RawOffset + loc.RawLength
		} else {
			// New blob after rotation: offset resets to 0.
			if loc.RawOffset != 0 {
				t.Errorf("[%d] after rotation: RawOffset = %d, want 0", i, loc.RawOffset)
			}
			prevEnd = loc.RawLength
		}
		prevBlob = loc.LogBlobID
	}
}

// TestConcurrentReadsDuringAppend: concurrent ReadAt of already-committed
// offsets runs cleanly alongside active Appends under the race detector.
func TestConcurrentReadsDuringAppend(t *testing.T) {
	dir := t.TempDir()
	// Small SizeCap to trigger rotations during the test.
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()

	// Pre-write a batch of known payloads.
	const preN = 30
	preLocs := make([]block.LocalChunkLocation, preN)
	prePayloads := make([][]byte, preN)
	for i := 0; i < preN; i++ {
		prePayloads[i] = []byte(fmt.Sprintf("pre-%04d-xxxxxxxxxx", i))
		preLocs[i], err = m.Append(ctx, prePayloads[i])
		if err != nil {
			t.Fatalf("pre-Append[%d]: %v", i, err)
		}
	}

	var wg sync.WaitGroup

	// Readers: verify each pre-written payload repeatedly.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < preN; i++ {
				dst := make([]byte, preLocs[i].RawLength)
				if _, err := m.ReadAt(ctx, preLocs[i], dst); err != nil {
					t.Errorf("concurrent ReadAt[%d]: %v", i, err)
					return
				}
				if !bytes.Equal(dst, prePayloads[i]) {
					t.Errorf("concurrent ReadAt[%d]: got %q, want %q", i, dst, prePayloads[i])
					return
				}
			}
		}()
	}

	// Appenders: write new data concurrently.
	for a := 0; a < 4; a++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				p := []byte(fmt.Sprintf("concurrent-a%d-i%04d", id, i))
				if _, err := m.Append(ctx, p); err != nil {
					t.Errorf("concurrent Append a=%d i=%d: %v", id, i, err)
					return
				}
			}
		}(a)
	}

	wg.Wait()
}

// TestRotationAtSizeCap: once the active blob reaches SizeCap, a new blob is
// created. The old blob remains readable via its original LocalChunkLocations.
func TestRotationAtSizeCap(t *testing.T) {
	dir := t.TempDir()
	const payloadSize = 100
	const sizeCap = int64(payloadSize * 3) // fits exactly 3 writes before rotation

	m, err := logblob.Open(dir, logblob.Options{SizeCap: sizeCap})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	const total = 5
	locs := make([]block.LocalChunkLocation, total)
	payloads := make([][]byte, total)

	for i := 0; i < total; i++ {
		payloads[i] = bytes.Repeat([]byte{byte('A' + i)}, payloadSize)
		locs[i], err = m.Append(ctx, payloads[i])
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// Expect at least 2 blobs.
	blobs, err := m.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if len(blobs) < 2 {
		t.Fatalf("expected ≥2 blobs after rotation, got %d", len(blobs))
	}

	// First and last writes should be in different blobs.
	if locs[0].LogBlobID == locs[total-1].LogBlobID {
		t.Errorf("expected rotation: locs[0] and locs[%d] both in %q", total-1, locs[0].LogBlobID)
	}

	// All data must be readable regardless of which blob it landed in.
	for i, loc := range locs {
		dst := make([]byte, loc.RawLength)
		n, err := m.ReadAt(ctx, loc, dst)
		if err != nil {
			t.Fatalf("ReadAt[%d] after rotation: %v", i, err)
		}
		if n != payloadSize {
			t.Errorf("ReadAt[%d] n = %d, want %d", i, n, payloadSize)
		}
		if !bytes.Equal(dst, payloads[i]) {
			t.Errorf("ReadAt[%d] = %q, want %q", i, dst, payloads[i])
		}
	}
}

// TestCallerDrivenRotate: the explicit Rotate() call seals the current blob
// and the next Append targets a new blob with a higher ID.
func TestCallerDrivenRotate(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	data1 := []byte("before-rotate")
	loc1, err := m.Append(ctx, data1)
	if err != nil {
		t.Fatalf("Append before Rotate: %v", err)
	}

	if err := m.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	data2 := []byte("after-rotate")
	loc2, err := m.Append(ctx, data2)
	if err != nil {
		t.Fatalf("Append after Rotate: %v", err)
	}

	if loc1.LogBlobID == loc2.LogBlobID {
		t.Errorf("Rotate did not produce a new blob: both use %q", loc1.LogBlobID)
	}
	if loc2.LogBlobID <= loc1.LogBlobID {
		t.Errorf("new blobID %q not monotonically greater than %q", loc2.LogBlobID, loc1.LogBlobID)
	}
	if loc2.RawOffset != 0 {
		t.Errorf("first append after rotate: offset = %d, want 0", loc2.RawOffset)
	}

	// Old blob still readable.
	dst1 := make([]byte, loc1.RawLength)
	if _, err := m.ReadAt(ctx, loc1, dst1); err != nil {
		t.Fatalf("ReadAt after Rotate (old blob): %v", err)
	}
	if !bytes.Equal(dst1, data1) {
		t.Errorf("old blob data = %q, want %q", dst1, data1)
	}

	// New blob also readable.
	dst2 := make([]byte, loc2.RawLength)
	if _, err := m.ReadAt(ctx, loc2, dst2); err != nil {
		t.Fatalf("ReadAt after Rotate (new blob): %v", err)
	}
	if !bytes.Equal(dst2, data2) {
		t.Errorf("new blob data = %q, want %q", dst2, data2)
	}
}

// TestBlobIDStableAcrossReopen: blob IDs present before Close are the same IDs
// visible on the next Open of the same directory.
func TestBlobIDStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	m, err := logblob.Open(dir, logblob.Options{SizeCap: 50})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Write enough to create two blobs. Keep locs+payloads to verify after reopen.
	type entry struct {
		loc  block.LocalChunkLocation
		data []byte
	}
	var entries []entry
	for i := 0; i < 5; i++ {
		p := []byte(fmt.Sprintf("item%04d-xxxxxxxxxxxxxxxxxxx", i)) // ≥ 20 bytes each
		loc, err := m.Append(ctx, p)
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
		entries = append(entries, entry{loc, p})
	}

	blobsBefore, err := m.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs before close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m2, err := logblob.Open(dir, logblob.Options{SizeCap: 50})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()

	// Verify all previously written data is still readable after reopen.
	for i, e := range entries {
		dst := make([]byte, e.loc.RawLength)
		if _, err := m2.ReadAt(ctx, e.loc, dst); err != nil {
			t.Fatalf("ReadAt[%d] after reopen: %v", i, err)
		}
		if !bytes.Equal(dst, e.data) {
			t.Errorf("ReadAt[%d] = %q, want %q", i, dst, e.data)
		}
	}

	blobsAfter, err := m2.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs after reopen: %v", err)
	}

	// Same set of blob IDs.
	beforeIDs := blobIDSet(blobsBefore)
	afterIDs := blobIDSet(blobsAfter)
	for id := range beforeIDs {
		if !afterIDs[id] {
			t.Errorf("blob ID %q present before close but missing after reopen", id)
		}
	}
	for id := range afterIDs {
		if !beforeIDs[id] {
			t.Errorf("blob ID %q appeared after reopen but was not present before", id)
		}
	}
}

// TestReopenResumesAtTail: after Close and reopen, Append writes at the
// correct tail offset (not at 0, not past the end).
func TestReopenResumesAtTail(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	data1 := []byte("first-write-before-close")
	loc1, err := m.Append(ctx, data1)
	if err != nil {
		t.Fatalf("Append before close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m2, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()

	data2 := []byte("second-write-after-reopen")
	loc2, err := m2.Append(ctx, data2)
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}

	// Same blob (no rotation between writes).
	if loc1.LogBlobID != loc2.LogBlobID {
		t.Errorf("expected same blob after reopen: %q vs %q", loc1.LogBlobID, loc2.LogBlobID)
	}

	// loc2 starts where loc1 ended.
	wantOffset := loc1.RawOffset + loc1.RawLength
	if loc2.RawOffset != wantOffset {
		t.Errorf("loc2.RawOffset = %d, want %d", loc2.RawOffset, wantOffset)
	}

	// Both records readable.
	dst1 := make([]byte, loc1.RawLength)
	if _, err := m2.ReadAt(ctx, loc1, dst1); err != nil {
		t.Fatalf("ReadAt loc1 after reopen: %v", err)
	}
	if !bytes.Equal(dst1, data1) {
		t.Errorf("ReadAt loc1 = %q, want %q", dst1, data1)
	}

	dst2 := make([]byte, loc2.RawLength)
	if _, err := m2.ReadAt(ctx, loc2, dst2); err != nil {
		t.Fatalf("ReadAt loc2: %v", err)
	}
	if !bytes.Equal(dst2, data2) {
		t.Errorf("ReadAt loc2 = %q, want %q", dst2, data2)
	}
}

// TestListBlobs: ListBlobs returns all blobs including the active one, with
// exactly one Active blob.
func TestListBlobs(t *testing.T) {
	dir := t.TempDir()
	const sizeCap = 50
	m, err := logblob.Open(dir, logblob.Options{SizeCap: sizeCap})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()

	// Fresh Manager: one active blob.
	blobs, err := m.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs (empty): %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("expected 1 blob initially, got %d", len(blobs))
	}
	if !blobs[0].Active {
		t.Errorf("initial blob must be marked Active")
	}

	// Write enough to trigger at least one rotation.
	for i := 0; i < 8; i++ {
		p := []byte(fmt.Sprintf("data-%d-padding-xxxxxxxxxxx", i))
		if _, err := m.Append(ctx, p); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	blobs, err = m.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs after writes: %v", err)
	}
	if len(blobs) < 2 {
		t.Errorf("expected ≥2 blobs after rotation, got %d", len(blobs))
	}

	activeCount := 0
	for _, b := range blobs {
		if b.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active blob, got %d", activeCount)
	}
}

// TestNewBlobIDIsHigherAfterReopen: after reopen, the first new blob created
// by rotation gets an ID strictly greater than any existing ID.
func TestNewBlobIDIsHigherAfterReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	m, err := logblob.Open(dir, logblob.Options{SizeCap: 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Write enough to create several blobs.
	for i := 0; i < 5; i++ {
		if _, err := m.Append(ctx, []byte("12345678901234567890")); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}
	blobsBefore, _ := m.ListBlobs()
	maxIDBefore := ""
	for _, b := range blobsBefore {
		if b.LogBlobID > maxIDBefore {
			maxIDBefore = b.LogBlobID
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen, force a rotation, check new ID > old max.
	m2, err := logblob.Open(dir, logblob.Options{SizeCap: 20})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()

	if err := m2.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	loc, err := m2.Append(ctx, []byte("new"))
	if err != nil {
		t.Fatalf("Append after reopen+rotate: %v", err)
	}
	if loc.LogBlobID <= maxIDBefore {
		t.Errorf("new blob ID %q ≤ max before reopen %q", loc.LogBlobID, maxIDBefore)
	}
}

// TestSyncFlushes: Sync() does not error on a healthy Manager.
func TestSyncFlushes(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	if _, err := m.Append(ctx, []byte("data")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := m.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// TestClosedManagerErrors: operations on a closed Manager return ErrClosed.
func TestClosedManagerErrors(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if _, err := m.Append(ctx, []byte("x")); err != nil {
		t.Fatalf("Append before Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := m.Append(ctx, []byte("y")); err == nil {
		t.Error("expected error from Append after Close, got nil")
	}
	loc := block.LocalChunkLocation{LogBlobID: "0000000000000000", RawOffset: 0, RawLength: 1}
	if _, err := m.ReadAt(ctx, loc, make([]byte, 1)); err == nil {
		t.Error("expected error from ReadAt after Close, got nil")
	}
	if err := m.Rotate(); err == nil {
		t.Error("expected error from Rotate after Close, got nil")
	}
	if err := m.Sync(); err == nil {
		t.Error("expected error from Sync after Close, got nil")
	}
}

// TestMultipleReopenCycles: open→write→close→reopen repeated several times;
// each cycle correctly resumes at the tail.
func TestMultipleReopenCycles(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	type record struct {
		loc  block.LocalChunkLocation
		data []byte
	}
	var records []record

	for cycle := 0; cycle < 4; cycle++ {
		m, err := logblob.Open(dir, logblob.Options{})
		if err != nil {
			t.Fatalf("cycle %d Open: %v", cycle, err)
		}

		p := []byte(fmt.Sprintf("cycle-%d-payload", cycle))
		loc, err := m.Append(ctx, p)
		if err != nil {
			_ = m.Close()
			t.Fatalf("cycle %d Append: %v", cycle, err)
		}
		records = append(records, record{loc, p})

		// Verify all previously written records are still readable.
		for _, r := range records {
			dst := make([]byte, r.loc.RawLength)
			if _, err := m.ReadAt(ctx, r.loc, dst); err != nil {
				_ = m.Close()
				t.Fatalf("cycle %d ReadAt: %v", cycle, err)
			}
			if !bytes.Equal(dst, r.data) {
				_ = m.Close()
				t.Fatalf("cycle %d ReadAt = %q, want %q", cycle, dst, r.data)
			}
		}

		if err := m.Close(); err != nil {
			t.Fatalf("cycle %d Close: %v", cycle, err)
		}
	}
}

// TestReadAtDuringRotate exercises the race window where fdForBlob snapshots
// the active fd under mu, releases mu, Rotate moves that fd to sealedFDs, and
// then ReadAt runs on the former-active fd. Every ReadAt must return the exact
// original bytes.
func TestReadAtDuringRotate(t *testing.T) {
	dir := t.TempDir()
	// Small SizeCap keeps the active blob small; we drive rotations manually too.
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	payload := []byte("readat-during-rotate-payload-data")
	loc, err := m.Append(ctx, payload)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	const (
		readers  = 8
		rotators = 4
		iters    = 50
	)

	var wg sync.WaitGroup

	// Readers: repeatedly read the same location and verify bytes.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, loc.RawLength)
			for i := 0; i < iters; i++ {
				n, err := m.ReadAt(ctx, loc, dst)
				if err != nil {
					t.Errorf("ReadAt during rotate: %v", err)
					return
				}
				if n != len(payload) {
					t.Errorf("ReadAt short: got %d, want %d", n, len(payload))
					return
				}
				if !bytes.Equal(dst[:n], payload) {
					t.Errorf("ReadAt corrupt: got %q, want %q", dst[:n], payload)
					return
				}
			}
		}()
	}

	// Rotators: keep sealing the active blob and opening a fresh one.
	for r := 0; r < rotators; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if err := m.Rotate(); err != nil {
					t.Errorf("Rotate: %v", err)
					return
				}
				// Write something so the new active blob isn't empty.
				if _, err := m.Append(ctx, []byte("filler")); err != nil {
					t.Errorf("Append filler: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
}

// TestReadAtDuringClose fires many concurrent ReadAt goroutines while another
// goroutine calls Close. Each ReadAt must return either correct bytes or an
// error (ErrClosed / os.ErrClosed / a wrapped variant). A nil error paired with
// wrong or garbage bytes is a bug.
func TestReadAtDuringClose(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := context.Background()
	payload := []byte("readat-during-close-payload-data")
	loc, err := m.Append(ctx, payload)
	if err != nil {
		_ = m.Close()
		t.Fatalf("Append: %v", err)
	}

	const readers = 16

	var wg sync.WaitGroup
	// Closer: let all readers start, then close.
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Brief yield so readers are in flight.
		runtime.Gosched()
		_ = m.Close()
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, loc.RawLength)
			n, err := m.ReadAt(ctx, loc, dst)
			if err != nil {
				// Any error is acceptable — closed sentinel or OS error on closed fd.
				return
			}
			// Success: bytes must be exactly right.
			if n != len(payload) || !bytes.Equal(dst[:n], payload) {
				t.Errorf("ReadAt during Close: nil err but wrong bytes: got %q (%d bytes), want %q", dst[:n], n, payload)
			}
		}()
	}

	wg.Wait()
}

// blobIDSet converts a BlobInfo slice to a set of IDs.
func blobIDSet(blobs []logblob.BlobInfo) map[string]bool {
	s := make(map[string]bool, len(blobs))
	for _, b := range blobs {
		s[b.LogBlobID] = true
	}
	return s
}

// TestEvictSealedBlob rotates to produce a sealed blob, evicts it, and
// verifies the .blob file is gone from disk.
func TestEvictSealedBlob(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()

	// First append → blob 0 (active).
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("A"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID

	// Second append → triggers rotation, blob 0 becomes sealed.
	if _, err := m.Append(ctx, []byte("second")); err != nil {
		t.Fatalf("Append(second): %v", err)
	}

	// Evict blob 0 (sealed).
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob: %v", err)
	}

	// File must be gone.
	blobPath := filepath.Join(dir, blob0+".blob")
	if _, statErr := os.Stat(blobPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("expected blob file to be gone, got stat err: %v", statErr)
	}
}

// TestReadAtEvictedReturnsErrEvicted evicts a sealed blob then expects
// ReadAt to return ErrEvicted.
func TestReadAtEvictedReturnsErrEvicted(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("B"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, []byte("seal-trigger")); err != nil {
		t.Fatalf("Append(trigger): %v", err)
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

// TestEvictActiveBlobRefused verifies that EvictBlob returns ErrActiveBlob
// when called on the currently active blob.
func TestEvictActiveBlobRefused(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc, err := m.Append(ctx, []byte("active-data"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	evictErr := m.EvictBlob(ctx, loc.LogBlobID, func(string) bool { return true })
	if !errors.Is(evictErr, logblob.ErrActiveBlob) {
		t.Errorf("EvictBlob(active): got %v, want ErrActiveBlob", evictErr)
	}
}

// TestEvictUnsyncedBytesRefused verifies that a synced function returning
// false causes EvictBlob to return ErrUnsyncedBytes.
func TestEvictUnsyncedBytesRefused(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("C"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, []byte("seal")); err != nil {
		t.Fatalf("Append(seal): %v", err)
	}

	evictErr := m.EvictBlob(ctx, blob0, func(string) bool { return false })
	if !errors.Is(evictErr, logblob.ErrUnsyncedBytes) {
		t.Errorf("EvictBlob(unsynced): got %v, want ErrUnsyncedBytes", evictErr)
	}
}

// TestEvictIdempotent verifies that calling EvictBlob twice on the same
// sealed blob returns nil on the second call.
func TestEvictIdempotent(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("D"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, []byte("seal")); err != nil {
		t.Fatalf("Append(seal): %v", err)
	}

	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob(first): %v", err)
	}
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Errorf("EvictBlob(second, idempotent): got %v, want nil", err)
	}
}

// TestEvictRaceDuringReadAt exercises concurrent ReadAt goroutines against an
// EvictBlob call on the same sealed blob. A nil error from ReadAt must be
// paired with correct bytes; the race detector validates locking.
func TestEvictRaceDuringReadAt(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("E"), 70)
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc0, err := m.Append(ctx, payload)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := m.Append(ctx, []byte("seal")); err != nil {
		t.Fatalf("Append(seal): %v", err)
	}

	var wg sync.WaitGroup
	const readers = 8
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, loc0.RawLength)
			n, err := m.ReadAt(ctx, loc0, dst)
			if err != nil {
				// Eviction may have raced: any error is acceptable.
				return
			}
			if n != len(payload) || !bytes.Equal(dst[:n], payload) {
				t.Errorf("ReadAt during evict race: nil err but wrong bytes: got %q, want %q", dst[:n], payload)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = m.EvictBlob(ctx, loc0.LogBlobID, func(string) bool { return true })
	}()

	wg.Wait()
}

// TestRecoverActiveBlobTornTail writes 3 payloads, injects garbage beyond
// them, calls Recover to the valid offset, then verifies that subsequent
// Append lands at that offset and earlier reads are intact.
func TestRecoverActiveBlobTornTail(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	payloads := [][]byte{
		[]byte("payload-one"),
		[]byte("payload-two"),
		[]byte("payload-three"),
	}
	locs := make([]block.LocalChunkLocation, 3)
	for i, p := range payloads {
		locs[i], err = m.Append(ctx, p)
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// mark is the valid data boundary.
	mark := locs[2].RawOffset + locs[2].RawLength

	// Inject garbage after the mark directly into the blob file.
	blobPath := filepath.Join(dir, locs[0].LogBlobID+".blob")
	f, openErr := os.OpenFile(blobPath, os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatalf("OpenFile: %v", openErr)
	}
	if _, werr := f.WriteAt([]byte("GARBAGE!!"), mark); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteAt garbage: %v", werr)
	}
	_ = f.Close()

	// Recover to mark.
	if err := m.Recover(ctx, locs[0].LogBlobID, mark); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Next Append must land at mark.
	afterLoc, err := m.Append(ctx, []byte("after"))
	if err != nil {
		t.Fatalf("Append(after): %v", err)
	}
	if afterLoc.RawOffset != mark {
		t.Errorf("Append after Recover: RawOffset = %d, want %d", afterLoc.RawOffset, mark)
	}

	// Original payloads still readable.
	for i, loc := range locs {
		dst := make([]byte, loc.RawLength)
		if _, err := m.ReadAt(ctx, loc, dst); err != nil {
			t.Fatalf("ReadAt[%d]: %v", i, err)
		}
		if !bytes.Equal(dst, payloads[i]) {
			t.Errorf("ReadAt[%d] = %q, want %q", i, dst, payloads[i])
		}
	}

	// Reading at mark+5 (inside the former garbage) should fail (file truncated).
	garbageLoc := block.LocalChunkLocation{
		LogBlobID: locs[0].LogBlobID,
		RawOffset: mark + 5,
		RawLength: 5,
	}
	dst := make([]byte, 5)
	_, readErr := m.ReadAt(ctx, garbageLoc, dst)
	if readErr == nil {
		t.Errorf("ReadAt beyond Recover boundary: expected error (EOF/short), got nil")
	}
}

// TestRecoverSealedBlobTornTail appends 2 payloads, seals the blob, injects
// garbage after the payloads, calls Recover, then verifies reads before the
// offset succeed and the file size matches the offset.
func TestRecoverSealedBlobTornTail(t *testing.T) {
	dir := t.TempDir()
	payload1 := bytes.Repeat([]byte("F"), 50)
	payload2 := bytes.Repeat([]byte("G"), 50)

	m, err := logblob.Open(dir, logblob.Options{SizeCap: 200})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc1, err := m.Append(ctx, payload1)
	if err != nil {
		t.Fatalf("Append(1): %v", err)
	}
	loc2, err := m.Append(ctx, payload2)
	if err != nil {
		t.Fatalf("Append(2): %v", err)
	}
	blob0 := loc1.LogBlobID
	validOffset := loc2.RawOffset + loc2.RawLength // = 100

	// Seal blob0 by rotating.
	if err := m.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Inject garbage directly past validOffset.
	blobPath := filepath.Join(dir, blob0+".blob")
	f, openErr := os.OpenFile(blobPath, os.O_WRONLY, 0)
	if openErr != nil {
		t.Fatalf("OpenFile: %v", openErr)
	}
	garbage := bytes.Repeat([]byte("X"), 30)
	if _, werr := f.WriteAt(garbage, validOffset); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteAt garbage: %v", werr)
	}
	_ = f.Close()

	// Recover sealed blob.
	if err := m.Recover(ctx, blob0, validOffset); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Reads before offset must succeed.
	dst1 := make([]byte, loc1.RawLength)
	if _, err := m.ReadAt(ctx, loc1, dst1); err != nil {
		t.Fatalf("ReadAt(loc1): %v", err)
	}
	if !bytes.Equal(dst1, payload1) {
		t.Errorf("ReadAt(loc1) = %q, want %q", dst1, payload1)
	}
	dst2 := make([]byte, loc2.RawLength)
	if _, err := m.ReadAt(ctx, loc2, dst2); err != nil {
		t.Fatalf("ReadAt(loc2): %v", err)
	}
	if !bytes.Equal(dst2, payload2) {
		t.Errorf("ReadAt(loc2) = %q, want %q", dst2, payload2)
	}

	// File size must equal validOffset.
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if fi.Size() != validOffset {
		t.Errorf("blob file size = %d, want %d", fi.Size(), validOffset)
	}
}

// TestRecoverAckedBoundary verifies the acked/unacked boundary: data written
// before the mark survives Recover, data written after is gone.
func TestRecoverAckedBoundary(t *testing.T) {
	dir := t.TempDir()
	acked := bytes.Repeat([]byte("H"), 50)
	unacked := bytes.Repeat([]byte("I"), 50)

	m, err := logblob.Open(dir, logblob.Options{SizeCap: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	locAcked, err := m.Append(ctx, acked)
	if err != nil {
		t.Fatalf("Append(acked): %v", err)
	}
	mark := locAcked.RawOffset + locAcked.RawLength // = 50

	locUnacked, err := m.Append(ctx, unacked)
	if err != nil {
		t.Fatalf("Append(unacked): %v", err)
	}

	// Recover to mark; discards the "unacked" portion.
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

	// Unacked loc is now beyond the truncation point: reading must fail.
	dstU := make([]byte, locUnacked.RawLength)
	n, readErr := m.ReadAt(ctx, locUnacked, dstU)
	if readErr == nil && n == len(unacked) {
		t.Error("ReadAt(unacked) after Recover: expected error or short read, got full success")
	}

	// Next Append must resume at mark.
	postLoc, err := m.Append(ctx, []byte("post"))
	if err != nil {
		t.Fatalf("Append(post): %v", err)
	}
	if postLoc.RawOffset != mark {
		t.Errorf("Append after Recover: RawOffset = %d, want %d", postLoc.RawOffset, mark)
	}
}

// TestEvictIdempotentUnsyncedSecondCall verifies that EvictBlob is idempotent
// even when the synced function returns false on the second call. Once a blob
// has been evicted the synced check must not be re-evaluated — the eviction
// record is authoritative.
func TestEvictIdempotentUnsyncedSecondCall(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("D"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, []byte("seal")); err != nil {
		t.Fatalf("Append(seal): %v", err)
	}

	// First eviction: synced=true, must succeed.
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob(first): %v", err)
	}
	// Second eviction with synced=false: blob is already evicted, must return nil.
	if err := m.EvictBlob(ctx, blob0, func(string) bool { return false }); err != nil {
		t.Errorf("EvictBlob(second, unsynced but already evicted): got %v, want nil", err)
	}
}

// TestRecoverActiveOverrunRejected verifies that Recover returns an error and
// does NOT extend the active blob when validUpToOffset exceeds the blob's
// current size (POSIX ftruncate would zero-fill if we let it through).
func TestRecoverActiveOverrunRejected(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	data := []byte("hello")
	loc, err := m.Append(ctx, data)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	currentSize := loc.RawOffset + loc.RawLength // = 5
	overrunOffset := currentSize + 100

	// Recover with offset > size must return an error.
	if recoverErr := m.Recover(ctx, loc.LogBlobID, overrunOffset); recoverErr == nil {
		t.Fatal("Recover(active) with offset > blob size: expected error, got nil")
	}

	// File must not have been extended.
	blobPath := filepath.Join(dir, loc.LogBlobID+".blob")
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if fi.Size() != currentSize {
		t.Errorf("Recover(active) extended file: size = %d, want %d", fi.Size(), currentSize)
	}
}

// TestRecoverSealedOverrunRejected verifies that Recover returns an error and
// does NOT extend a sealed blob when validUpToOffset exceeds the file size.
func TestRecoverSealedOverrunRejected(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 200})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	data := bytes.Repeat([]byte("Z"), 50)
	loc, err := m.Append(ctx, data)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc.LogBlobID
	currentSize := loc.RawOffset + loc.RawLength // = 50

	// Seal blob0.
	if err := m.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	overrunOffset := currentSize + 100

	// Recover with offset > size must return an error.
	if recoverErr := m.Recover(ctx, blob0, overrunOffset); recoverErr == nil {
		t.Fatal("Recover(sealed) with offset > blob size: expected error, got nil")
	}

	// File must not have been extended.
	blobPath := filepath.Join(dir, blob0+".blob")
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if fi.Size() != currentSize {
		t.Errorf("Recover(sealed) extended file: size = %d, want %d", fi.Size(), currentSize)
	}
}

// TestReadAtZeroLengthAfterClose verifies that a zero-length ReadAt on a
// closed Manager returns ErrClosed rather than silently succeeding.
func TestReadAtZeroLengthAfterClose(t *testing.T) {
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if _, err := m.Append(ctx, []byte("data")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	loc := block.LocalChunkLocation{LogBlobID: "0000000000000000", RawOffset: 0, RawLength: 0}
	_, readErr := m.ReadAt(ctx, loc, nil)
	if !errors.Is(readErr, logblob.ErrClosed) {
		t.Errorf("ReadAt(zero-length) on closed Manager: got %v, want ErrClosed", readErr)
	}
}

// TestEvictBlobRemoveFailure verifies that EvictBlob correctly handles a
// failed os.Remove: the blob is permanently marked evicted (ReadAt → ErrEvicted)
// and the error is returned to the caller so it knows the file is an orphan.
func TestEvictBlobRemoveFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot revoke directory write permission")
	}
	dir := t.TempDir()
	m, err := logblob.Open(dir, logblob.Options{SizeCap: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := context.Background()
	loc0, err := m.Append(ctx, bytes.Repeat([]byte("F"), 70))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	blob0 := loc0.LogBlobID
	if _, err := m.Append(ctx, []byte("seal")); err != nil {
		t.Fatalf("Append(seal): %v", err)
	}

	// Make the directory read-only so os.Remove cannot delete the blob file.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() {
		// Restore permissions so t.TempDir() cleanup succeeds.
		_ = os.Chmod(dir, 0o755)
		_ = m.Close()
	}()

	// EvictBlob must return an error because the unlink fails.
	evictErr := m.EvictBlob(ctx, blob0, func(string) bool { return true })
	if evictErr == nil {
		t.Fatal("EvictBlob: expected error when os.Remove fails, got nil")
	}

	// Despite the unlink failure, the blob must be permanently evicted so
	// ReadAt returns ErrEvicted — not stale bytes from the still-present file.
	dst := make([]byte, loc0.RawLength)
	_, readErr := m.ReadAt(ctx, loc0, dst)
	if !errors.Is(readErr, logblob.ErrEvicted) {
		t.Errorf("ReadAt after failed eviction: got %v, want ErrEvicted", readErr)
	}
}

// TestLogBlobConformance runs the logblob conformance suite.
func TestLogBlobConformance(t *testing.T) {
	blockstoretest.LogBlobConformance(t, func(t *testing.T) (*logblob.Manager, string, func()) {
		t.Helper()
		dir := t.TempDir()
		m, err := logblob.Open(dir, logblob.Options{SizeCap: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return m, dir, func() { _ = m.Close() }
	})
}
