package fs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// memFBS is a minimal in-memory block.EngineFileChunkStore that
// retains the per-payload FileChunk manifest rows persisted by the rollup
// ObjectIDPersister. Unlike nopFBS, its ListFileChunks returns the stored
// rows so ReadPayloadAt's CAS-manifest path can resolve rolled-up bytes —
// which is exactly what ResetLocalState must leave readable after dropping
// the stale append log.
type memFBS struct {
	mu   sync.Mutex
	rows map[string]map[string]*block.FileChunk // payloadID -> blockID -> row
}

func newMemFileChunkStore() *memFBS {
	return &memFBS{rows: make(map[string]map[string]*block.FileChunk)}
}

// persist mirrors the engine's ObjectIDPersister FileChunk-row write loop
// (engine.go) so the test FBS holds rows with the canonical
// "<payloadID>/<offset>" ID that ParseChunkOffset decodes.
func (m *memFBS) persist(_ context.Context, payloadID string, blocks []block.ChunkRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm := m.rows[payloadID]
	if pm == nil {
		pm = make(map[string]*block.FileChunk)
		m.rows[payloadID] = pm
	}
	for _, b := range blocks {
		if b.Hash.IsZero() {
			continue
		}
		id := fmt.Sprintf("%s/%d", payloadID, b.Offset)
		pm[id] = &block.FileChunk{
			ID:       id,
			Hash:     b.Hash,
			DataSize: b.Size,
			State:    block.BlockStatePending,
		}
	}
	return nil
}

func (m *memFBS) ListFileChunks(_ context.Context, payloadID string) ([]*block.FileChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm := m.rows[payloadID]
	out := make([]*block.FileChunk, 0, len(pm))
	for _, fb := range pm {
		out = append(out, fb)
	}
	return out, nil
}

func (m *memFBS) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.rows))
	for payloadID := range m.rows {
		ids = append(ids, payloadID)
	}
	m.mu.Unlock()
	for _, payloadID := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// GetFileChunkAtOffset + GetFileChunkAtOrAfterOffset make memFBS satisfy the
// coveringChunkResolver fast path so ReadPayloadAt drives fillFromCASManifest's
// indexed covering + successor loop (not the ListFileChunks scan fallback).
// Both mirror the badger backend's contract, which pkg/metadata/storetest pins.
func (m *memFBS) GetFileChunkAtOffset(_ context.Context, payloadID string, off uint64) (*block.FileChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, fb := range m.rows[payloadID] {
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		if off >= abs && off-abs < uint64(fb.DataSize) {
			return fb, nil // covering guard: off is within [abs, abs+DataSize)
		}
	}
	return nil, nil
}

func (m *memFBS) GetFileChunkAtOrAfterOffset(_ context.Context, payloadID string, off uint64) (*block.FileChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *block.FileChunk
	var bestAbs uint64
	for _, fb := range m.rows[payloadID] {
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		if abs >= off && (best == nil || abs < bestAbs) {
			best, bestAbs = fb, abs
		}
	}
	return best, nil
}

func (m *memFBS) GetFileChunk(_ context.Context, blockID string) (*block.FileChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pm := range m.rows {
		if fb, ok := pm[blockID]; ok {
			return fb, nil
		}
	}
	return nil, block.ErrFileChunkNotFound
}

func (m *memFBS) GetByHash(_ context.Context, _ block.ContentHash) (*block.FileChunk, error) {
	return nil, nil
}
func (m *memFBS) Put(_ context.Context, _ *block.FileChunk) error { return nil }
func (m *memFBS) Delete(_ context.Context, _ string) error {
	return block.ErrFileChunkNotFound
}
func (m *memFBS) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (m *memFBS) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (m *memFBS) DecrementRefCountAndReap(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (m *memFBS) AddRef(_ context.Context, _ block.ContentHash, _ string, _ block.ChunkRef) error {
	return block.ErrUnknownHash
}

// newFSStoreForTestWithFBS is newFSStoreForTest with a caller-supplied
// FileChunkStore so the CAS-manifest read path can resolve rolled-up bytes.
func newFSStoreForTestWithFBS(t *testing.T, fbs block.EngineFileChunkStore, opts FSStoreOptions) *FSStore {
	t.Helper()
	// The log-blob substrate is required for normal chunk I/O; default-wire a
	// memory metadata store as the LocalChunkIndex when the caller did not
	// supply one.
	if opts.LocalChunkIndex == nil {
		opts.LocalChunkIndex = memmeta.NewMemoryMetadataStoreWithDefaults()
	}
	dir, err := os.MkdirTemp("", "fsstore-drainreset-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	bc, err := NewWithOptions(dir, 1<<30, fbs, opts)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() {
		_ = bc.Close()
		for range 5 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return bc
}
