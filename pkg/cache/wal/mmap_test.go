//go:build !windows

package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMmapPersister_CreateNew(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	if !p.IsEnabled() {
		t.Error("IsEnabled() = false, want true")
	}

	// Verify file was created
	filePath := filepath.Join(dir, "cache.dat")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("cache.dat was not created")
	}
}

func TestMmapPersister_AppendBlockWrite(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	entry := &BlockWriteEntry{
		PayloadID:     "test-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte("0123456789"),
	}

	if err := p.AppendBlockWrite(entry); err != nil {
		t.Fatalf("AppendBlockWrite() error = %v", err)
	}

	if err := p.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
}

func TestMmapPersister_AppendAndRecover(t *testing.T) {
	dir := t.TempDir()

	// Create persister and write entries
	p1, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	entries := []*BlockWriteEntry{
		{
			PayloadID:     "file1",
			ChunkIdx:      0,
			BlockIdx:      0,
			OffsetInBlock: 0,
			Data:          []byte("hello"),
		},
		{
			PayloadID:     "file1",
			ChunkIdx:      0,
			BlockIdx:      0,
			OffsetInBlock: 5,
			Data:          []byte("world"),
		},
		{
			PayloadID:     "file2",
			ChunkIdx:      1,
			BlockIdx:      2,
			OffsetInBlock: 100,
			Data:          []byte("test"),
		},
	}

	for _, e := range entries {
		if err := p1.AppendBlockWrite(e); err != nil {
			t.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}

	if err := p1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered.Entries) != len(entries) {
		t.Fatalf("Recover() got %d entries, want %d", len(recovered.Entries), len(entries))
	}

	// Verify entries
	for i, got := range recovered.Entries {
		want := entries[i]
		if got.PayloadID != want.PayloadID {
			t.Errorf("entry[%d].PayloadID = %s, want %s", i, got.PayloadID, want.PayloadID)
		}
		if got.ChunkIdx != want.ChunkIdx {
			t.Errorf("entry[%d].ChunkIdx = %d, want %d", i, got.ChunkIdx, want.ChunkIdx)
		}
		if got.BlockIdx != want.BlockIdx {
			t.Errorf("entry[%d].BlockIdx = %d, want %d", i, got.BlockIdx, want.BlockIdx)
		}
		if got.OffsetInBlock != want.OffsetInBlock {
			t.Errorf("entry[%d].OffsetInBlock = %d, want %d", i, got.OffsetInBlock, want.OffsetInBlock)
		}
		if string(got.Data) != string(want.Data) {
			t.Errorf("entry[%d].Data = %s, want %s", i, got.Data, want.Data)
		}
	}
}

func TestMmapPersister_AppendRemove(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Add some entries
	entry1 := &BlockWriteEntry{
		PayloadID:     "file1",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte("hello"),
	}
	entry2 := &BlockWriteEntry{
		PayloadID:     "file2",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte("world"),
	}

	if err := p.AppendBlockWrite(entry1); err != nil {
		t.Fatalf("AppendBlockWrite(entry1) error = %v", err)
	}
	if err := p.AppendBlockWrite(entry2); err != nil {
		t.Fatalf("AppendBlockWrite(entry2) error = %v", err)
	}

	// Remove file1
	if err := p.AppendRemove("file1"); err != nil {
		t.Fatalf("AppendRemove() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should only have file2 (file1 was removed)
	if len(recovered.Entries) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered.Entries))
	}

	if string(recovered.Entries[0].PayloadID) != "file2" {
		t.Errorf("recovered[0].PayloadID = %s, want file2", recovered.Entries[0].PayloadID)
	}
}

func TestMmapPersister_ClosedOperations(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// All operations should fail after close
	entry := &BlockWriteEntry{
		PayloadID: "test",
		Data:      []byte("data"),
	}

	if err := p.AppendBlockWrite(entry); err != ErrPersisterClosed {
		t.Errorf("AppendBlockWrite() after close = %v, want ErrPersisterClosed", err)
	}

	if err := p.AppendRemove("test"); err != ErrPersisterClosed {
		t.Errorf("AppendRemove() after close = %v, want ErrPersisterClosed", err)
	}

	if err := p.Sync(); err != ErrPersisterClosed {
		t.Errorf("Sync() after close = %v, want ErrPersisterClosed", err)
	}

	if _, err := p.Recover(); err != ErrPersisterClosed {
		t.Errorf("Recover() after close = %v, want ErrPersisterClosed", err)
	}
}

func TestMmapPersister_GrowFile(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Write enough data to trigger file growth
	largeData := make([]byte, 10*1024*1024) // 10MB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	for i := 0; i < 10; i++ {
		entry := &BlockWriteEntry{
			PayloadID:     "large-file",
			ChunkIdx:      uint32(i),
			BlockIdx:      0,
			OffsetInBlock: 0,
			Data:          largeData,
		}

		if err := p.AppendBlockWrite(entry); err != nil {
			t.Fatalf("AppendBlockWrite(%d) error = %v", i, err)
		}
	}
}

// ============================================================================
// Unit Tests - Edge Cases and Error Handling
// ============================================================================

func TestMmapPersister_EmptyRecover(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Recover from empty WAL should return empty result
	result, err := p.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(result.Entries) != 0 {
		t.Errorf("Recover() got %d entries, want 0", len(result.Entries))
	}
}

func TestMmapPersister_LargePayloadID(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Create entry with large file handle (512 bytes)
	largeHandle := make([]byte, 512)
	for i := range largeHandle {
		largeHandle[i] = byte('a' + i%26)
	}

	entry := &BlockWriteEntry{
		PayloadID:     string(largeHandle),
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte("hello"),
	}

	if err := p.AppendBlockWrite(entry); err != nil {
		t.Fatalf("AppendBlockWrite() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered.Entries) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered.Entries))
	}

	if recovered.Entries[0].PayloadID != string(largeHandle) {
		t.Errorf("PayloadID mismatch after recovery")
	}
}

func TestMmapPersister_ZeroLengthData(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	entry := &BlockWriteEntry{
		PayloadID:     "empty-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte{},
	}

	if err := p.AppendBlockWrite(entry); err != nil {
		t.Fatalf("AppendBlockWrite() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered.Entries) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered.Entries))
	}

	if len(recovered.Entries[0].Data) != 0 {
		t.Errorf("Expected zero-length data")
	}
}

func TestMmapPersister_CorruptedMagic(t *testing.T) {
	dir := t.TempDir()

	// Create a valid persister first
	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	_ = p.Close()

	// Corrupt the magic bytes
	filePath := filepath.Join(dir, "cache.dat")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// Corrupt magic
	data[0] = 'X'
	data[1] = 'X'
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Attempt to open should fail
	_, err = NewMmapPersister(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("NewMmapPersister() error = %v, want ErrCorrupted", err)
	}
}

func TestMmapPersister_VersionMismatch(t *testing.T) {
	dir := t.TempDir()

	// Create a valid persister first
	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	_ = p.Close()

	// Change version to something wrong
	filePath := filepath.Join(dir, "cache.dat")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// Set version to 255
	data[4] = 255
	data[5] = 0
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Attempt to open should fail
	_, err = NewMmapPersister(dir)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("NewMmapPersister() error = %v, want ErrVersionMismatch", err)
	}
}

func TestMmapPersister_DoubleClose(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// First close should succeed
	if err := p.Close(); err != nil {
		t.Fatalf("First Close() error = %v", err)
	}

	// Second close should be no-op
	if err := p.Close(); err != nil {
		t.Errorf("Second Close() error = %v, want nil", err)
	}
}

func TestMmapPersister_SyncNoDirty(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Sync with no dirty data should be no-op
	if err := p.Sync(); err != nil {
		t.Errorf("Sync() error = %v", err)
	}
}

// ============================================================================
// Integration Tests - Recovery Scenarios
// ============================================================================

func TestMmapPersister_MultiFileRecovery(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Write entries for multiple files with multiple blocks
	files := []struct {
		handle string
		blocks int
	}{
		{"file1", 3},
		{"file2", 5},
		{"file3", 2},
	}

	totalEntries := 0
	for _, f := range files {
		for blockIdx := 0; blockIdx < f.blocks; blockIdx++ {
			entry := &BlockWriteEntry{
				PayloadID:     f.handle,
				ChunkIdx:      0,
				BlockIdx:      uint32(blockIdx),
				OffsetInBlock: 0,
				Data:          []byte(fmt.Sprintf("data-%s-%d", f.handle, blockIdx)),
			}
			if err := p.AppendBlockWrite(entry); err != nil {
				t.Fatalf("AppendBlockWrite() error = %v", err)
			}
			totalEntries++
		}
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and verify
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered.Entries) != totalEntries {
		t.Fatalf("Recover() got %d entries, want %d", len(recovered.Entries), totalEntries)
	}

	// Verify data integrity
	idx := 0
	for _, f := range files {
		for blockIdx := 0; blockIdx < f.blocks; blockIdx++ {
			got := recovered.Entries[idx]
			if got.PayloadID != f.handle {
				t.Errorf("entry[%d].PayloadID = %s, want %s", idx, got.PayloadID, f.handle)
			}
			if got.BlockIdx != uint32(blockIdx) {
				t.Errorf("entry[%d].BlockIdx = %d, want %d", idx, got.BlockIdx, blockIdx)
			}
			wantData := fmt.Sprintf("data-%s-%d", f.handle, blockIdx)
			if string(got.Data) != wantData {
				t.Errorf("entry[%d].Data = %s, want %s", idx, got.Data, wantData)
			}
			idx++
		}
	}
}

func TestMmapPersister_RemoveInterleavedWithWrites(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Interleave write and remove operations
	operations := []struct {
		op       string
		handle   string
		blockIdx uint32
	}{
		{"write", "file1", 0},
		{"write", "file1", 1},
		{"write", "file2", 0},
		{"remove", "file1", 0},
		{"write", "file3", 0},
		{"write", "file2", 1},
		{"remove", "file2", 0},
		{"write", "file3", 1},
	}

	for i, op := range operations {
		if op.op == "write" {
			entry := &BlockWriteEntry{
				PayloadID:     op.handle,
				ChunkIdx:      0,
				BlockIdx:      op.blockIdx,
				OffsetInBlock: 0,
				Data:          []byte("hello"),
			}
			if err := p.AppendBlockWrite(entry); err != nil {
				t.Fatalf("AppendBlockWrite() error = %v", err)
			}
		} else {
			if err := p.AppendRemove(op.handle); err != nil {
				t.Fatalf("AppendRemove() error = %v (op %d)", err, i)
			}
		}
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover - should only have file3 entries
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should only have 2 entries (both for file3)
	if len(recovered.Entries) != 2 {
		t.Fatalf("Recover() got %d entries, want 2", len(recovered.Entries))
	}

	for _, entry := range recovered.Entries {
		if entry.PayloadID != "file3" {
			t.Errorf("Unexpected file handle: %s (expected file3)", entry.PayloadID)
		}
	}
}

func TestMmapPersister_BlockUploadedTracking(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Write some blocks
	entries := []*BlockWriteEntry{
		{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 0, OffsetInBlock: 0, Data: []byte("hello")},
		{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 1, OffsetInBlock: 0, Data: []byte("world")},
		{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 2, OffsetInBlock: 0, Data: []byte("test")},
	}

	for _, e := range entries {
		if err := p.AppendBlockWrite(e); err != nil {
			t.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}

	// Mark block 0 and block 2 as uploaded
	if err := p.AppendBlockUploaded("file1", 0, 0); err != nil {
		t.Fatalf("AppendBlockUploaded(0,0) error = %v", err)
	}
	if err := p.AppendBlockUploaded("file1", 0, 2); err != nil {
		t.Fatalf("AppendBlockUploaded(0,2) error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	result, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should have all 3 entries
	if len(result.Entries) != 3 {
		t.Fatalf("Recover() got %d entries, want 3", len(result.Entries))
	}

	// Should have 2 uploaded blocks
	if len(result.UploadedBlocks) != 2 {
		t.Fatalf("Recover() got %d uploaded blocks, want 2", len(result.UploadedBlocks))
	}

	// Verify uploaded blocks
	key0 := BlockKey{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 0}
	key1 := BlockKey{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 1}
	key2 := BlockKey{PayloadID: "file1", ChunkIdx: 0, BlockIdx: 2}

	if !result.UploadedBlocks[key0] {
		t.Error("Block 0 should be marked as uploaded")
	}
	if result.UploadedBlocks[key1] {
		t.Error("Block 1 should NOT be marked as uploaded")
	}
	if !result.UploadedBlocks[key2] {
		t.Error("Block 2 should be marked as uploaded")
	}
}

func TestMmapPersister_RemoveClearsUploadedBlocks(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Write blocks for file1
	entry := &BlockWriteEntry{
		PayloadID:     "file1",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          []byte("hello"),
	}
	if err := p.AppendBlockWrite(entry); err != nil {
		t.Fatalf("AppendBlockWrite() error = %v", err)
	}

	// Mark block as uploaded
	if err := p.AppendBlockUploaded("file1", 0, 0); err != nil {
		t.Fatalf("AppendBlockUploaded() error = %v", err)
	}

	// Remove file1
	if err := p.AppendRemove("file1"); err != nil {
		t.Fatalf("AppendRemove() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p2.Close() }()

	result, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should have no entries (file was removed)
	if len(result.Entries) != 0 {
		t.Fatalf("Recover() got %d entries, want 0", len(result.Entries))
	}

	// Should have no uploaded blocks (file was removed)
	if len(result.UploadedBlocks) != 0 {
		t.Fatalf("Recover() got %d uploaded blocks, want 0", len(result.UploadedBlocks))
	}
}

func TestMmapPersister_AppendAfterRecovery(t *testing.T) {
	dir := t.TempDir()

	// First session: write some entries
	p1, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	for i := 0; i < 3; i++ {
		entry := &BlockWriteEntry{
			PayloadID:     "test-file",
			ChunkIdx:      0,
			BlockIdx:      uint32(i),
			OffsetInBlock: 0,
			Data:          []byte("hello"),
		}
		if err := p1.AppendBlockWrite(entry); err != nil {
			t.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}

	if err := p1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Second session: recover and append more
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}

	_, err = p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Append more entries
	for i := 3; i < 6; i++ {
		entry := &BlockWriteEntry{
			PayloadID:     "test-file",
			ChunkIdx:      0,
			BlockIdx:      uint32(i),
			OffsetInBlock: 0,
			Data:          []byte("world"),
		}
		if err := p2.AppendBlockWrite(entry); err != nil {
			t.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}

	if err := p2.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Third session: verify all entries
	p3, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer func() { _ = p3.Close() }()

	recovered, err := p3.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered.Entries) != 6 {
		t.Fatalf("Recover() got %d entries, want 6", len(recovered.Entries))
	}

	// Verify data from both sessions
	for i, entry := range recovered.Entries {
		if entry.BlockIdx != uint32(i) {
			t.Errorf("entry[%d].BlockIdx = %d, want %d", i, entry.BlockIdx, i)
		}
		wantData := "hello"
		if i >= 3 {
			wantData = "world"
		}
		if string(entry.Data) != wantData {
			t.Errorf("entry[%d].Data = %s, want %s", i, entry.Data, wantData)
		}
	}
}

// ============================================================================
// Benchmark Tests - Performance
// ============================================================================

func BenchmarkMmapPersister_AppendBlockWrite_Small(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Small write (512 bytes)
	data := make([]byte, 512)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &BlockWriteEntry{
		PayloadID:     "bench-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          data,
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.BlockIdx = uint32(i)
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_AppendBlockWrite_Medium(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Medium write (32KB - typical NFS write size)
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &BlockWriteEntry{
		PayloadID:     "bench-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          data,
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.BlockIdx = uint32(i)
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_AppendBlockWrite_Large(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Large write (1MB)
	data := make([]byte, 1*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &BlockWriteEntry{
		PayloadID:     "bench-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          data,
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.BlockIdx = uint32(i)
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_Recover_100Entries(b *testing.B) {
	dir := b.TempDir()

	// Setup: write 100 entries
	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		entry := &BlockWriteEntry{
			PayloadID:     fmt.Sprintf("file-%d", i%10),
			ChunkIdx:      0,
			BlockIdx:      uint32(i),
			OffsetInBlock: 0,
			Data:          data,
		}
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}
	_ = p.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		p2, err := NewMmapPersister(dir)
		if err != nil {
			b.Fatalf("NewMmapPersister() error = %v", err)
		}

		_, err = p2.Recover()
		if err != nil {
			b.Fatalf("Recover() error = %v", err)
		}
		_ = p2.Close()
	}
}

func BenchmarkMmapPersister_Recover_1000Entries(b *testing.B) {
	dir := b.TempDir()

	// Setup: write 1000 entries
	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}

	data := make([]byte, 1024)
	for i := 0; i < 1000; i++ {
		entry := &BlockWriteEntry{
			PayloadID:     fmt.Sprintf("file-%d", i%10),
			ChunkIdx:      0,
			BlockIdx:      uint32(i),
			OffsetInBlock: 0,
			Data:          data,
		}
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}
	_ = p.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		p2, err := NewMmapPersister(dir)
		if err != nil {
			b.Fatalf("NewMmapPersister() error = %v", err)
		}

		_, err = p2.Recover()
		if err != nil {
			b.Fatalf("Recover() error = %v", err)
		}
		_ = p2.Close()
	}
}

func BenchmarkMmapPersister_Recover_WithRemoves(b *testing.B) {
	dir := b.TempDir()

	// Setup: write 500 entries across 50 files, then remove 25 files
	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}

	data := make([]byte, 1024)

	// Write entries for 50 files (10 entries each)
	for file := 0; file < 50; file++ {
		for blockIdx := 0; blockIdx < 10; blockIdx++ {
			entry := &BlockWriteEntry{
				PayloadID:     fmt.Sprintf("file-%02d", file),
				ChunkIdx:      0,
				BlockIdx:      uint32(blockIdx),
				OffsetInBlock: 0,
				Data:          data,
			}
			if err := p.AppendBlockWrite(entry); err != nil {
				b.Fatalf("AppendBlockWrite() error = %v", err)
			}
		}
	}

	// Remove half the files
	for file := 0; file < 50; file += 2 {
		if err := p.AppendRemove(fmt.Sprintf("file-%02d", file)); err != nil {
			b.Fatalf("AppendRemove() error = %v", err)
		}
	}
	_ = p.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		p2, err := NewMmapPersister(dir)
		if err != nil {
			b.Fatalf("NewMmapPersister() error = %v", err)
		}

		result, err := p2.Recover()
		if err != nil {
			b.Fatalf("Recover() error = %v", err)
		}

		// Verify we got 250 entries (half the files removed)
		if len(result.Entries) != 250 {
			b.Fatalf("Expected 250 entries, got %d", len(result.Entries))
		}
		_ = p2.Close()
	}
}

func BenchmarkMmapPersister_AppendRemove(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := p.AppendRemove(fmt.Sprintf("file-%d", i)); err != nil {
			b.Fatalf("AppendRemove() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_Sync(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Write some data first
	data := make([]byte, 1024)
	entry := &BlockWriteEntry{
		PayloadID:     "bench-file",
		ChunkIdx:      0,
		BlockIdx:      0,
		OffsetInBlock: 0,
		Data:          data,
	}

	if err := p.AppendBlockWrite(entry); err != nil {
		b.Fatalf("AppendBlockWrite() error = %v", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := p.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
	}
}

// BenchmarkMmapPersister_Throughput measures end-to-end throughput
// simulating realistic NFS write patterns.
func BenchmarkMmapPersister_Throughput(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer func() { _ = p.Close() }()

	// Simulate NFS write pattern: 32KB writes
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &BlockWriteEntry{
		PayloadID: "throughput-test",
		Data:      data,
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.ChunkIdx = uint32(i / 2048) // ~64MB per chunk
		entry.BlockIdx = uint32(i % 16)   // 16 blocks per chunk
		entry.OffsetInBlock = uint32((i % 128) * 32 * 1024)
		if err := p.AppendBlockWrite(entry); err != nil {
			b.Fatalf("AppendBlockWrite() error = %v", err)
		}
	}

	b.StopTimer()

	// Report final file size
	info, _ := p.file.Stat()
	b.ReportMetric(float64(info.Size())/1024/1024, "final_MB")
}
