package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMmapPersister_CreateNew(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	if !p.IsEnabled() {
		t.Error("IsEnabled() = false, want true")
	}

	// Verify file was created
	filePath := filepath.Join(dir, "cache.dat")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("cache.dat was not created")
	}
}

func TestMmapPersister_AppendSlice(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	entry := &SliceEntry{
		FileHandle: "test-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    10,
			Data:      []byte("0123456789"),
			State:     SliceStatePending,
			CreatedAt: time.Now(),
			BlockRefs: nil,
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Fatalf("AppendSlice() error = %v", err)
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

	now := time.Now()
	entries := []*SliceEntry{
		{
			FileHandle: "file1",
			ChunkIdx:   0,
			Slice: Slice{
				ID:        "11111111-1111-1111-1111-111111111111",
				Offset:    0,
				Length:    5,
				Data:      []byte("hello"),
				State:     SliceStatePending,
				CreatedAt: now,
			},
		},
		{
			FileHandle: "file1",
			ChunkIdx:   0,
			Slice: Slice{
				ID:        "22222222-2222-2222-2222-222222222222",
				Offset:    5,
				Length:    5,
				Data:      []byte("world"),
				State:     SliceStatePending,
				CreatedAt: now.Add(time.Second),
			},
		},
		{
			FileHandle: "file2",
			ChunkIdx:   1,
			Slice: Slice{
				ID:        "33333333-3333-3333-3333-333333333333",
				Offset:    100,
				Length:    4,
				Data:      []byte("test"),
				State:     SliceStateFlushed,
				CreatedAt: now.Add(2 * time.Second),
				BlockRefs: []BlockRef{
					{ID: "block-1", Size: 4},
				},
			},
		},
	}

	for _, e := range entries {
		if err := p1.AppendSlice(e); err != nil {
			t.Fatalf("AppendSlice() error = %v", err)
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
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != len(entries) {
		t.Fatalf("Recover() got %d entries, want %d", len(recovered), len(entries))
	}

	// Verify entries
	for i, got := range recovered {
		want := entries[i]
		if got.FileHandle != want.FileHandle {
			t.Errorf("entry[%d].FileHandle = %s, want %s", i, got.FileHandle, want.FileHandle)
		}
		if got.ChunkIdx != want.ChunkIdx {
			t.Errorf("entry[%d].ChunkIdx = %d, want %d", i, got.ChunkIdx, want.ChunkIdx)
		}
		if string(got.Data) != string(want.Data) {
			t.Errorf("entry[%d].Data = %s, want %s", i, got.Data, want.Data)
		}
		if got.State != want.State {
			t.Errorf("entry[%d].State = %v, want %v", i, got.State, want.State)
		}
		if len(got.BlockRefs) != len(want.BlockRefs) {
			t.Errorf("entry[%d].BlockRefs len = %d, want %d", i, len(got.BlockRefs), len(want.BlockRefs))
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
	entry1 := &SliceEntry{
		FileHandle: "file1",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "11111111-1111-1111-1111-111111111111",
			Offset:    0,
			Length:    5,
			Data:      []byte("hello"),
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}
	entry2 := &SliceEntry{
		FileHandle: "file2",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "22222222-2222-2222-2222-222222222222",
			Offset:    0,
			Length:    5,
			Data:      []byte("world"),
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	if err := p.AppendSlice(entry1); err != nil {
		t.Fatalf("AppendSlice(entry1) error = %v", err)
	}
	if err := p.AppendSlice(entry2); err != nil {
		t.Fatalf("AppendSlice(entry2) error = %v", err)
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
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should only have file2 (file1 was removed)
	if len(recovered) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered))
	}

	if string(recovered[0].FileHandle) != "file2" {
		t.Errorf("recovered[0].FileHandle = %s, want file2", recovered[0].FileHandle)
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
	entry := &SliceEntry{
		FileHandle: "test",
		Slice: Slice{
			ID:   "12345678-1234-1234-1234-123456789012",
			Data: []byte("data"),
		},
	}

	if err := p.AppendSlice(entry); err != ErrPersisterClosed {
		t.Errorf("AppendSlice() after close = %v, want ErrPersisterClosed", err)
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
	defer p.Close()

	// Write enough data to trigger file growth
	largeData := make([]byte, 10*1024*1024) // 10MB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	for i := 0; i < 10; i++ {
		entry := &SliceEntry{
			FileHandle: "large-file",
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        "12345678-1234-1234-1234-123456789012",
				Offset:    0,
				Length:    uint32(len(largeData)),
				Data:      largeData,
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			},
		}

		if err := p.AppendSlice(entry); err != nil {
			t.Fatalf("AppendSlice(%d) error = %v", i, err)
		}
	}
}


func TestSliceState_String(t *testing.T) {
	tests := []struct {
		state SliceState
		want  string
	}{
		{SliceStatePending, "Pending"},
		{SliceStateFlushed, "Flushed"},
		{SliceStateUploading, "Uploading"},
		{SliceState(99), "Unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("SliceState(%d).String() = %s, want %s", tt.state, got, tt.want)
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
	defer p.Close()

	// Recover from empty WAL should return empty slice
	entries, err := p.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("Recover() got %d entries, want 0", len(entries))
	}
}

func TestMmapPersister_LargeFileHandle(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	// Create entry with large file handle (512 bytes)
	largeHandle := make([]byte, 512)
	for i := range largeHandle {
		largeHandle[i] = byte('a' + i%26)
	}

	entry := &SliceEntry{
		FileHandle: string(largeHandle),
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    5,
			Data:      []byte("hello"),
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Fatalf("AppendSlice() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered))
	}

	if recovered[0].FileHandle != string(largeHandle) {
		t.Errorf("FileHandle mismatch after recovery")
	}
}

func TestMmapPersister_ManyBlockRefs(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	// Create entry with many block refs
	blockRefs := make([]BlockRef, 100)
	for i := range blockRefs {
		blockRefs[i] = BlockRef{
			ID:   fmt.Sprintf("block-%04d", i),
			Size: uint32(1024 * (i + 1)),
		}
	}

	entry := &SliceEntry{
		FileHandle: "test-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    5,
			Data:      []byte("hello"),
			State:     SliceStateFlushed,
			CreatedAt: time.Now(),
			BlockRefs: blockRefs,
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Fatalf("AppendSlice() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered))
	}

	if len(recovered[0].BlockRefs) != 100 {
		t.Fatalf("BlockRefs count = %d, want 100", len(recovered[0].BlockRefs))
	}

	for i, ref := range recovered[0].BlockRefs {
		wantID := fmt.Sprintf("block-%04d", i)
		wantSize := uint32(1024 * (i + 1))
		if ref.ID != wantID || ref.Size != wantSize {
			t.Errorf("BlockRef[%d] = {%s, %d}, want {%s, %d}", i, ref.ID, ref.Size, wantID, wantSize)
		}
	}
}

func TestMmapPersister_ZeroLengthData(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	entry := &SliceEntry{
		FileHandle: "empty-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    0,
			Data:      []byte{},
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Fatalf("AppendSlice() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and recover
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered))
	}

	if recovered[0].Length != 0 || len(recovered[0].Data) != 0 {
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
	p.Close()

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
	p.Close()

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
	defer p.Close()

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

	now := time.Now()

	// Write entries for multiple files with multiple chunks
	files := []struct {
		handle string
		chunks int
	}{
		{"file1", 3},
		{"file2", 5},
		{"file3", 2},
	}

	totalEntries := 0
	for _, f := range files {
		for chunk := 0; chunk < f.chunks; chunk++ {
			entry := &SliceEntry{
				FileHandle: f.handle,
				ChunkIdx:   uint32(chunk),
				Slice: Slice{
					ID:        fmt.Sprintf("%s-chunk%d", f.handle, chunk),
					Offset:    uint32(chunk * 1024),
					Length:    uint32(len(fmt.Sprintf("data-%s-%d", f.handle, chunk))),
					Data:      []byte(fmt.Sprintf("data-%s-%d", f.handle, chunk)),
					State:     SliceStatePending,
					CreatedAt: now.Add(time.Duration(totalEntries) * time.Second),
				},
			}
			if err := p.AppendSlice(entry); err != nil {
				t.Fatalf("AppendSlice() error = %v", err)
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
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != totalEntries {
		t.Fatalf("Recover() got %d entries, want %d", len(recovered), totalEntries)
	}

	// Verify data integrity
	idx := 0
	for _, f := range files {
		for chunk := 0; chunk < f.chunks; chunk++ {
			got := recovered[idx]
			if got.FileHandle != f.handle {
				t.Errorf("entry[%d].FileHandle = %s, want %s", idx, got.FileHandle, f.handle)
			}
			if got.ChunkIdx != uint32(chunk) {
				t.Errorf("entry[%d].ChunkIdx = %d, want %d", idx, got.ChunkIdx, chunk)
			}
			wantData := fmt.Sprintf("data-%s-%d", f.handle, chunk)
			if string(got.Data) != wantData {
				t.Errorf("entry[%d].Data = %s, want %s", idx, got.Data, wantData)
			}
			idx++
		}
	}
}

func TestMmapPersister_RemoveInterleavedWithSlices(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	now := time.Now()

	// Interleave slice and remove operations
	// file1: slice, slice
	// file2: slice
	// remove file1
	// file3: slice
	// file2: slice
	// remove file2
	// file3: slice

	operations := []struct {
		op     string
		handle string
		chunk  uint32
	}{
		{"slice", "file1", 0},
		{"slice", "file1", 1},
		{"slice", "file2", 0},
		{"remove", "file1", 0},
		{"slice", "file3", 0},
		{"slice", "file2", 1},
		{"remove", "file2", 0},
		{"slice", "file3", 1},
	}

	for i, op := range operations {
		if op.op == "slice" {
			entry := &SliceEntry{
				FileHandle: op.handle,
				ChunkIdx:   op.chunk,
				Slice: Slice{
					ID:        fmt.Sprintf("slice-%d", i),
					Offset:    0,
					Length:    5,
					Data:      []byte("hello"),
					State:     SliceStatePending,
					CreatedAt: now.Add(time.Duration(i) * time.Second),
				},
			}
			if err := p.AppendSlice(entry); err != nil {
				t.Fatalf("AppendSlice() error = %v", err)
			}
		} else {
			if err := p.AppendRemove(op.handle); err != nil {
				t.Fatalf("AppendRemove() error = %v", err)
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
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	// Should only have 2 entries (both for file3)
	if len(recovered) != 2 {
		t.Fatalf("Recover() got %d entries, want 2", len(recovered))
	}

	for _, entry := range recovered {
		if entry.FileHandle != "file3" {
			t.Errorf("Unexpected file handle: %s (expected file3)", entry.FileHandle)
		}
	}
}

func TestMmapPersister_RecoverPreservesState(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	now := time.Now()

	states := []SliceState{SliceStatePending, SliceStateUploading, SliceStateFlushed}
	for i, state := range states {
		entry := &SliceEntry{
			FileHandle: "test-file",
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        fmt.Sprintf("slice-%d", i),
				Offset:    0,
				Length:    5,
				Data:      []byte("hello"),
				State:     state,
				CreatedAt: now.Add(time.Duration(i) * time.Second),
			},
		}
		if err := p.AppendSlice(entry); err != nil {
			t.Fatalf("AppendSlice() error = %v", err)
		}
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and verify states are preserved
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != len(states) {
		t.Fatalf("Recover() got %d entries, want %d", len(recovered), len(states))
	}

	for i, entry := range recovered {
		if entry.State != states[i] {
			t.Errorf("entry[%d].State = %v, want %v", i, entry.State, states[i])
		}
	}
}

func TestMmapPersister_RecoverPreservesTimestamp(t *testing.T) {
	dir := t.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	// Use a specific timestamp (nanosecond precision)
	createdAt := time.Date(2025, 6, 15, 10, 30, 45, 123456789, time.UTC)

	entry := &SliceEntry{
		FileHandle: "test-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "test-slice",
			Offset:    0,
			Length:    5,
			Data:      []byte("hello"),
			State:     SliceStatePending,
			CreatedAt: createdAt,
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Fatalf("AppendSlice() error = %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopen and verify timestamp is preserved
	p2, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() reopen error = %v", err)
	}
	defer p2.Close()

	recovered, err := p2.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != 1 {
		t.Fatalf("Recover() got %d entries, want 1", len(recovered))
	}

	if !recovered[0].CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", recovered[0].CreatedAt, createdAt)
	}
}

func TestMmapPersister_AppendAfterRecovery(t *testing.T) {
	dir := t.TempDir()

	// First session: write some entries
	p1, err := NewMmapPersister(dir)
	if err != nil {
		t.Fatalf("NewMmapPersister() error = %v", err)
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		entry := &SliceEntry{
			FileHandle: "test-file",
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        fmt.Sprintf("slice-%d", i),
				Offset:    0,
				Length:    5,
				Data:      []byte("hello"),
				State:     SliceStatePending,
				CreatedAt: now.Add(time.Duration(i) * time.Second),
			},
		}
		if err := p1.AppendSlice(entry); err != nil {
			t.Fatalf("AppendSlice() error = %v", err)
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
		entry := &SliceEntry{
			FileHandle: "test-file",
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        fmt.Sprintf("slice-%d", i),
				Offset:    0,
				Length:    5,
				Data:      []byte("world"),
				State:     SliceStatePending,
				CreatedAt: now.Add(time.Duration(i) * time.Second),
			},
		}
		if err := p2.AppendSlice(entry); err != nil {
			t.Fatalf("AppendSlice() error = %v", err)
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
	defer p3.Close()

	recovered, err := p3.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if len(recovered) != 6 {
		t.Fatalf("Recover() got %d entries, want 6", len(recovered))
	}

	// Verify data from both sessions
	for i, entry := range recovered {
		if entry.ChunkIdx != uint32(i) {
			t.Errorf("entry[%d].ChunkIdx = %d, want %d", i, entry.ChunkIdx, i)
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

func BenchmarkMmapPersister_AppendSlice_Small(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	// Small slice (512 bytes)
	data := make([]byte, 512)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &SliceEntry{
		FileHandle: "bench-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    uint32(len(data)),
			Data:      data,
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.ChunkIdx = uint32(i)
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_AppendSlice_Medium(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	// Medium slice (32KB - typical NFS write size)
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &SliceEntry{
		FileHandle: "bench-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    uint32(len(data)),
			Data:      data,
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.ChunkIdx = uint32(i)
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
		}
	}
}

func BenchmarkMmapPersister_AppendSlice_Large(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

	// Large slice (1MB)
	data := make([]byte, 1*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &SliceEntry{
		FileHandle: "bench-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    uint32(len(data)),
			Data:      data,
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.ChunkIdx = uint32(i)
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
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

	now := time.Now()
	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		entry := &SliceEntry{
			FileHandle: fmt.Sprintf("file-%d", i%10),
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        fmt.Sprintf("slice-%04d", i),
				Offset:    0,
				Length:    uint32(len(data)),
				Data:      data,
				State:     SliceStatePending,
				CreatedAt: now,
			},
		}
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
		}
	}
	p.Close()

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
		p2.Close()
	}
}

func BenchmarkMmapPersister_Recover_1000Entries(b *testing.B) {
	dir := b.TempDir()

	// Setup: write 1000 entries
	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}

	now := time.Now()
	data := make([]byte, 1024)
	for i := 0; i < 1000; i++ {
		entry := &SliceEntry{
			FileHandle: fmt.Sprintf("file-%d", i%10),
			ChunkIdx:   uint32(i),
			Slice: Slice{
				ID:        fmt.Sprintf("slice-%04d", i),
				Offset:    0,
				Length:    uint32(len(data)),
				Data:      data,
				State:     SliceStatePending,
				CreatedAt: now,
			},
		}
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
		}
	}
	p.Close()

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
		p2.Close()
	}
}

func BenchmarkMmapPersister_Recover_WithRemoves(b *testing.B) {
	dir := b.TempDir()

	// Setup: write 500 entries across 50 files, then remove 25 files
	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}

	now := time.Now()
	data := make([]byte, 1024)

	// Write entries for 50 files (10 entries each)
	for file := 0; file < 50; file++ {
		for chunk := 0; chunk < 10; chunk++ {
			entry := &SliceEntry{
				FileHandle: fmt.Sprintf("file-%02d", file),
				ChunkIdx:   uint32(chunk),
				Slice: Slice{
					ID:        fmt.Sprintf("slice-%02d-%02d", file, chunk),
					Offset:    0,
					Length:    uint32(len(data)),
					Data:      data,
					State:     SliceStatePending,
					CreatedAt: now,
				},
			}
			if err := p.AppendSlice(entry); err != nil {
				b.Fatalf("AppendSlice() error = %v", err)
			}
		}
	}

	// Remove half the files
	for file := 0; file < 50; file += 2 {
		if err := p.AppendRemove(fmt.Sprintf("file-%02d", file)); err != nil {
			b.Fatalf("AppendRemove() error = %v", err)
		}
	}
	p.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		p2, err := NewMmapPersister(dir)
		if err != nil {
			b.Fatalf("NewMmapPersister() error = %v", err)
		}

		entries, err := p2.Recover()
		if err != nil {
			b.Fatalf("Recover() error = %v", err)
		}

		// Verify we got 250 entries (half the files removed)
		if len(entries) != 250 {
			b.Fatalf("Expected 250 entries, got %d", len(entries))
		}
		p2.Close()
	}
}

func BenchmarkMmapPersister_AppendRemove(b *testing.B) {
	dir := b.TempDir()

	p, err := NewMmapPersister(dir)
	if err != nil {
		b.Fatalf("NewMmapPersister() error = %v", err)
	}
	defer p.Close()

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
	defer p.Close()

	// Write some data first
	data := make([]byte, 1024)
	entry := &SliceEntry{
		FileHandle: "bench-file",
		ChunkIdx:   0,
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Offset:    0,
			Length:    uint32(len(data)),
			Data:      data,
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	if err := p.AppendSlice(entry); err != nil {
		b.Fatalf("AppendSlice() error = %v", err)
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
	defer p.Close()

	// Simulate NFS write pattern: 32KB writes
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	entry := &SliceEntry{
		FileHandle: "throughput-test",
		Slice: Slice{
			ID:        "12345678-1234-1234-1234-123456789012",
			Length:    uint32(len(data)),
			Data:      data,
			State:     SliceStatePending,
			CreatedAt: time.Now(),
		},
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		entry.ChunkIdx = uint32(i / 2048)     // ~64MB per chunk
		entry.Offset = uint32((i % 2048) * 32 * 1024)
		if err := p.AppendSlice(entry); err != nil {
			b.Fatalf("AppendSlice() error = %v", err)
		}
	}

	b.StopTimer()

	// Report final file size
	info, _ := p.file.Stat()
	b.ReportMetric(float64(info.Size())/1024/1024, "final_MB")
}
