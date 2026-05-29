//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// isAlreadyExists reports whether err is a StoreError carrying the
// ErrAlreadyExists code.
func isAlreadyExists(err error) bool {
	var se *metadata.StoreError
	return errors.As(err, &se) && se.Code == metadata.ErrAlreadyExists
}

// newConflictTestStore builds a PostgresMetadataStore from
// DITTOFS_TEST_POSTGRES_DSN. Separate from the legacy hardcoded factory so
// it can run against the dedicated test database.
func newConflictTestStore(t *testing.T) *postgres.PostgresMetadataStore {
	t.Helper()
	dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping object_id conflict mapping test")
	}
	cfg := &postgres.PostgresMetadataStoreConfig{SSLMode: "disable", AutoMigrate: true}
	for _, kv := range strings.Fields(dsn) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "host":
			cfg.Host = parts[1]
		case "port":
			p, err := strconv.Atoi(parts[1])
			if err != nil {
				t.Fatalf("parse port: %v", err)
			}
			cfg.Port = p
		case "user":
			cfg.User = parts[1]
		case "password":
			cfg.Password = parts[1]
		case "dbname", "database":
			cfg.Database = parts[1]
		case "sslmode", "ssl_mode":
			cfg.SSLMode = parts[1]
		}
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
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func conflictTestRoot(t *testing.T, store *postgres.PostgresMetadataStore, shareName string) {
	t.Helper()
	// CreateRootDirectory inserts BOTH the "/" files-row and the share row
	// (Postgres' shares.root_file_id is NOT NULL).
	if _, err := store.CreateRootDirectory(context.Background(), shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
}

func conflictTestFile(t *testing.T, store *postgres.PostgresMetadataStore, shareName, name string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/" + name,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			PayloadID: metadata.PayloadID(strings.TrimPrefix(shareName, "/") + "/" + name),
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile %q: %v", name, err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount %q: %v", name, err)
	}
	return handle
}

// TestPostgresPutFile_ObjectIDConflictMapsToErrConflict verifies that a
// files_object_id_idx uniqueness violation (two files claiming the same
// Merkle-root ObjectID — the file-level dedup contention) surfaces as
// mderrors.ErrConflict, NOT the generic ErrAlreadyExists. The rollup-persist
// path depends on this code so it can recognise the benign conflict and
// persist the duplicate's blocks without claiming the dedup pointer.
func TestPostgresPutFile_ObjectIDConflictMapsToErrConflict(t *testing.T) {
	store := newConflictTestStore(t)
	ctx := context.Background()

	const shareName = "oidconf"
	conflictTestRoot(t, store, shareName)

	hA := conflictTestFile(t, store, shareName, "a.bin")
	hB := conflictTestFile(t, store, shareName, "b.bin")

	contested := blockstore.ComputeObjectID([]blockstore.BlockRef{
		{Hash: blockstore.ContentHash{1, 2, 3, 4}, Offset: 0, Size: 4096},
	})

	// First claimant wins.
	fA, err := store.GetFile(ctx, hA)
	if err != nil {
		t.Fatalf("GetFile A: %v", err)
	}
	fA.ObjectID = contested
	fA.Blocks = []blockstore.BlockRef{{Hash: blockstore.ContentHash{1, 2, 3, 4}, Offset: 0, Size: 4096}}
	if err := store.PutFile(ctx, fA); err != nil {
		t.Fatalf("PutFile A (first claimant): %v", err)
	}

	// Second claimant must lose with ErrConflict (NOT ErrAlreadyExists).
	fB, err := store.GetFile(ctx, hB)
	if err != nil {
		t.Fatalf("GetFile B: %v", err)
	}
	fB.ObjectID = contested
	fB.Blocks = []blockstore.BlockRef{{Hash: blockstore.ContentHash{1, 2, 3, 4}, Offset: 0, Size: 4096}}
	err = store.PutFile(ctx, fB)
	if err == nil {
		t.Fatal("PutFile B should have failed with object_id conflict")
	}
	if !mderrors.IsConflictError(err) {
		t.Fatalf("object_id conflict must map to ErrConflict, got %v", err)
	}
	if isAlreadyExists(err) {
		t.Fatalf("object_id conflict must NOT map to ErrAlreadyExists (would be indistinguishable from a path collision): %v", err)
	}
}

// TestPostgresPutFile_PathConflictStaysErrAlreadyExists verifies the path-hash
// uniqueness contract is preserved: a second active file at the same
// (share, path) still maps to ErrAlreadyExists (NOT ErrConflict), so the
// error-mapping split does not weaken path uniqueness.
func TestPostgresPutFile_PathConflictStaysErrAlreadyExists(t *testing.T) {
	store := newConflictTestStore(t)
	ctx := context.Background()

	const shareName = "pathconf"
	conflictTestRoot(t, store, shareName)

	_ = conflictTestFile(t, store, shareName, "dup.bin")

	// A DIFFERENT id at the SAME path with nlink>0 collides on
	// unique_share_path_hash_active.
	dupFile := &metadata.File{
		ID:        uuid.New(),
		ShareName: shareName,
		Path:      "/dup.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		},
	}
	if perr := store.PutFile(ctx, dupFile); perr == nil {
		t.Fatal("PutFile at duplicate path should have failed")
	} else if !isAlreadyExists(perr) {
		t.Fatalf("path collision must stay ErrAlreadyExists, got %v", perr)
	}
}
