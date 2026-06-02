package migrate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Test fixtures
// ============================================================================

// fakeBlockStore is an in-memory block.BlockStore for the migration
// tests. Records the chunks written so post-run assertions can verify
// what was Put.
type fakeBlockStore struct {
	mu       sync.Mutex
	chunks   map[block.ContentHash][]byte
	putCount int64
	getCount int64
	hasCount int64
}

func newFakeBlockStore() *fakeBlockStore {
	return &fakeBlockStore{
		chunks: make(map[block.ContentHash][]byte),
	}
}

func (b *fakeBlockStore) Put(_ context.Context, hash block.ContentHash, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	b.chunks[hash] = copied
	atomic.AddInt64(&b.putCount, 1)
	return nil
}

func (b *fakeBlockStore) Get(_ context.Context, hash block.ContentHash) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	atomic.AddInt64(&b.getCount, 1)
	data, ok := b.chunks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

func (b *fakeBlockStore) GetRange(_ context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.chunks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	if offset >= int64(len(data)) {
		return nil, block.ErrInvalidOffset
	}
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	out := make([]byte, end-offset)
	copy(out, data[offset:end])
	return out, nil
}

func (b *fakeBlockStore) Has(_ context.Context, hash block.ContentHash) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	atomic.AddInt64(&b.hasCount, 1)
	_, ok := b.chunks[hash]
	return ok, nil
}

func (b *fakeBlockStore) Delete(_ context.Context, hash block.ContentHash) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.chunks, hash)
	return nil
}

func (b *fakeBlockStore) Head(_ context.Context, hash block.ContentHash) (block.Meta, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.chunks[hash]
	if !ok {
		return block.Meta{}, block.ErrChunkNotFound
	}
	return block.Meta{Size: int64(len(data)), LastModified: time.Now()}, nil
}

func (b *fakeBlockStore) Walk(_ context.Context, fn func(block.ContentHash, block.Meta) error) error {
	b.mu.Lock()
	snap := make(map[block.ContentHash][]byte, len(b.chunks))
	for k, v := range b.chunks {
		snap[k] = v
	}
	b.mu.Unlock()
	for h, data := range snap {
		if err := fn(h, block.Meta{Size: int64(len(data)), LastModified: time.Now()}); err != nil {
			if errors.Is(err, block.ErrStopWalk) {
				return nil
			}
			return fmt.Errorf("walk halted at %s: %w", h, err)
		}
	}
	return nil
}

// corruptingBlockStore wraps a fakeBlockStore but returns DIFFERENT bytes
// on Get than what was Put — used to exercise the verification-mismatch
// path. ErrChunkPutMismatch is the expected return from
// MigrateShareToCAS.
type corruptingBlockStore struct {
	*fakeBlockStore
}

func (c *corruptingBlockStore) Get(_ context.Context, hash block.ContentHash) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.chunks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	// Flip one byte to simulate storage corruption.
	out := make([]byte, len(data))
	copy(out, data)
	if len(out) > 0 {
		out[0] ^= 0xff
	}
	return out, nil
}

// stubMetadataAdapter is the in-memory MetadataAdapter the unit tests
// drive. Returns a pre-loaded list of legacy files and records each
// UpdateFileBlocks call for assertion.
type stubMetadataAdapter struct {
	files       []LegacyFileInfo
	mu          sync.Mutex
	updateCalls []updateCall
}

type updateCall struct {
	handle metadata.FileHandle
	blocks []block.BlockRef
}

func (s *stubMetadataAdapter) ListLegacyFiles(_ context.Context) ([]LegacyFileInfo, error) {
	out := make([]LegacyFileInfo, len(s.files))
	copy(out, s.files)
	return out, nil
}

func (s *stubMetadataAdapter) UpdateFileBlocks(_ context.Context, handle metadata.FileHandle, blocks []block.BlockRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls = append(s.updateCalls, updateCall{handle: handle, blocks: blocks})
	return nil
}

// buildLegacyShare materializes a fake share directory with `fileCount`
// legacy files of `fileSize` bytes each. The on-disk layout mirrors the
// pre-Phase-17 `<shareDir>/blocks/<shard>/<PayloadID>/<idx>.blk` shape. Returns the
// share directory + the LegacyFileInfo list ready to feed into the stub
// MetadataAdapter.
func buildLegacyShare(t *testing.T, fileCount int, fileSize int64) (string, []LegacyFileInfo) {
	t.Helper()
	shareDir := t.TempDir()

	const legacyBlockSize = 8 * 1024 * 1024 // 8 MiB

	var files []LegacyFileInfo
	for i := 0; i < fileCount; i++ {
		payloadID := metadata.PayloadID(fmt.Sprintf("file-%03d", i))
		pid := string(payloadID)
		shard := pid[:2]
		payloadDir := filepath.Join(shareDir, "blocks", shard, pid)
		if err := os.MkdirAll(payloadDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		// Generate deterministic-ish content from crypto/rand for entropy
		// — chunks should not all dedup to one hash.
		data := make([]byte, fileSize)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		// Slice into 8 MiB legacy blocks.
		var idx uint64
		for off := int64(0); off < fileSize; off += legacyBlockSize {
			end := off + legacyBlockSize
			if end > fileSize {
				end = fileSize
			}
			blkPath := filepath.Join(payloadDir, fmt.Sprintf("%d.blk", idx))
			if err := os.WriteFile(blkPath, data[off:end], 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", blkPath, err)
			}
			idx++
		}
		files = append(files, LegacyFileInfo{
			Handle:    metadata.FileHandle(fmt.Sprintf("h-%03d", i)),
			Path:      string(payloadID),
			PayloadID: payloadID,
			Size:      fileSize,
			BlockSize: legacyBlockSize,
		})
	}
	return shareDir, files
}

// ============================================================================
// TestMigrateShareToCAS_HappyPath
// ============================================================================

func TestMigrateShareToCAS_HappyPath(t *testing.T) {
	shareDir, files := buildLegacyShare(t, 3, 5*1024*1024) // 3 files of 5 MiB each

	bs := newFakeBlockStore()
	adapter := &stubMetadataAdapter{files: files}

	res, err := MigrateShareToCAS(context.Background(), shareDir, bs, adapter, MigrationOpts{})
	if err != nil {
		t.Fatalf("MigrateShareToCAS: %v", err)
	}

	if res.Stats.FilesDone != 3 {
		t.Errorf("FilesDone = %d, want 3", res.Stats.FilesDone)
	}
	if res.Stats.ChunksDone == 0 {
		t.Errorf("ChunksDone = 0; expected non-zero chunks emitted")
	}
	if len(adapter.updateCalls) != 3 {
		t.Errorf("len(updateCalls) = %d, want 3", len(adapter.updateCalls))
	}
	for i, call := range adapter.updateCalls {
		if len(call.blocks) == 0 {
			t.Errorf("updateCalls[%d].blocks is empty", i)
		}
	}
	if len(bs.chunks) == 0 {
		t.Errorf("destination store has 0 chunks; expected at least one Put per file")
	}

	// Sentinel exists at <shareDir>/.cas-migrated-v1.
	sentinelPath := filepath.Join(shareDir, SentinelFileName)
	st, err := os.Stat(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel %s not present: %v", sentinelPath, err)
	}
	if st.Size() == 0 {
		t.Errorf("sentinel is empty")
	}

	// Sentinel .tmp must NOT linger post-success.
	if _, err := os.Stat(sentinelPath + SentinelTmpSuffix); !os.IsNotExist(err) {
		t.Errorf("sentinel .tmp lingered post-success: %v", err)
	}

	// Journal removed post-success.
	journalPath := filepath.Join(shareDir, MigrateJournalFile)
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Errorf("journal not removed post-success: %v", err)
	}

	// Legacy .blk files removed.
	for _, f := range files {
		pid := string(f.PayloadID)
		shard := pid[:2]
		payloadDir := filepath.Join(shareDir, "blocks", shard, pid)
		entries, _ := os.ReadDir(payloadDir)
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".blk" {
				t.Errorf("legacy .blk remains: %s", e.Name())
			}
		}
	}
}

// ============================================================================
// TestMigrateShareToCAS_JournalResume
// ============================================================================

func TestMigrateShareToCAS_JournalResume(t *testing.T) {
	// Build a share with one large file; cancel mid-flight; rerun and
	// verify it completes with the sentinel present.
	shareDir, files := buildLegacyShare(t, 5, 2*1024*1024)

	bs := newFakeBlockStore()
	adapter := &stubMetadataAdapter{files: files}

	ctx, cancel := context.WithCancel(context.Background())
	progressSeen := make(chan struct{}, 1)
	opts := MigrationOpts{
		Progress: func(s MigrationStats) {
			if s.FilesDone > 0 {
				select {
				case progressSeen <- struct{}{}:
				default:
				}
			}
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := MigrateShareToCAS(ctx, shareDir, bs, adapter, opts)
		done <- err
	}()

	// Wait up to 5s for the first file to complete, then cancel.
	select {
	case <-progressSeen:
	case <-time.After(5 * time.Second):
		// If we never see progress, the test box is slow — let it run
		// and cancel anyway.
	}
	cancel()
	<-done

	// Journal should exist; sentinel should NOT (since we cancelled).
	journalPath := filepath.Join(shareDir, MigrateJournalFile)
	if _, err := os.Stat(journalPath); err != nil {
		// Journal may have been removed if the run completed before our
		// cancel. That's also valid — check the sentinel instead.
		t.Logf("journal absent post-cancel (run may have completed in time): %v", err)
	}

	// Re-run on a fresh context.
	res, err := MigrateShareToCAS(context.Background(), shareDir, bs, adapter, MigrationOpts{})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if res.Stats.FilesDone == 0 {
		// All-files-already-done means the first run completed before
		// cancel. Still a valid path.
		t.Logf("resume reported zero files; first run completed pre-cancel")
	}

	// Sentinel must exist post-resume.
	sentinelPath := filepath.Join(shareDir, SentinelFileName)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("sentinel missing post-resume: %v", err)
	}

	// All files in the metadata adapter received an UpdateFileBlocks call
	// across the combined runs (cancel + resume).
	if len(adapter.updateCalls) < len(files) {
		t.Errorf("updateCalls=%d after resume, expected >= %d", len(adapter.updateCalls), len(files))
	}
}

// ============================================================================
// TestMigrateShareToCAS_DryRun
// ============================================================================

func TestMigrateShareToCAS_DryRun(t *testing.T) {
	shareDir, files := buildLegacyShare(t, 2, 3*1024*1024)

	bs := newFakeBlockStore()
	adapter := &stubMetadataAdapter{files: files}

	res, err := MigrateShareToCAS(context.Background(), shareDir, bs, adapter,
		MigrationOpts{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	// No chunks should have been Put to the destination store.
	if bs.putCount != 0 {
		t.Errorf("dry-run wrote %d chunks; expected 0", bs.putCount)
	}
	if len(bs.chunks) != 0 {
		t.Errorf("dry-run left %d chunks on destination", len(bs.chunks))
	}

	// EstDedupRatio should be in (0, 1] when at least one chunk sampled
	// allow 0 if the per-file sample budget produced no chunks.
	if res.EstDedupRatio < 0 || res.EstDedupRatio > 1 {
		t.Errorf("EstDedupRatio out of [0,1]: %f", res.EstDedupRatio)
	}

	// No sentinel, no journal.
	if _, err := os.Stat(filepath.Join(shareDir, SentinelFileName)); !os.IsNotExist(err) {
		t.Errorf("sentinel present after dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shareDir, MigrateJournalFile)); !os.IsNotExist(err) {
		t.Errorf("journal present after dry-run: %v", err)
	}

	// No metadata updates.
	if len(adapter.updateCalls) != 0 {
		t.Errorf("dry-run committed %d metadata updates; expected 0", len(adapter.updateCalls))
	}
}

// ============================================================================
// TestMigrateShareToCAS_SentinelAtomic
// ============================================================================

func TestMigrateShareToCAS_SentinelAtomic(t *testing.T) {
	shareDir, files := buildLegacyShare(t, 1, 2*1024*1024)

	bs := newFakeBlockStore()
	adapter := &stubMetadataAdapter{files: files}

	if _, err := MigrateShareToCAS(context.Background(), shareDir, bs, adapter, MigrationOpts{}); err != nil {
		t.Fatalf("MigrateShareToCAS: %v", err)
	}

	sentinelPath := filepath.Join(shareDir, SentinelFileName)
	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	var content sentinelContent
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatalf("sentinel not valid JSON: %v", err)
	}
	if content.Version != "v1" {
		t.Errorf("sentinel Version = %q, want v1", content.Version)
	}
	if content.ToolVersion != MigrationToolVersion {
		t.Errorf("sentinel ToolVersion = %q, want %q", content.ToolVersion, MigrationToolVersion)
	}
	if content.CompletedAt.IsZero() {
		t.Errorf("sentinel CompletedAt is zero")
	}

	// .tmp must NOT linger.
	if _, err := os.Stat(sentinelPath + SentinelTmpSuffix); !os.IsNotExist(err) {
		t.Errorf("sentinel .tmp lingered: %v", err)
	}
}

// ============================================================================
// TestMigrateShareToCAS_VerifyMismatch
// ============================================================================

func TestMigrateShareToCAS_VerifyMismatch(t *testing.T) {
	shareDir, files := buildLegacyShare(t, 1, 2*1024*1024)

	bs := &corruptingBlockStore{fakeBlockStore: newFakeBlockStore()}
	adapter := &stubMetadataAdapter{files: files}

	_, err := MigrateShareToCAS(context.Background(), shareDir, bs, adapter, MigrationOpts{})
	if err == nil {
		t.Fatalf("expected error from verification mismatch, got nil")
	}
	if !errors.Is(err, ErrChunkPutMismatch) {
		t.Errorf("expected errors.Is(err, ErrChunkPutMismatch) = true; got err=%v", err)
	}

	// Sentinel must NOT exist after failure.
	if _, statErr := os.Stat(filepath.Join(shareDir, SentinelFileName)); !os.IsNotExist(statErr) {
		t.Errorf("sentinel present after verification failure: %v", statErr)
	}

	// Journal MUST exist (resume point preserved).
	if _, statErr := os.Stat(filepath.Join(shareDir, MigrateJournalFile)); statErr != nil {
		t.Errorf("journal missing after failure: %v", statErr)
	}
}
