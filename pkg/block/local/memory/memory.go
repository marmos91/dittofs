// Package memory is a pure in-memory local.LocalStore used by tests and
// ephemeral configs. Since the Wall-A switchover it is a per-file byte cache
// (payloadID+offset keyed), mirroring the journal's shape without disk,
// segments, or eviction.
package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
)

var (
	_ local.LocalStore         = (*MemoryStore)(nil)
	_ block.DurabilityReporter = (*MemoryStore)(nil)
)

// ErrStoreClosed is an alias for block.ErrStoreClosed for backward compatibility.
var ErrStoreClosed = block.ErrStoreClosed

// memFile is one file's byte buffer plus its not-yet-carved byte count.
type memFile struct {
	buf      []byte
	unsynced int64
}

// MemoryStore is a pure in-memory implementation of local.LocalStore.
type MemoryStore struct {
	mu    sync.RWMutex
	files map[string]*memFile

	deduper journal.Deduper
	sink    journal.BlockSink

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
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[offset:end], data)
	return int64(len(data))
}

// WriteAt buffers a dirty write.
func (s *MemoryStore) WriteAt(_ context.Context, payloadID string, offset int64, data []byte) error {
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	n := s.writeLocked(payloadID, offset, data)
	s.files[payloadID].unsynced += n
	s.unsynced.Add(n)
	return nil
}

// Hydrate writes remote-fetched bytes; born clean, so no unsynced charge.
func (s *MemoryStore) Hydrate(_ context.Context, payloadID string, offset int64, data []byte) error {
	if offset < 0 {
		return block.ErrInvalidOffset
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	s.writeLocked(payloadID, offset, data)
	return nil
}

// ReadAt copies bytes into dst; never-written ranges are zero-filled holes.
// Memory never evicts, so cold is always false.
func (s *MemoryStore) ReadAt(_ context.Context, payloadID string, offset int64, dst []byte) (int, bool, error) {
	if offset < 0 {
		return 0, false, block.ErrInvalidOffset
	}
	if len(dst) == 0 {
		return 0, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, false, ErrStoreClosed
	}
	clear(dst)
	f := s.files[payloadID]
	if f == nil || offset >= int64(len(f.buf)) {
		return len(dst), false, nil
	}
	copy(dst, f.buf[offset:])
	return len(dst), false, nil
}

// Commit is a no-op: memory has no durable substrate.
func (s *MemoryStore) Commit(context.Context, string) error { return nil }

// FileSize reports the data high-water mark.
func (s *MemoryStore) FileSize(_ context.Context, payloadID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.files[payloadID]
	if f == nil {
		return 0, false
	}
	return int64(len(f.buf)), true
}

// DataExtents returns the single written region clamped to fileSize. Conservative
// over-reporting is RFC-safe.
func (s *MemoryStore) DataExtents(_ context.Context, payloadID string, fileSize int64) ([][2]uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.files[payloadID]
	if f == nil || len(f.buf) == 0 || fileSize <= 0 {
		return nil, nil
	}
	end := int64(len(f.buf))
	if end > fileSize {
		end = fileSize
	}
	if end <= 0 {
		return nil, nil
	}
	return [][2]uint64{{0, uint64(end)}}, nil
}

// Truncate shrinks a file to newSize; growing is a no-op.
func (s *MemoryStore) Truncate(_ context.Context, payloadID string, newSize int64) error {
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
	return nil
}

// Delete drops all of a file's cached ranges.
func (s *MemoryStore) Delete(_ context.Context, payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[payloadID]; f != nil {
		s.unsynced.Add(-f.unsynced)
		delete(s.files, payloadID)
	}
	return nil
}

// ListFiles returns every payloadID with local data.
func (s *MemoryStore) ListFiles(context.Context) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.files))
	for id := range s.files {
		out = append(out, id)
	}
	return out
}

// SetCarveTargets injects the carve collaborators.
func (s *MemoryStore) SetCarveTargets(d journal.Deduper, sink journal.BlockSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deduper = d
	s.sink = sink
}

// Carve packs each dirty file's bytes into the sink.
//
// ponytail: packs each dirty file as ONE whole-file chunk at offset 0 rather
// than running FastCDC — this is a test fixture with no cross-store dedup
// contract, so chunk boundaries don't matter. Wire the real chunker if a memory
// test ever asserts journal-identical boundaries.
func (s *MemoryStore) Carve(ctx context.Context, opts journal.CarveOptions) (journal.CarveResult, error) {
	s.mu.RLock()
	d, sink := s.deduper, s.sink
	var ids []string
	if opts.FileID != "" {
		if s.files[string(opts.FileID)] != nil {
			ids = []string{string(opts.FileID)}
		}
	} else {
		for id, f := range s.files {
			if f.unsynced > 0 {
				ids = append(ids, id)
			}
		}
	}
	s.mu.RUnlock()

	var res journal.CarveResult
	if sink == nil {
		return res, nil
	}

	for _, id := range ids {
		s.mu.RLock()
		f := s.files[id]
		var data []byte
		if f != nil {
			data = append([]byte(nil), f.buf...)
		}
		s.mu.RUnlock()
		if len(data) == 0 {
			continue
		}
		h := journal.ChunkHash(blake3.Sum256(data))
		if d != nil {
			if durable, err := d.IsChunkDurable(ctx, h); err == nil && durable {
				s.markCarved(id)
				continue
			}
		}
		if err := sink.CommitBlock(ctx, []journal.CarveChunk{{
			Hash: h, FileID: journal.FileID(id), FileOffset: 0, Data: data,
		}}); err != nil {
			return res, err
		}
		res.BlocksWritten++
		res.BytesCarved += int64(len(data))
		s.markCarved(id)
	}
	return res, nil
}

// markCarved clears a file's unsynced charge after its bytes reach the sink.
func (s *MemoryStore) markCarved(payloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[payloadID]; f != nil {
		s.unsynced.Add(-f.unsynced)
		f.unsynced = 0
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

// SetRetentionPolicy is a no-op.
func (s *MemoryStore) SetRetentionPolicy(block.RetentionPolicy, time.Duration) {}

// Start is a no-op.
func (s *MemoryStore) Start(context.Context) {}

// Close marks the store closed.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Stats reports coarse in-memory usage.
func (s *MemoryStore) Stats() local.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var used int64
	for _, f := range s.files {
		used += int64(len(f.buf))
	}
	return local.Stats{DiskUsed: used, FileCount: len(s.files)}
}

// Durable reports crash survival. In-memory storage is volatile → false default.
func (s *MemoryStore) Durable() bool { return s.durable.Load() }

// SetDurable overrides the durability report.
func (s *MemoryStore) SetDurable(v bool) { s.durable.Store(v) }
