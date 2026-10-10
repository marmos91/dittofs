// Package memory is a pure in-memory local.LocalStore used by tests and
// ephemeral configs. It is a per-file byte cache (FileID+offset keyed),
// mirroring the journal's shape without disk, segments, or eviction.
package memory

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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
	dirty    [][2]int64
	version  uint64
}

// addWritten records [start, end) as written, coalescing it with any range it
// overlaps or abuts so the slice stays sorted and non-overlapping.
func (f *memFile) addWritten(start, end int64) {
	f.written = addRange(f.written, start, end)
}

func addRange(ranges [][2]int64, start, end int64) [][2]int64 {
	if end <= start {
		return ranges
	}
	out := make([][2]int64, 0, len(ranges)+1)
	i := 0
	for ; i < len(ranges) && ranges[i][1] < start; i++ {
		out = append(out, ranges[i])
	}
	for ; i < len(ranges) && ranges[i][0] <= end; i++ {
		start = min(start, ranges[i][0])
		end = max(end, ranges[i][1])
	}
	out = append(out, [2]int64{start, end})
	return append(out, ranges[i:]...)
}

// clipWritten drops the recorded ranges past newSize and trims a straddling
// one, keeping the record consistent with the shortened buffer.
func (f *memFile) clipWritten(newSize int64) {
	f.written = clipRanges(f.written, newSize)
}

func clipRanges(ranges [][2]int64, newSize int64) [][2]int64 {
	out := ranges[:0]
	for _, e := range ranges {
		if e[0] >= newSize {
			continue
		}
		out = append(out, [2]int64{e[0], min(e[1], newSize)})
	}
	return out
}

func rangeBytes(ranges [][2]int64) int64 {
	var n int64
	for _, r := range ranges {
		n += r[1] - r[0]
	}
	return n
}

type fenceEntry struct {
	id       string
	version  uint64
	survives int64
}

const maxHydrateFences = 1024

// ErrHydrateHistoryExpired means a fetch predates the retained mutation
// history. Refusing the fill avoids returning holes as successful zero data;
// retrying the read or warm operation samples a current WriteVersion.
var ErrHydrateHistoryExpired = errors.New("memory: hydrate plan predates retained mutation history")

// MemoryStore is a pure in-memory implementation of local.LocalStore.
type MemoryStore struct {
	mu    sync.RWMutex
	files map[string]*memFile
	// Flush locks belong to file IDs, not memFile instances, so deleting and
	// recreating a file cannot let its new pass overtake an older callback.
	flushLocks [64]sync.Mutex
	// These outlive a file's buffer: a delayed fetch must not fill holes that
	// truncate or delete created, including after that file ID is reused.
	fences     map[string]uint64
	fenceOrder []fenceEntry
	fenceFloor uint64
	flushing   map[string]bool

	unsynced atomic.Int64
	version  atomic.Uint64
	durable  atomic.Bool
	closed   bool
}

// New creates an empty MemoryStore.
func New() *MemoryStore {
	return &MemoryStore{
		files: make(map[string]*memFile), fences: make(map[string]uint64), flushing: make(map[string]bool),
	}
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
	f.version = s.version.Add(1)
	return int64(len(data))
}

// WriteAt buffers a dirty write.
func (s *MemoryStore) WriteAt(ctx context.Context, id journal.FileID, offset int64, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payloadID := string(id)
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	if int64(len(data)) > math.MaxInt64-offset {
		return block.ErrInvalidSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	if len(data) == 0 {
		return nil
	}
	s.writeLocked(payloadID, offset, data)
	f := s.files[payloadID]
	f.dirty = addRange(f.dirty, offset, offset+int64(len(data)))
	s.updateDirty(f)
	return nil
}

// Hydrate fills only absent ranges. Resident bytes are authoritative even when
// the fetch carries the current version, and retained mutation fences reject
// older fills of holes left by truncate or delete. Filled bytes are born clean.
func (s *MemoryStore) Hydrate(ctx context.Context, id journal.FileID, offset int64, data []byte, notAfter uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payloadID := string(id)
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	if int64(len(data)) > math.MaxInt64-offset {
		return block.ErrInvalidSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	end := offset + int64(len(data))
	if len(data) == 0 {
		return nil
	}
	if f := s.files[payloadID]; f != nil && coversRange(f.written, offset, end) {
		return nil // A no-op refill needs no retained mutation history.
	}
	if notAfter < s.fenceFloor {
		return ErrHydrateHistoryExpired
	}
	if notAfter < s.fences[payloadID] {
		// Keep every prefix that survived mutations after this plan. Dropping a
		// straddling fill whole would turn still-valid remote prefix bytes into
		// zero-filled holes. Older fences do not constrain a newer plan.
		for _, fence := range s.fenceOrder {
			if fence.id == payloadID && fence.version > notAfter {
				end = min(end, fence.survives)
			}
		}
	}
	if end <= offset {
		return nil
	}
	// Plan against the same locked ranges that the fill updates. A concurrent
	// writer cannot land between choosing a gap and copying remote bytes into it.
	var gaps [][2]int64
	cursor := offset
	if f := s.files[payloadID]; f != nil {
		for _, r := range f.written {
			if r[1] <= cursor {
				continue
			}
			if r[0] >= end {
				break
			}
			if r[0] > cursor {
				gaps = append(gaps, [2]int64{cursor, r[0]})
			}
			cursor = max(cursor, r[1])
		}
	}
	if cursor < end {
		gaps = append(gaps, [2]int64{cursor, end})
	}
	for _, gap := range gaps {
		s.writeLocked(payloadID, gap[0], data[gap[0]-offset:gap[1]-offset])
	}
	return nil
}

// updateDirty publishes the delta in live dirty ranges. Caller holds mu.
func (s *MemoryStore) updateDirty(f *memFile) {
	n := rangeBytes(f.dirty)
	s.unsynced.Add(n - f.unsynced)
	f.unsynced = n
}

// rememberFence retains each mutation's cleared range for a file ID.
// Mutations are atomic under mu, so a bound at the completed mutation's version
// may fill again; every older bound remains fenced, including initial zero.
//
// ponytail: keep a bounded FIFO of mutation fences. Evicted versions raise a
// global floor, conservatively refusing old fills for other files too. Those
// reads or warm runs must retry with a fresh plan; without cold ranges, silently
// dropping a fill would instead serve zeros. Track active fetch versions if
// mutation churn makes those retries costly.
func (s *MemoryStore) rememberFence(id string, version uint64, survives int64) {
	s.fences[id] = version
	s.fenceOrder = append(s.fenceOrder, fenceEntry{id: id, version: version, survives: survives})
	if len(s.fenceOrder) > maxHydrateFences {
		oldest := s.fenceOrder[0]
		s.fenceOrder[0] = fenceEntry{}
		s.fenceOrder = s.fenceOrder[1:]
		if s.fences[oldest.id] == oldest.version {
			delete(s.fences, oldest.id)
		}
		s.fenceFloor = max(s.fenceFloor, oldest.version)
	}
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

// IsRangeResident checks the written ranges without touching the byte buffer.
// Adjacent writes are coalesced, and memory never evicts, so one range must
// cover the entire query for it to be resident.
func (s *MemoryStore) IsRangeResident(ctx context.Context, id journal.FileID, offset, length int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false, block.ErrStoreClosed
	}
	if offset < 0 {
		return false, block.ErrInvalidOffset
	}
	if length < 0 || length > math.MaxInt64-offset {
		return false, block.ErrInvalidSize
	}
	if length == 0 {
		return true, nil
	}
	f := s.files[string(id)]
	if f == nil {
		return false, nil
	}
	return coversRange(f.written, offset, offset+length), nil
}

func coversRange(ranges [][2]int64, start, end int64) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] > start })
	return i < len(ranges) && ranges[i][0] <= start && ranges[i][1] >= end
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
func (s *MemoryStore) Truncate(ctx context.Context, id journal.FileID, newSize int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payloadID := string(id)
	if newSize < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	// Memory has no cold markers, so an absent local tail can still have a
	// remote fetch in flight. Fence the requested tail even without a buffer.
	version := s.version.Add(1)
	s.rememberFence(payloadID, version, newSize)
	f := s.files[payloadID]
	if f == nil || int64(len(f.buf)) <= newSize {
		return nil
	}
	f.buf = f.buf[:newSize]
	f.clipWritten(newSize)
	f.dirty = clipRanges(f.dirty, newSize)
	s.updateDirty(f)
	f.version = version
	return nil
}

// Delete drops all of a file's cached ranges.
func (s *MemoryStore) Delete(ctx context.Context, id journal.FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payloadID := string(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	s.rememberFence(payloadID, s.version.Add(1), 0)
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
	release, err := s.acquireFlush(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return block.ErrStoreClosed
	}
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
		s.mu.Lock()
		f := s.files[fid]
		var data []byte
		var version uint64
		if f != nil && len(f.buf) > 0 {
			data = append([]byte(nil), f.buf...)
			version = f.version
			s.flushing[fid] = true
		}
		s.mu.Unlock()
		if len(data) == 0 {
			continue
		}
		defer s.finishFlush(fid)
		// Offer the whole dirty file as one run. Credit applies only to this
		// snapshot: a concurrent mutation leaves the file dirty for another pass.
		extents, ferr := fn(ctx, journal.Run{
			ID:       journal.FileID(fid),
			Extent:   journal.Extent{Off: 0, Len: int64(len(data)), State: journal.StateDirty},
			Final:    true,
			ReaderAt: bytes.NewReader(data),
		})
		// Manifest cleanup belongs to the serialized pass. Until it succeeds,
		// retain dirty bytes so retry can reconstruct and reap the same rows.
		var cleanupErr error
		if opts.AfterFile != nil {
			cleanupErr = opts.AfterFile(context.WithoutCancel(ctx), journal.FileID(fid))
		}
		if cleanupErr == nil {
			s.markCarvedSnapshot(fid, f, version, int64(len(data)), extents)
		}
		ferr = errors.Join(ferr, cleanupErr)
		if ferr != nil && firstErr == nil {
			firstErr = ferr
		}
	}
	return firstErr
}

func (s *MemoryStore) finishFlush(id string) {
	s.mu.Lock()
	delete(s.flushing, id)
	s.mu.Unlock()
}

// acquireFlush serializes a file's snapshot, sink callback, and dirty credit.
// A version check on credit alone cannot stop an old callback from publishing
// after a newer pass has committed its rows and already cleared dirty state.
//
// ponytail: 64 stripes bound lock memory and concurrent flushes. Collisions
// serialize unrelated files until the upload completes, while reads and writes
// remain independent. Use retiring per-file locks if memory-backed workloads
// need more flush concurrency than this bound provides.
func (s *MemoryStore) acquireFlush(ctx context.Context, id journal.FileID) (func(), error) {
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(id); i++ {
		hash = (hash ^ uint64(id[i])) * 1099511628211
	}
	mu := &s.flushLocks[hash%uint64(len(s.flushLocks))]
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if mu.TryLock() {
			return mu.Unlock, nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// markCarvedSnapshot clears only a fully durable, unchanged snapshot.
//
// ponytail: one version fences the whole file, so partial reports leave all
// dirty ranges for retry. Per-range versions would let a pass credit unchanged
// fragments even when another write arrived; add them if that efficiency is
// necessary for this local tier.
func (s *MemoryStore) markCarvedSnapshot(fid string, snapshot *memFile, version uint64, size int64, extents []journal.Extent) {
	var spans [][2]int64
	for _, e := range extents {
		if e.Off >= 0 && e.Len > 0 && e.Off <= size && e.Len <= size-e.Off {
			spans = append(spans, [2]int64{e.Off, e.Off + e.Len})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var covered int64
	for _, span := range spans {
		if span[0] > covered {
			return
		}
		covered = max(covered, span[1])
	}
	if covered < size {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[fid]
	if f != snapshot || f == nil || f.version != version {
		return
	}
	s.unsynced.Add(-f.unsynced)
	f.unsynced = 0
	f.dirty = nil
}

// UnsyncedBytes reports bytes not yet carved to the sink.
func (s *MemoryStore) UnsyncedBytes() int64 {
	if v := s.unsynced.Load(); v > 0 {
		return v
	}
	return 0
}

// HasDirty reports live bytes or an active pass still awaiting publication.
func (s *MemoryStore) HasDirty(ctx context.Context, id journal.FileID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false, block.ErrStoreClosed
	}
	f := s.files[string(id)]
	return s.flushing[string(id)] || (f != nil && f.unsynced > 0), nil
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

// WriteVersion reports the latest local mutation, including retained fences
// for files whose bytes truncate or delete removed.
func (s *MemoryStore) WriteVersion() uint64 { return s.version.Load() }

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
// time to restore. WriteVersion fences live cache fills but retains no history.
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
