package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func newTestStore(t *testing.T) *memory.MemoryMetadataStore {
	t.Helper()
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a share and root directory.
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if err := tx.CreateShare(ctx, &metadata.Share{Name: "/test"}); err != nil {
			return err
		}
		_, err := tx.CreateRootDirectory(ctx, "/test", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		})
		return err
	})
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	return store
}

func createFile(t *testing.T, store *memory.MemoryMetadataStore, name string, size uint64) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()

	var handle metadata.FileHandle
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		rootHandle, err := tx.GetRootHandle(ctx, "/test")
		if err != nil {
			return err
		}
		h, err := tx.GenerateHandle(ctx, "/test", "/"+name)
		if err != nil {
			return err
		}
		handle = h

		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			return err
		}

		now := time.Now()
		file := &metadata.File{
			ID:        id,
			ShareName: "/test",
			Path:      "/" + name,
			FileAttr: metadata.FileAttr{
				Type:  metadata.FileTypeRegular,
				Mode:  0o644,
				Size:  size,
				Atime: now,
				Mtime: now,
				Ctime: now,
			},
		}
		if err := tx.PutFile(ctx, file); err != nil {
			return err
		}
		return tx.SetChild(ctx, rootHandle, name, h)
	})
	if err != nil {
		t.Fatalf("createFile(%s) failed: %v", name, err)
	}
	return handle
}

// TestCounter_InitialZero verifies new store has usedBytes == 0.
func TestCounter_InitialZero(t *testing.T) {
	t.Parallel()
	store := memory.NewMemoryMetadataStoreWithDefaults()

	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("expected usedBytes == 0, got %d", got)
	}
}

// TestCounter_CreateFile verifies usedBytes increases on file creation.
func TestCounter_CreateFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	createFile(t, store, "file1.txt", 1000)

	if got := store.GetUsedBytes(); got != 1000 {
		t.Fatalf("expected usedBytes == 1000 after creating 1000-byte file, got %d", got)
	}
}

// TestCounter_UpdateFileSize verifies usedBytes adjusts on size change (PutFile update).
func TestCounter_UpdateFileSize(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	handle := createFile(t, store, "file1.txt", 1000)

	if got := store.GetUsedBytes(); got != 1000 {
		t.Fatalf("expected usedBytes == 1000 after create, got %d", got)
	}

	// Update file size from 1000 to 5000.
	ctx := context.Background()
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			return err
		}
		now := time.Now()
		return tx.PutFile(ctx, &metadata.File{
			ID:        id,
			ShareName: "/test",
			Path:      "/file1.txt",
			FileAttr: metadata.FileAttr{
				Type:  metadata.FileTypeRegular,
				Mode:  0o644,
				Size:  5000,
				Atime: now,
				Mtime: now,
				Ctime: now,
			},
		})
	})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}

	if got := store.GetUsedBytes(); got != 5000 {
		t.Fatalf("expected usedBytes == 5000 after update to 5000, got %d", got)
	}
}

// TestCounter_Truncate verifies usedBytes decreases when file is truncated (PutFile with smaller size).
func TestCounter_Truncate(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	handle := createFile(t, store, "file1.txt", 5000)

	// Truncate: update size from 5000 to 500.
	ctx := context.Background()
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			return err
		}
		now := time.Now()
		return tx.PutFile(ctx, &metadata.File{
			ID:        id,
			ShareName: "/test",
			Path:      "/file1.txt",
			FileAttr: metadata.FileAttr{
				Type:  metadata.FileTypeRegular,
				Mode:  0o644,
				Size:  500,
				Atime: now,
				Mtime: now,
				Ctime: now,
			},
		})
	})
	if err != nil {
		t.Fatalf("truncate failed: %v", err)
	}

	if got := store.GetUsedBytes(); got != 500 {
		t.Fatalf("expected usedBytes == 500 after truncate, got %d", got)
	}
}

// TestCounter_RemoveFile verifies usedBytes goes to 0 after removing the file.
func TestCounter_RemoveFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	handle := createFile(t, store, "file1.txt", 1000)

	ctx := context.Background()
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		rootHandle, err := tx.GetRootHandle(ctx, "/test")
		if err != nil {
			return err
		}
		if err := tx.DeleteChild(ctx, rootHandle, "file1.txt"); err != nil {
			return err
		}
		return tx.DeleteFile(ctx, handle)
	})
	if err != nil {
		t.Fatalf("remove failed: %v", err)
	}

	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("expected usedBytes == 0 after remove, got %d", got)
	}
}

// TestCounter_DirectoryIgnored verifies that creating a directory does NOT change usedBytes.
func TestCounter_DirectoryIgnored(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	ctx := context.Background()
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		rootHandle, err := tx.GetRootHandle(ctx, "/test")
		if err != nil {
			return err
		}
		h, err := tx.GenerateHandle(ctx, "/test", "/subdir")
		if err != nil {
			return err
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			return err
		}
		now := time.Now()
		dir := &metadata.File{
			ID:        id,
			ShareName: "/test",
			Path:      "/subdir",
			FileAttr: metadata.FileAttr{
				Type:  metadata.FileTypeDirectory,
				Mode:  0o755,
				Size:  4096, // Directories report a size but it shouldn't count.
				Atime: now,
				Mtime: now,
				Ctime: now,
			},
		}
		if err := tx.PutFile(ctx, dir); err != nil {
			return err
		}
		return tx.SetChild(ctx, rootHandle, "subdir", h)
	})
	if err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("expected usedBytes == 0 after creating directory, got %d", got)
	}
}

// TestCounter_StatisticsMatch verifies GetFilesystemStatistics returns UsedBytes matching the atomic counter.
func TestCounter_StatisticsMatch(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	handle := createFile(t, store, "file1.txt", 2048)

	ctx := context.Background()

	// Get a valid handle to pass to GetFilesystemStatistics.
	stats, err := store.GetFilesystemStatistics(ctx, handle)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics failed: %v", err)
	}

	counterVal := store.GetUsedBytes()
	if stats.UsedBytes != uint64(counterVal) {
		t.Fatalf("GetFilesystemStatistics.UsedBytes=%d does not match GetUsedBytes()=%d",
			stats.UsedBytes, counterVal)
	}
}
