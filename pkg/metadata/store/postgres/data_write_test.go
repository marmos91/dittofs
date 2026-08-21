//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// pgStore opens the localhost Postgres the other postgres_test.go files use.
// DITTOFS_TEST_POSTGRES_DSN is a boolean gate, not a parsed DSN.
func pgStore(tb testing.TB) metadata.Store {
	tb.Helper()
	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "dittofs_test",
		User:        "postgres",
		Password:    "postgres",
		SSLMode:     "disable",
		AutoMigrate: true,
	}
	caps := metadata.FilesystemCapabilities{
		MaxReadSize: 1048576, PreferredReadSize: 1048576,
		MaxWriteSize: 1048576, PreferredWriteSize: 1048576,
		MaxFileSize: 9223372036854775807, MaxFilenameLen: 255,
		MaxPathLen: 4096, MaxHardLinkCount: 32767,
		SupportsHardLinks: true, SupportsSymlinks: true,
		CaseSensitive: true, CasePreserving: true, TimestampResolution: 1,
	}
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		tb.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}

// uniqueShare keeps repeated runs against the same persistent database from
// colliding on share names.
func uniqueShare(prefix string) string {
	return fmt.Sprintf("/%s-%d", prefix, time.Now().UnixNano())
}

// TestApplyDataWrite_Sound_Postgres is the Postgres twin of the sqlite
// soundness test: it drives the DataWriteApplier fast path through a real
// transaction and asserts the row ends in the state the GetFile+UpdateAttrs
// fallback would have produced.
func TestApplyDataWrite_Sound_Postgres(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL data-write tests")
	}
	ctx := context.Background()
	store := pgStore(t)

	share := uniqueShare("dw")
	if _, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		t.Fatal(err)
	}

	apply := func(h metadata.FileHandle, newSize uint64, now time.Time, clearSUID bool) (uint64, error) {
		var final uint64
		err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			applier, ok := tx.(metadata.DataWriteApplier)
			if !ok {
				t.Fatal("postgres transaction does not implement DataWriteApplier")
			}
			var e error
			final, e = applier.ApplyDataWrite(ctx, h, newSize, now, clearSUID)
			return e
		})
		return final, err
	}

	mk := func(name string, mode uint32, size uint64) metadata.FileHandle {
		fp := share + "/" + name
		h, err := store.GenerateHandle(ctx, share, fp)
		if err != nil {
			t.Fatal(err)
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateAttrs(ctx, &metadata.File{ShareName: share, Path: fp, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode, UID: 1000, GID: 1000, Size: size}}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetParent(ctx, h, rootHandle); err != nil {
			t.Fatal(err)
		}
		if err := store.SetChild(ctx, rootHandle, name, h); err != nil {
			t.Fatal(err)
		}
		return h
	}

	t.Run("grows size and stamps times", func(t *testing.T) {
		h := mk("grow", 0o644, 100)
		now := time.Now().Truncate(time.Second).Add(time.Hour)
		final, err := apply(h, 4096, now, false)
		if err != nil {
			t.Fatal(err)
		}
		if final != 4096 {
			t.Fatalf("final size = %d, want 4096", final)
		}
		f, err := store.GetFile(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if f.Size != 4096 {
			t.Fatalf("stored size = %d, want 4096", f.Size)
		}
		if !f.Mtime.Equal(now) || !f.Ctime.Equal(now) {
			t.Fatalf("mtime/ctime not stamped: mtime=%v ctime=%v want %v", f.Mtime, f.Ctime, now)
		}
	})

	t.Run("never shrinks (out-of-order write)", func(t *testing.T) {
		h := mk("noshrink", 0o644, 8192)
		final, err := apply(h, 4096, time.Now(), false)
		if err != nil {
			t.Fatal(err)
		}
		if final != 8192 {
			t.Fatalf("final size = %d, want 8192 (must not shrink)", final)
		}
		f, err := store.GetFile(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if f.Size != 8192 {
			t.Fatalf("stored size = %d, want 8192", f.Size)
		}
	})

	t.Run("clears setuid/setgid for non-root", func(t *testing.T) {
		h := mk("suid", 0o6755, 100)
		if _, err := apply(h, 200, time.Now(), true); err != nil {
			t.Fatal(err)
		}
		f, err := store.GetFile(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if f.Mode&0o6000 != 0 {
			t.Fatalf("setuid/setgid not cleared: mode=0%o", f.Mode)
		}
		if f.Mode&0o777 != 0o755 {
			t.Fatalf("permission bits altered: mode=0%o, want low bits 0755", f.Mode)
		}
	})

	t.Run("keeps setuid when clearSUID is false", func(t *testing.T) {
		h := mk("keepsuid", 0o6755, 100)
		if _, err := apply(h, 200, time.Now(), false); err != nil {
			t.Fatal(err)
		}
		f, err := store.GetFile(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if f.Mode&0o6000 != 0o6000 {
			t.Fatalf("setuid/setgid wrongly cleared: mode=0%o", f.Mode)
		}
	})

	t.Run("missing file returns ErrNotFound", func(t *testing.T) {
		h, err := store.GenerateHandle(ctx, share, share+"/ghost")
		if err != nil {
			t.Fatal(err)
		}
		_, err = apply(h, 100, time.Now(), false)
		var se *metadata.StoreError
		if !asPgStoreErr(err, &se) || se.Code != metadata.ErrNotFound {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("directory returns ErrIsDirectory", func(t *testing.T) {
		_, err := apply(rootHandle, 100, time.Now(), false)
		var se *metadata.StoreError
		if !asPgStoreErr(err, &se) || se.Code != metadata.ErrIsDirectory {
			t.Fatalf("want ErrIsDirectory, got %v", err)
		}
	})
}

func asPgStoreErr(err error, target **metadata.StoreError) bool {
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
