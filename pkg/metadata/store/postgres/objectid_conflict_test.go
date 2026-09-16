//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/block"
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

// newConflictTestStore opens the shared Postgres test store.
// DITTOFS_TEST_POSTGRES_DSN is a BOOLEAN GATE here, as it is everywhere else in
// this package: any non-empty value opts in and the value itself is not parsed,
// so a URL-form DSN opts these tests in exactly like a keyword-form one.
func newConflictTestStore(t *testing.T) *postgres.PostgresMetadataStore {
	t.Helper()
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping object_id conflict mapping test")
	}
	return newPostgresStore(t)
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
	if err := store.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs %q: %v", name, err)
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

	// object_id uniqueness is what this test provokes, so a fixed hash would
	// collide with the row an earlier run against the same persistent database
	// left behind and fail on the FIRST claimant instead of the second.
	var hash block.ContentHash
	copy(hash[:], uuid.New().String())
	chunk := block.ChunkRef{Hash: hash, Offset: 0, Size: 4096}
	contested := block.ComputeObjectID([]block.ChunkRef{chunk})

	// First claimant wins.
	fA, err := store.GetFile(ctx, hA)
	if err != nil {
		t.Fatalf("GetFile A: %v", err)
	}
	fA.ObjectID = contested
	fA.Blocks = []block.ChunkRef{chunk}
	if err := store.UpdateAttrs(ctx, fA); err != nil {
		t.Fatalf("UpdateAttrs A (first claimant): %v", err)
	}

	// Second claimant must lose with ErrConflict (NOT ErrAlreadyExists).
	fB, err := store.GetFile(ctx, hB)
	if err != nil {
		t.Fatalf("GetFile B: %v", err)
	}
	fB.ObjectID = contested
	fB.Blocks = []block.ChunkRef{chunk}
	err = store.UpdateAttrs(ctx, fB)
	if err == nil {
		t.Fatal("UpdateAttrs B should have failed with object_id conflict")
	}
	if !mderrors.IsConflictError(err) {
		t.Fatalf("object_id conflict must map to ErrConflict, got %v", err)
	}
	if isAlreadyExists(err) {
		t.Fatalf("object_id conflict must NOT map to ErrAlreadyExists (would be indistinguishable from a path collision): %v", err)
	}
}

// TestPostgresPutFile_DuplicatePathAllowed pins the post-#1165 contract: the
// store no longer enforces path-hash uniqueness. Migration 000031 dropped
// unique_share_path_hash_active because it was incompatible with hard links (a
// rename overwriting a multiply-linked destination momentarily leaves two
// active rows sharing a path_hash — pjdfstest rename/23, #1160). Namespace
// uniqueness — no two entries with the same name in a directory — is enforced
// by parent_child_map UNIQUE(parent_id, child_name), NOT files.path_hash, so a
// raw UpdateAttrs of a DIFFERENT id at the SAME (share, path) with nlink>0 must now
// succeed instead of failing with ErrAlreadyExists. This guards against the
// index being reintroduced (which would re-break hard-link rename on postgres).
func TestPostgresPutFile_DuplicatePathAllowed(t *testing.T) {
	store := newConflictTestStore(t)
	ctx := context.Background()

	const shareName = "pathconf"
	conflictTestRoot(t, store, shareName)

	_ = conflictTestFile(t, store, shareName, "dup.bin")

	// A DIFFERENT id at the SAME path with nlink>0 used to collide on
	// unique_share_path_hash_active; with that index dropped it is accepted.
	dupFile := &metadata.File{
		ID:        uuid.New(),
		ShareName: shareName,
		Path:      "/dup.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		},
	}
	if perr := store.UpdateAttrs(ctx, dupFile); perr != nil {
		t.Fatalf("duplicate-path UpdateAttrs should now succeed (path_hash uniqueness dropped in #1165), got %v", perr)
	}
}
