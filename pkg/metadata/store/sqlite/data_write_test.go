package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// apply runs ApplyDataWrite through the DataWriteApplier interface inside a real
// transaction, the same way io.go's fast path does.
func apply(t *testing.T, store metadata.Store, h metadata.FileHandle, newSize uint64, now time.Time, clearSUID bool) (uint64, error) {
	t.Helper()
	var final uint64
	err := store.WithTransaction(context.Background(), func(tx metadata.Transaction) error {
		applier, ok := tx.(metadata.DataWriteApplier)
		if !ok {
			t.Fatal("sqlite transaction does not implement DataWriteApplier")
		}
		var e error
		final, e = applier.ApplyDataWrite(context.Background(), h, newSize, now, clearSUID)
		return e
	})
	return final, err
}

func TestApplyDataWrite_Sound(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.NewSQLiteMetadataStore(ctx,
		&sqlite.SQLiteMetadataStoreConfig{Path: t.TempDir() + "/m.db", AutoMigrate: true}, sqliteTestCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const share = "/s"
	if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		t.Fatal(err)
	}

	// A regular file, mode with setuid+setgid set, initial size 100.
	mk := func(name string, mode uint32, size uint64) metadata.FileHandle {
		h, err := store.GenerateHandle(ctx, share, "/s/"+name)
		if err != nil {
			t.Fatal(err)
		}
		_, id, _ := metadata.DecodeFileHandle(h)
		if err := store.UpdateAttrs(ctx, &metadata.File{ShareName: share, Path: "/s/" + name, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode, UID: 1000, GID: 1000, Size: size}}); err != nil {
			t.Fatal(err)
		}
		_ = store.SetParent(ctx, h, rootHandle)
		_ = store.SetChild(ctx, rootHandle, name, h)
		return h
	}

	t.Run("grows size and stamps times", func(t *testing.T) {
		h := mk("grow", 0o644, 100)
		now := time.Now().Truncate(time.Second).Add(time.Hour)
		final, err := apply(t, store, h, 4096, now, false)
		if err != nil {
			t.Fatal(err)
		}
		if final != 4096 {
			t.Fatalf("final size = %d, want 4096", final)
		}
		f, _ := store.GetFile(ctx, h)
		if f.Size != 4096 {
			t.Fatalf("stored size = %d, want 4096", f.Size)
		}
		if !f.Mtime.Equal(now) || !f.Ctime.Equal(now) {
			t.Fatalf("mtime/ctime not stamped: mtime=%v ctime=%v want %v", f.Mtime, f.Ctime, now)
		}
	})

	t.Run("never shrinks (out-of-order write)", func(t *testing.T) {
		h := mk("noshrink", 0o644, 8192)
		final, err := apply(t, store, h, 4096, time.Now(), false)
		if err != nil {
			t.Fatal(err)
		}
		if final != 8192 {
			t.Fatalf("final size = %d, want 8192 (must not shrink)", final)
		}
		f, _ := store.GetFile(ctx, h)
		if f.Size != 8192 {
			t.Fatalf("stored size = %d, want 8192", f.Size)
		}
	})

	t.Run("clears setuid/setgid for non-root", func(t *testing.T) {
		h := mk("suid", 0o6755, 100)
		if _, err := apply(t, store, h, 200, time.Now(), true); err != nil {
			t.Fatal(err)
		}
		f, _ := store.GetFile(ctx, h)
		if f.Mode&0o6000 != 0 {
			t.Fatalf("setuid/setgid not cleared: mode=0%o", f.Mode)
		}
		if f.Mode&0o777 != 0o755 {
			t.Fatalf("permission bits altered: mode=0%o, want low bits 0755", f.Mode)
		}
	})

	t.Run("keeps setuid when clearSUID is false", func(t *testing.T) {
		h := mk("keepsuid", 0o6755, 100)
		if _, err := apply(t, store, h, 200, time.Now(), false); err != nil {
			t.Fatal(err)
		}
		f, _ := store.GetFile(ctx, h)
		if f.Mode&0o6000 != 0o6000 {
			t.Fatalf("setuid/setgid wrongly cleared: mode=0%o", f.Mode)
		}
	})

	t.Run("missing file returns ErrNotFound", func(t *testing.T) {
		h, _ := store.GenerateHandle(ctx, share, "/s/ghost")
		_, err := apply(t, store, h, 100, time.Now(), false)
		var se *metadata.StoreError
		if !asStoreErr(err, &se) || se.Code != metadata.ErrNotFound {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("directory returns ErrIsDirectory", func(t *testing.T) {
		_, err := apply(t, store, rootHandle, 100, time.Now(), false)
		var se *metadata.StoreError
		if !asStoreErr(err, &se) || se.Code != metadata.ErrIsDirectory {
			t.Fatalf("want ErrIsDirectory, got %v", err)
		}
	})
}

func asStoreErr(err error, target **metadata.StoreError) bool {
	for err != nil {
		if se, ok := err.(*metadata.StoreError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
