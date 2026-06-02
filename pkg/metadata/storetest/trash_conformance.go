package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// trashStubPolicy is a fixed-config TrashPolicy: it enables trash for the test
// share with the supplied exclude patterns. Mirrors stubTrashPolicy in the
// metadata package's unit tests, but lives here so the conformance suite runs
// the same recycle behavior against every backend.
type trashStubPolicy struct {
	cfg metadata.TrashConfig
}

func (p trashStubPolicy) TrashConfigForShare(string) (metadata.TrashConfig, bool) {
	return p.cfg, true
}

// trashFixture bundles a service bootstrapped over a backend store with trash
// enabled, plus the auth context and root handle the subtests operate against.
type trashFixture struct {
	svc        *metadata.MetadataService
	ctx        *metadata.AuthContext
	rootHandle metadata.FileHandle
	shareName  string
}

// newTrashService bootstraps a MetadataService over a fresh backend store with
// trash enabled (and the given exclude patterns). It follows the fixture
// ordering the recycle path requires: CreateShare BEFORE CreateRootDirectory,
// so GetRootHandle resolves when recycleNode looks up the share root.
func newTrashService(t *testing.T, factory StoreFactory, excludes []string) *trashFixture {
	t.Helper()

	store := factory(t)
	ctx := context.Background()
	shareName := "/test"

	// Share must exist before the root directory: the recycle path resolves the
	// share root via GetRootHandle, which fails if the share isn't registered.
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare(%q) failed: %v", shareName, err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q) failed: %v", shareName, err)
	}
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle() failed: %v", err)
	}

	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("RegisterStoreForShare(%q) failed: %v", shareName, err)
	}
	svc.SetTrashPolicy(trashStubPolicy{cfg: metadata.TrashConfig{
		Enabled:         true,
		ExcludePatterns: excludes,
	}})

	return &trashFixture{
		svc:        svc,
		ctx:        rootTrashContext(),
		rootHandle: rootHandle,
		shareName:  shareName,
	}
}

// rootTrashContext returns a root (uid/gid 0) AuthContext, the identity the
// conformance subtests act as.
func rootTrashContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

// runTrashConformanceTests exercises the recycle behavior (T2–T4) against every
// metadata backend the suite runs over (memory, badger, postgres-in-CI). It is
// the cross-backend parity gate for the trash feature: a backend that drops
// DeletedAt/OriginalPath, mishandles the in-bin permanent-delete escape hatch,
// or overwrites collisions will fail here even when the in-package unit tests
// (memory-only) pass.
func runTrashConformanceTests(t *testing.T, factory StoreFactory) {
	t.Run("UnlinkRecyclesIntoBin", func(t *testing.T) { testUnlinkRecyclesIntoBin(t, factory) })
	t.Run("DeleteInsideBinIsPermanent", func(t *testing.T) { testDeleteInsideBinIsPermanent(t, factory) })
	t.Run("ExcludePatternBypassesBin", func(t *testing.T) { testExcludePatternBypassesBin(t, factory) })
	t.Run("CollisionGetsUniqueName", func(t *testing.T) { testCollisionGetsUniqueName(t, factory) })
	t.Run("SubtreeRecycledAsOneEntry", func(t *testing.T) { testSubtreeRecycledAsOneEntry(t, factory) })
	t.Run("OverwriteRecyclesVictim", func(t *testing.T) { testOverwriteRecyclesVictim(t, factory) })
}

// --- local helpers -----------------------------------------------------------

// trashCreateFile creates a regular file with the given mode under parent.
func (fx *trashFixture) trashCreateFile(t *testing.T, parent metadata.FileHandle, name string, mode uint32) {
	t.Helper()
	if _, _, err := fx.svc.CreateFile(fx.ctx, parent, name, &metadata.FileAttr{Mode: mode}); err != nil {
		t.Fatalf("CreateFile(%q) failed: %v", name, err)
	}
}

// trashMkdir creates a directory under parent and returns its handle.
func (fx *trashFixture) trashMkdir(t *testing.T, parent metadata.FileHandle, name string, mode uint32) metadata.FileHandle {
	t.Helper()
	dir, _, err := fx.svc.CreateDirectory(fx.ctx, parent, name, &metadata.FileAttr{Mode: mode})
	if err != nil {
		t.Fatalf("CreateDirectory(%q) failed: %v", name, err)
	}
	h, err := metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle(%q) failed: %v", name, err)
	}
	return h
}

// trashBinHandle resolves the share's #recycle bin handle, failing the test if
// it does not exist.
func (fx *trashFixture) trashBinHandle(t *testing.T) metadata.FileHandle {
	t.Helper()
	bin, err := fx.svc.GetChild(fx.ctx.Context, fx.rootHandle, metadata.RecycleDirName)
	if err != nil {
		t.Fatalf("GetChild(%q) failed: %v", metadata.RecycleDirName, err)
	}
	return bin
}

// trashBinMissing asserts the #recycle bin was never created under root.
func (fx *trashFixture) trashBinMissing(t *testing.T) {
	t.Helper()
	if _, err := fx.svc.GetChild(fx.ctx.Context, fx.rootHandle, metadata.RecycleDirName); err == nil {
		t.Fatalf("expected no %q bin, but it exists", metadata.RecycleDirName)
	}
}

// trashLookupMissing asserts name does not resolve under dir.
func (fx *trashFixture) trashLookupMissing(t *testing.T, dir metadata.FileHandle, name string) {
	t.Helper()
	if _, err := fx.svc.Lookup(fx.ctx, dir, name); !metadata.IsNotFoundError(err) {
		t.Fatalf("Lookup(%q): expected not-found, got err=%v", name, err)
	}
}

// --- subtests -----------------------------------------------------------------

func testUnlinkRecyclesIntoBin(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, nil)
	fx.trashCreateFile(t, fx.rootHandle, "doc.txt", 0644)

	removed, _, err := fx.svc.RemoveFile(fx.ctx, fx.rootHandle, "doc.txt")
	if err != nil {
		t.Fatalf("RemoveFile(doc.txt) failed: %v", err)
	}
	if removed == nil {
		t.Fatal("RemoveFile returned nil *File")
	}
	// Recycle (not permanent delete) clears PayloadID so adapters skip block
	// deletion.
	if removed.PayloadID != metadata.PayloadID("") {
		t.Errorf("recycled PayloadID = %q, want empty", removed.PayloadID)
	}

	// Original location is gone.
	fx.trashLookupMissing(t, fx.rootHandle, "doc.txt")

	// File now lives under #recycle, stamped deleted, with its original path.
	bin := fx.trashBinHandle(t)
	moved, err := fx.svc.Lookup(fx.ctx, bin, "doc.txt")
	if err != nil {
		t.Fatalf("Lookup(#recycle/doc.txt) failed: %v", err)
	}
	if moved.DeletedAt == nil {
		t.Error("recycled file missing DeletedAt stamp")
	}
	if moved.OriginalPath != "doc.txt" {
		t.Errorf("OriginalPath = %q, want %q", moved.OriginalPath, "doc.txt")
	}
}

func testDeleteInsideBinIsPermanent(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, nil)
	fx.trashCreateFile(t, fx.rootHandle, "doc.txt", 0644)

	if _, _, err := fx.svc.RemoveFile(fx.ctx, fx.rootHandle, "doc.txt"); err != nil {
		t.Fatalf("RemoveFile(doc.txt) failed: %v", err)
	}
	bin := fx.trashBinHandle(t)

	// Deleting the entry already inside #recycle must be permanent: a real
	// PayloadID is returned (so blocks are reaped) and no nested bin is made.
	removed, _, err := fx.svc.RemoveFile(fx.ctx, bin, "doc.txt")
	if err != nil {
		t.Fatalf("RemoveFile(#recycle/doc.txt) failed: %v", err)
	}
	if removed == nil {
		t.Fatal("RemoveFile returned nil *File for in-bin delete")
	}
	if removed.PayloadID == metadata.PayloadID("") {
		t.Error("in-bin delete returned empty PayloadID, want permanent (non-empty)")
	}

	// Entry is gone from the bin.
	fx.trashLookupMissing(t, bin, "doc.txt")

	// No nested #recycle was created inside the bin.
	if _, err := fx.svc.GetChild(fx.ctx.Context, bin, metadata.RecycleDirName); err == nil {
		t.Error("nested #recycle should not be created inside the bin")
	}
}

func testExcludePatternBypassesBin(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, []string{"*.tmp"})
	fx.trashCreateFile(t, fx.rootHandle, "scratch.tmp", 0644)

	removed, _, err := fx.svc.RemoveFile(fx.ctx, fx.rootHandle, "scratch.tmp")
	if err != nil {
		t.Fatalf("RemoveFile(scratch.tmp) failed: %v", err)
	}
	if removed == nil {
		t.Fatal("RemoveFile returned nil *File")
	}
	// Excluded names bypass the bin and delete permanently (real PayloadID).
	if removed.PayloadID == metadata.PayloadID("") {
		t.Error("excluded-name delete returned empty PayloadID, want permanent (non-empty)")
	}

	// An excluded-only delete never touches the bin, so it must not exist.
	fx.trashBinMissing(t)
}

func testCollisionGetsUniqueName(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, nil)

	// Create + recycle "a.txt" twice with no sleep, so both deletes collide on
	// the same name. The second must NOT overwrite the first.
	for i := 0; i < 2; i++ {
		fx.trashCreateFile(t, fx.rootHandle, "a.txt", 0644)
		if _, _, err := fx.svc.RemoveFile(fx.ctx, fx.rootHandle, "a.txt"); err != nil {
			t.Fatalf("RemoveFile(a.txt) iteration %d failed: %v", i, err)
		}
	}

	bin := fx.trashBinHandle(t)
	page, err := fx.svc.ReadDirectory(fx.ctx, bin, 0, 0)
	if err != nil {
		t.Fatalf("ReadDirectory(#recycle) failed: %v", err)
	}

	var count int
	for _, e := range page.Entries {
		if e.Name != "a.txt" && !strings.HasPrefix(e.Name, "a.txt (") {
			continue
		}
		count++
		child, err := fx.svc.GetChild(fx.ctx.Context, bin, e.Name)
		if err != nil {
			t.Fatalf("GetChild(#recycle/%q) failed: %v", e.Name, err)
		}
		f, err := fx.svc.GetFile(fx.ctx.Context, child)
		if err != nil {
			t.Fatalf("GetFile(#recycle/%q) failed: %v", e.Name, err)
		}
		if f.DeletedAt == nil {
			t.Errorf("recycled entry %q missing DeletedAt", e.Name)
		}
		if f.OriginalPath != "a.txt" {
			t.Errorf("recycled entry %q OriginalPath = %q, want %q", e.Name, f.OriginalPath, "a.txt")
		}
	}
	if count != 2 {
		t.Errorf("found %d recycled copies of a.txt, want 2 (collision must not overwrite)", count)
	}
}

func testSubtreeRecycledAsOneEntry(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, nil)

	projectHandle := fx.trashMkdir(t, fx.rootHandle, "project", 0755)
	fx.trashCreateFile(t, projectHandle, "main.go", 0644)

	// RemoveDirectory on a non-empty dir recycles the whole subtree (no
	// ErrNotEmpty).
	if _, err := fx.svc.RemoveDirectory(fx.ctx, fx.rootHandle, "project"); err != nil {
		t.Fatalf("RemoveDirectory(project) failed: %v", err)
	}
	fx.trashLookupMissing(t, fx.rootHandle, "project")

	bin := fx.trashBinHandle(t)
	moved, err := fx.svc.Lookup(fx.ctx, bin, "project")
	if err != nil {
		t.Fatalf("Lookup(#recycle/project) failed: %v", err)
	}
	if moved.DeletedAt == nil {
		t.Error("recycled subtree root missing DeletedAt stamp")
	}

	// The child moved with the subtree as one entry.
	movedHandle, err := metadata.EncodeFileHandle(moved)
	if err != nil {
		t.Fatalf("EncodeFileHandle(moved project) failed: %v", err)
	}
	if _, err := fx.svc.Lookup(fx.ctx, movedHandle, "main.go"); err != nil {
		t.Fatalf("Lookup(#recycle/project/main.go) failed: %v", err)
	}
}

func testOverwriteRecyclesVictim(t *testing.T, factory StoreFactory) {
	fx := newTrashService(t, factory, nil)

	// Distinct modes stand in for distinct content (CreateFile may not persist
	// Size). The surviving file's mode tells us whose identity won.
	const modeA = uint32(0640)
	const modeB = uint32(0600)
	fx.trashCreateFile(t, fx.rootHandle, "a", modeA)
	fx.trashCreateFile(t, fx.rootHandle, "b", modeB)

	// Rename "b" onto "a" — an overwrite that recycles the old "a".
	if _, err := fx.svc.Move(fx.ctx, fx.rootHandle, "b", fx.rootHandle, "a"); err != nil {
		t.Fatalf("Move(b -> a) failed: %v", err)
	}

	// Source "b" is gone.
	fx.trashLookupMissing(t, fx.rootHandle, "b")

	// Live "a" carries b's identity and is NOT stamped deleted.
	liveA, err := fx.svc.Lookup(fx.ctx, fx.rootHandle, "a")
	if err != nil {
		t.Fatalf("Lookup(a) failed: %v", err)
	}
	if liveA.Mode&0777 != modeB {
		t.Errorf("live a Mode = %o, want %o (should hold b's identity)", liveA.Mode&0777, modeB)
	}
	if liveA.DeletedAt != nil {
		t.Error("live destination must not be stamped deleted")
	}

	// Old "a" lives under #recycle, stamped, with its original identity.
	bin := fx.trashBinHandle(t)
	recycledA, err := fx.svc.Lookup(fx.ctx, bin, "a")
	if err != nil {
		t.Fatalf("Lookup(#recycle/a) failed: %v", err)
	}
	if recycledA.DeletedAt == nil {
		t.Error("recycled victim missing DeletedAt stamp")
	}
	if recycledA.OriginalPath != "a" {
		t.Errorf("recycled victim OriginalPath = %q, want %q", recycledA.OriginalPath, "a")
	}
	if recycledA.Mode&0777 != modeA {
		t.Errorf("recycled victim Mode = %o, want %o (should keep a's identity)", recycledA.Mode&0777, modeA)
	}
}
