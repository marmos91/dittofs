// Package memory is a pure in-memory local.LocalStore used by tests and
// ephemeral configs. It is a per-file byte cache (FileID+offset keyed),
// mirroring the journal's shape without disk, segments, or eviction.
package memory

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
)

var (
	_ local.LocalStore         = (*MemoryStore)(nil)
	_ block.DurabilityReporter = (*MemoryStore)(nil)
)

// memFile is one file's byte buffer, its not-yet-carved byte count, and the
// ranges of it that were actually written.
//
// The buffer alone cannot answer which bytes are real: it zero-fills the gap
// when a write lands past its end, so a never-written range is byte-identical
// to one written with zeros. written records the difference, sorted and
// non-overlapping, which is what lets DataExtents describe an interior hole
// instead of claiming one unbroken span from zero.
type memFile struct {
	buf      []byte
	unsynced int64
	written  [][2]int64
}

// addWritten records [start, end) as written, coalescing it with any range it
// overlaps or abuts so the slice stays sorted and non-overlapping.
func (f *memFile) addWritten(start, end int64) {
	if end <= start {
		return
	}
	out := make([][2]int64, 0, len(f.written)+1)
	i := 0
	for ; i < len(f.written) && f.written[i][1] < start; i++ {
		out = append(out, f.written[i])
	}
	for ; i < len(f.written) && f.written[i][0] <= end; i++ {
		start = min(start, f.written[i][0])
		end = max(end, f.written[i][1])
	}
	out = append(out, [2]int64{start, end})
	f.written = append(out, f.written[i:]...)
}

// clipWritten drops the recorded ranges past newSize and trims a straddling
// one, keeping the record consistent with the shortened buffer.
func (f *memFile) clipWritten(newSize int64) {
	out := f.written[:0]
	for _, e := range f.written {
		if e[0] >= newSize {
			continue
		}
		out = append(out, [2]int64{e[0], min(e[1], newSize)})
	}
	f.written = out
}

// MemoryStore is a pure in-memory implementation of local.LocalStore.
type MemoryStore struct {
	mu    sync.RWMutex
	files map[string]*memFile

	unsynced atomic.Int64
	durable  atomic.Bool
	closed   bool
}

// New creates an empty MemoryStore.
func New() *MemoryStore {
	return &MemoryStore{files: make(map[string]*memFile)}
}

// writeLocked copies data into the file's buffer at offset, growing (zero-filling
// gaps) as needed, and returns the number of freshly-written bytes.
func (s *MemoryStore) writeLocked(payloadID string, offset int64, data []byte) int64 {
	f := s.files[payloadID]
	if f == nil {
		f = &memFile{}
		s.files[payloadID] = f
	}
	end := offset + int64(len(data))
	if int64(len(f.buf)) < end {
		// Extend through append so the runtime's geometric growth amortizes the
		// copy. Allocating exactly `end` each time would re-copy the whole buffer
		// on every extending write, making a sequential file O(n^2) to fill. The
		// appended bytes are zero, which keeps the gap-filling semantics.
		f.buf = append(f.buf, make([]byte, end-int64(len(f.buf)))...)
	}
	copy(f.buf[offset:end], data)
	f.addWritten(offset, end)
	return int64(len(data))
}

// WriteAt buffers a dirty write.
func (s *MemoryStore) WriteAt(_ context.Context, id journal.FileID, offset int64, data []byte) error {
	payloadID := string(id)
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	n := s.writeLocked(payloadID, offset, data)
	s.files[payloadID].unsynced += n
	s.unsynced.Add(n)
	return nil
}

// Hydrate writes remote-fetched bytes; born clean, so no unsynced charge.
//
// ponytail: notAfter is accepted and ignored — the per-file record tracks which
// ranges were written but stamps no version on them, so there is nothing to
// compare it against, and this store never evicts, so the cold read that
// carries a meaningful mark does not arise here. Version the recorded ranges if
// a memory-local share ever needs the gate.
func (s *MemoryStore) Hydrate(_ context.Context, id journal.FileID, offset int64, data []byte, _ uint64) error {
	payloadID := string(id)
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	s.writeLocked(payloadID, offset, data)
	return nil
}

// ReadAt copies bytes into dst; never-written ranges are zero-filled holes.
// Memory never evicts, so Cold is always false.
//
// Hole is derived from the written-range record rather than the buffer's
// length, which is what the interface's rule requires: a byte this store was
// never given must not be claimed as one it holds. The buffer zero-fills the
// gap when a write lands past its end, so a never-written range inside it is
// byte-identical to one written with zeros and its length alone cannot tell
// them apart. The record can, and it is the same record DataExtents answers
// from, so the two views agree about any given byte.
func (s *MemoryStore) ReadAt(_ context.Context, id journal.FileID, offset int64, dst []byte) (int, journal.ReadState, error) {
	payloadID := string(id)
	if offset < 0 {
		return 0, journal.ReadState{}, block.ErrInvalidOffset
	}
	if len(dst) == 0 {
		return 0, journal.ReadState{}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, journal.ReadState{}, block.ErrStoreClosed
	}
	f := s.files[payloadID]
	if f == nil || offset >= int64(len(f.buf)) {
		clear(dst)
		return len(dst), journal.ReadState{Hole: true}, nil
	}
	// Zero only the tail the copy does not cover; pre-clearing the whole of dst
	// would write the copied prefix twice.
	n := copy(dst, f.buf[offset:])
	if n < len(dst) {
		clear(dst[n:])
	}
	// Any byte of the window the record does not cover is a hole, whether it
	// sits past the buffer's end or between two writes inside it.
	end := offset + int64(len(dst))
	var covered int64
	for _, e := range f.written {
		if e[1] <= offset {
			continue
		}
		if e[0] >= end {
			break
		}
		covered += min(e[1], end) - max(e[0], offset)
	}
	return len(dst), journal.ReadState{Hole: covered < int64(len(dst))}, nil
}

// Commit is a no-op: memory has no durable substrate.
func (s *MemoryStore) Commit(context.Context, journal.FileID) error { return nil }

// FileSize reports the data high-water mark.
func (s *MemoryStore) FileSize(_ context.Context, id journal.FileID) (int64, bool) {
	payloadID := string(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.files[payloadID]
	if f == nil {
		return 0, false
	}
	return int64(len(f.buf)), true
}

// DataExtents returns the ranges actually written, clamped to fileSize.
//
// It reports coverage rather than the buffer's span because its two consumers
// want opposite kinds of caution. SEEK/READ_PLUS tolerates naming data where
// there is a hole. The offline-readiness cross-check does not: it subtracts
// this from what the manifest places and treats the remainder as the share's
// shortfall, so an extent claiming an unwritten range makes a share holding
// zeros read as provably offline-safe. Describing only what was written is the
// answer that is safe for both.
func (s *MemoryStore) DataExtents(_ context.Context, id journal.FileID, fileSize int64) ([][2]uint64, error) {
	payloadID := string(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.files[payloadID]
	if f == nil || fileSize <= 0 {
		return nil, nil
	}
	var out [][2]uint64
	for _, e := range f.written {
		if e[0] >= fileSize {
			break
		}
		if end := min(e[1], fileSize); end > e[0] {
			out = append(out, [2]uint64{uint64(e[0]), uint64(end)})
		}
	}
	return out, nil
}

// Truncate shrinks a file to newSize; growing is a no-op.
func (s *MemoryStore) Truncate(_ context.Context, id journal.FileID, newSize int64) error {
	payloadID := string(id)
	if newSize < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[payloadID]
	if f == nil || int64(len(f.buf)) <= newSize {
		return nil
	}
	f.buf = f.buf[:newSize]
	f.clipWritten(newSize)
	return nil
}

// Delete drops all of a file's cached ranges.
func (s *MemoryStore) Delete(_ context.Context, id journal.FileID) error {
	payloadID := string(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[payloadID]; f != nil {
		s.unsynced.Add(-f.unsynced)
		delete(s.files, payloadID)
	}
	return nil
}

// ListFiles returns every payloadID with local data.
func (s *MemoryStore) ListFiles(context.Context) []journal.FileID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]journal.FileID, 0, len(s.files))
	for id := range s.files {
		out = append(out, journal.FileID(id))
	}
	return out
}

// Flush offers each dirty file's bytes to fn as one run and flips what it
// reports durable. id scopes it to one file; the empty id is not special —
// callers enumerate ListFiles per id, matching the journal implementation.
func (s *MemoryStore) Flush(ctx context.Context, id journal.FileID, opts journal.FlushOptions, fn journal.FlushFunc) (err error) {
	if fn == nil {
		return errors.New("memory: Flush requires fn")
	}
	s.mu.RLock()
	var ids []string
	if f := s.files[string(id)]; f != nil && f.unsynced > 0 {
		ids = []string{string(id)}
	}
	s.mu.RUnlock()
	if len(ids) == 0 {
		return nil
	}

	var firstErr error
	for _, fid := range ids {
		s.mu.RLock()
		f := s.files[fid]
		var data []byte
		if f != nil {
			data = append([]byte(nil), f.buf...)
		}
		s.mu.RUnlock()
		if len(data) == 0 {
			continue
		}
		// Offer the whole dirty file as one run and flip whatever fn reports
		// durable. A failure still credits the committed prefix (the C5 shape:
		// durable extents ride the error), so a retry never re-offers bytes the
		// sink already took.
		extents, ferr := fn(ctx, journal.Run{
			ID:       journal.FileID(fid),
			Extent:   journal.Extent{Off: 0, Len: int64(len(data)), State: journal.StateDirty},
			Final:    true,
			ReaderAt: bytes.NewReader(data),
		})
		for _, e := range extents {
			if end := e.Off + e.Len; end <= int64(len(data)) {
				s.markCarvedRange(fid, e.Off, end)
			}
		}
		if ferr != nil && firstErr == nil {
			firstErr = ferr
		}
	}
	return firstErr
}

// markCarvedRange clears the file's unsynced charge over [off, end).
func (s *MemoryStore) markCarvedRange(fid string, off, end int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[fid]
	if f == nil {
		return
	}
	n := end - off
	if n > f.unsynced {
		n = f.unsynced
	}
	if n > 0 {
		f.unsynced -= n
		s.unsynced.Add(-n)
	}
}

// UnsyncedBytes reports bytes not yet carved to the sink.
func (s *MemoryStore) UnsyncedBytes() int64 {
	if v := s.unsynced.Load(); v > 0 {
		return v
	}
	return 0
}

// Evict is a no-op: memory never evicts.
func (s *MemoryStore) Evict(context.Context, int64) (journal.EvictResult, error) {
	return journal.EvictResult{}, nil
}

// SetEvictionEnabled is a no-op.
func (s *MemoryStore) SetEvictionEnabled(bool) {}

// SetEvictionPinned is a no-op.
func (s *MemoryStore) SetEvictionPinned(bool) {}

// Start is a no-op.
func (s *MemoryStore) Start(context.Context) {}

// Closed reports whether the store has been closed and is no longer accepting
// reads or writes — the only failure mode a pure in-memory store has. Cheap,
// lock-protected, safe for concurrent calls.
func (s *MemoryStore) Closed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Close marks the store closed.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Stats reports coarse in-memory usage.
func (s *MemoryStore) Stats() journal.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var used int64
	for _, f := range s.files {
		used += int64(len(f.buf))
	}
	return journal.Stats{DiskBytes: used}
}

// FileCount reports the number of files with a local entry.
func (s *MemoryStore) FileCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.files)
}

// Durable reports crash survival. In-memory storage is volatile → false default.
func (s *MemoryStore) Durable() bool { return s.durable.Load() }

// SetDurable overrides the durability report.
func (s *MemoryStore) SetDurable(v bool) { s.durable.Store(v) }

// WriteVersion reports zero: this store keeps no write history, so Hydrate's
// gate stays disabled.
func (s *MemoryStore) WriteVersion() uint64 { return 0 }

// Invalidate is a no-op: the memory store has no durable tier to demote and no
// remote copy to fall back to, so there is nothing a read could fetch instead.
func (s *MemoryStore) Invalidate(_ context.Context, _ journal.FileID, _, _ int64) error { return nil }

// MaxLocalBytes reports the store's disk cap. It has none: the buffers grow in
// the heap until the process runs out of it, which 0 (uncapped) states.
func (s *MemoryStore) MaxLocalBytes() int64 { return 0 }

// ColdExtents reports no remote-only ranges. Nothing here ever demotes bytes to
// the remote tier — Evict frees nothing — so every byte the store has been
// handed is still in its buffer and none has to be fetched back to serve.
func (s *MemoryStore) ColdExtents(context.Context) (int64, int64, error) { return 0, 0, nil }

// ColdSeeded reports that the store needs no seeding.
//
// decision: seeding exists so a tier that was restarted against an existing
// manifest knows which ranges live only on the remote, and this store has no
// such ranges: it never evicts, so a range it describes is a range it holds and
// a range it does not describe was never written to it. What it has forgotten
// across a restart — everything — is caught by the caller's manifest
// cross-check rather than by this flag, which only guards the cold tally.
// Withdraw this the moment the store grows eviction or any other way to hold a
// range it cannot serve.
func (s *MemoryStore) ColdSeeded() bool { return true }

// UploadConcurrency and BlockSize report no preference: the store does not size
// its own flushes (Flush hands whole dirty runs to the caller's fn without
// framing or uploading them), so the caller's defaults apply.
func (s *MemoryStore) UploadConcurrency() int { return 0 }
func (s *MemoryStore) BlockSize() int64       { return 0 }

// JournalVersion reports no watermark. The store keeps no log: a write lands in
// the buffer in place and leaves no record behind it, so there is no point in
// time to number. Same reason WriteVersion reports 0.
func (s *MemoryStore) JournalVersion() uint64 { return 0 }

// SetPinVersion does nothing.
//
// decision: a pin keeps reclamation from dropping the bytes a live snapshot
// still needs, and nothing here reclaims — Evict frees nothing and there is no
// GC — so the bytes a pin would protect are already safe for as long as the
// process lives. Withdraw this if the store ever frees a buffer it was not
// asked to delete.
func (s *MemoryStore) SetPinVersion(uint64) {}

// RestoreToVersion does nothing and reports success.
//
// decision: the store holds exactly one view of each file — the current one —
// because writes overwrite in place and no prior version is kept, so the view
// v names either is that one or was never recorded. Rewinding to the only view
// there is is what doing nothing achieves. This is why the restore
// orchestration reaches for it only on a share whose local tier is the sole
// durable copy of the bytes, which this store is never configured to be.
// Withdraw this if the store ever versions its buffers.
func (s *MemoryStore) RestoreToVersion(context.Context, uint64) error { return nil }

// DurableExtent declines to answer.
//
// decision: this store's durability is a configured claim (SetDurable), not a
// property of a substrate it writes to, so it cannot map any byte to a stable
// copy — including when the claim is true. ok=false is the interface's
// "unknown", which callers must not read as "nothing is durable"; answering
// (0, true) instead would publish a zero durable size for a store a test has
// deliberately declared durable. Withdraw this if the store ever gains a
// backing file whose fsync it can observe.
func (s *MemoryStore) DurableExtent(context.Context, journal.FileID) (int64, bool) {
	return 0, false
}

// SetVerifyReads does nothing.
//
// decision: verification re-checks a resident byte against the checksum the
// tier recorded beside it, and this store records none — ReadAt copies the
// buffer back verbatim, so a read cannot disagree with anything. There is no
// fast path to opt into and no corruption for the slow one to catch. Withdraw
// this if reads ever pass through an encoding that could fail.
func (s *MemoryStore) SetVerifyReads(bool) {}

// SeedCold and SeedColdBatch record nothing and report success.
//
// decision: a cold marker says "the remote holds these bytes and this tier does
// not", and this store has no way to describe a range it does not hold — an
// entry here IS its bytes, so there is no marker to attach and a read of an
// unseeded range already falls through as the hole it is. Recording the seed
// would be indistinguishable from discarding it. Withdraw this if the store
// ever grows an interval index that can describe a range without its bytes,
// at which point it must hold the markers or reads of those ranges zero-fill.
func (s *MemoryStore) SeedCold(context.Context, journal.FileID, [][2]int64) error { return nil }
func (s *MemoryStore) SeedColdBatch(context.Context, []journal.ColdSeed) error    { return nil }
