package journal

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable Clock for the age-gate tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// recRawFlags reads the on-disk Flags byte of the record covering fileOff.
func recRawFlags(t *testing.T, s *Store, id FileID, fileOff int64) uint8 {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		t.Fatalf("no index for %q", id)
	}
	for _, iv := range fi.ivs {
		if iv.fileOff <= fileOff && fileOff < iv.end() {
			seg := sh.segment(iv.loc.SegmentID)
			var b [1]byte
			if _, err := seg.fd.ReadAt(b[:], iv.recOff+recordFlagsOffset); err != nil {
				t.Fatalf("read flags: %v", err)
			}
			return b[0]
		}
	}
	t.Fatalf("no interval covering offset %d", fileOff)
	return 0
}

// forceDirty simulates a crash between commit and flip: the records go back to
// synced=false (on disk and in memory) while the deduper keeps the committed
// hashes, so a re-flush must dedup to a no-op.
func forceDirty(t *testing.T, s *Store, id FileID) {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	for k := range fi.ivs {
		iv := fi.ivs[k]
		seg := sh.segment(iv.loc.SegmentID)
		if _, err := seg.fd.WriteAt([]byte{0}, iv.recOff+recordFlagsOffset); err != nil {
			t.Fatalf("clear flag: %v", err)
		}
		if fi.ivs[k].synced {
			fi.ivs[k].synced = false
			s.unsynced.Add(fi.ivs[k].length)
		}
	}
	fi.firstDirtyNanos = s.clock.Now().UnixNano()
}

// writeRunAt writes n distinct 4 KiB appends starting at off, so the records
// form contiguous live ranges of 4 KiB each.
func writeRunAt(t *testing.T, s *Store, off int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.WriteAt(context.Background(), "f", off+int64(i)*(4<<10), randBytes(4<<10, off+int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", off+int64(i)*(4<<10), err)
		}
	}
}

// writeAdjacent writes nWrites distinct chunkBytes-sized appends contiguously
// from offset 0 and returns the full plaintext, so the file forms one
// contiguous dirty run.
func writeAdjacent(t *testing.T, s *Store, id FileID, nWrites, chunkBytes int) []byte {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, 0, nWrites*chunkBytes)
	for i := 0; i < nWrites; i++ {
		b := randBytes(chunkBytes, int64(i)+1)
		if err := s.WriteAt(ctx, id, int64(i*chunkBytes), b); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		data = append(data, b...)
	}
	return data
}
