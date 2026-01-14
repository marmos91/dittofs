package wal

import (
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
		SliceID:    "12345678-1234-1234-1234-123456789012",
		Offset:     0,
		Length:     10,
		Data:       []byte("0123456789"),
		State:      SliceStatePending,
		CreatedAt:  time.Now(),
		BlockRefs:  nil,
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
			SliceID:    "11111111-1111-1111-1111-111111111111",
			Offset:     0,
			Length:     5,
			Data:       []byte("hello"),
			State:      SliceStatePending,
			CreatedAt:  now,
		},
		{
			FileHandle: "file1",
			ChunkIdx:   0,
			SliceID:    "22222222-2222-2222-2222-222222222222",
			Offset:     5,
			Length:     5,
			Data:       []byte("world"),
			State:      SliceStatePending,
			CreatedAt:  now.Add(time.Second),
		},
		{
			FileHandle: "file2",
			ChunkIdx:   1,
			SliceID:    "33333333-3333-3333-3333-333333333333",
			Offset:     100,
			Length:     4,
			Data:       []byte("test"),
			State:      SliceStateFlushed,
			CreatedAt:  now.Add(2 * time.Second),
			BlockRefs: []BlockRef{
				{ID: "block-1", Size: 4},
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
		SliceID:    "11111111-1111-1111-1111-111111111111",
		Offset:     0,
		Length:     5,
		Data:       []byte("hello"),
		State:      SliceStatePending,
		CreatedAt:  time.Now(),
	}
	entry2 := &SliceEntry{
		FileHandle: "file2",
		ChunkIdx:   0,
		SliceID:    "22222222-2222-2222-2222-222222222222",
		Offset:     0,
		Length:     5,
		Data:       []byte("world"),
		State:      SliceStatePending,
		CreatedAt:  time.Now(),
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
		SliceID:    "12345678-1234-1234-1234-123456789012",
		Data:       []byte("data"),
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
			SliceID:    "12345678-1234-1234-1234-123456789012",
			Offset:     0,
			Length:     uint32(len(largeData)),
			Data:       largeData,
			State:      SliceStatePending,
			CreatedAt:  time.Now(),
		}

		if err := p.AppendSlice(entry); err != nil {
			t.Fatalf("AppendSlice(%d) error = %v", i, err)
		}
	}
}

func TestNullPersister(t *testing.T) {
	p := NewNullPersister()

	if p.IsEnabled() {
		t.Error("NullPersister.IsEnabled() = true, want false")
	}

	// All operations should succeed silently
	entry := &SliceEntry{
		FileHandle: "test",
		SliceID:    "12345678-1234-1234-1234-123456789012",
		Data:       []byte("data"),
	}

	if err := p.AppendSlice(entry); err != nil {
		t.Errorf("AppendSlice() error = %v, want nil", err)
	}

	if err := p.AppendRemove("test"); err != nil {
		t.Errorf("AppendRemove() error = %v, want nil", err)
	}

	if err := p.Sync(); err != nil {
		t.Errorf("Sync() error = %v, want nil", err)
	}

	recovered, err := p.Recover()
	if err != nil {
		t.Errorf("Recover() error = %v, want nil", err)
	}
	if recovered != nil {
		t.Errorf("Recover() = %v, want nil", recovered)
	}

	if err := p.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
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
