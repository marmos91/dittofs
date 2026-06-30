package logblob_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
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

// blobIDSet converts a BlobInfo slice to a set of IDs.
func blobIDSet(blobs []logblob.BlobInfo) map[string]bool {
	s := make(map[string]bool, len(blobs))
	for _, b := range blobs {
		s[b.LogBlobID] = true
	}
	return s
}
