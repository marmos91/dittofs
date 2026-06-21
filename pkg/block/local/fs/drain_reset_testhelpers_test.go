package fs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// memFBS is a minimal in-memory block.EngineFileBlockStore that
// retains the per-payload FileBlock manifest rows persisted by the rollup
// ObjectIDPersister. Unlike nopFBS, its ListFileBlocks returns the stored
// rows so ReadPayloadAt's CAS-manifest path can resolve rolled-up bytes —
// which is exactly what ResetLocalState must leave readable after dropping
// the stale append log.
type memFBS struct {
	mu   sync.Mutex
	rows map[string]map[string]*block.FileBlock // payloadID -> blockID -> row
}

func newMemFileBlockStore() *memFBS {
	return &memFBS{rows: make(map[string]map[string]*block.FileBlock)}
}

// persist mirrors the engine's ObjectIDPersister FileBlock-row write loop
// (engine.go) so the test FBS holds rows with the canonical
// "<payloadID>/<offset>" ID that ParseChunkOffset decodes.
func (m *memFBS) persist(_ context.Context, payloadID string, blocks []block.BlockRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm := m.rows[payloadID]
	if pm == nil {
		pm = make(map[string]*block.FileBlock)
		m.rows[payloadID] = pm
	}
	for _, b := range blocks {
		if b.Hash.IsZero() {
			continue
		}
		id := fmt.Sprintf("%s/%d", payloadID, b.Offset)
		pm[id] = &block.FileBlock{
			ID:       id,
			Hash:     b.Hash,
			DataSize: b.Size,
			State:    block.BlockStatePending,
		}
	}
	return nil
}

func (m *memFBS) ListFileBlocks(_ context.Context, payloadID string) ([]*block.FileBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm := m.rows[payloadID]
	out := make([]*block.FileBlock, 0, len(pm))
	for _, fb := range pm {
		out = append(out, fb)
	}
	return out, nil
}

func (m *memFBS) GetFileBlock(_ context.Context, blockID string) (*block.FileBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pm := range m.rows {
		if fb, ok := pm[blockID]; ok {
			return fb, nil
		}
	}
	return nil, block.ErrFileBlockNotFound
}

func (m *memFBS) GetByHash(_ context.Context, _ block.ContentHash) (*block.FileBlock, error) {
	return nil, nil
}
func (m *memFBS) Put(_ context.Context, _ *block.FileBlock) error { return nil }
func (m *memFBS) Delete(_ context.Context, _ string) error {
	return block.ErrFileBlockNotFound
}
func (m *memFBS) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (m *memFBS) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (m *memFBS) DecrementRefCountAndReap(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (m *memFBS) AddRef(_ context.Context, _ block.ContentHash, _ string, _ block.BlockRef) error {
	return block.ErrUnknownHash
}
func (m *memFBS) ListPending(_ context.Context, _ time.Duration, _ int) ([]*block.FileBlock, error) {
	return nil, nil
}

// newFSStoreForTestWithFBS is newFSStoreForTest with a caller-supplied
// FileBlockStore so the CAS-manifest read path can resolve rolled-up bytes.
func newFSStoreForTestWithFBS(t *testing.T, fbs block.EngineFileBlockStore, opts FSStoreOptions) *FSStore {
	t.Helper()
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
