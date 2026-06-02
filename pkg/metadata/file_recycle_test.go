package metadata_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTrashPolicy is a fixed-config TrashPolicy for unit tests.
type stubTrashPolicy struct {
	cfg metadata.TrashConfig
}

func (p stubTrashPolicy) TrashConfigForShare(string) (metadata.TrashConfig, bool) {
	return p.cfg, true
}

// newRecycleFixture builds a fully-registered share (shares map + root) so the
// service's GetRootHandle resolves — recycleNode depends on it. newTestFixture
// only calls CreateRootDirectory and leaves the shares map empty.
func newRecycleFixture(t *testing.T) *testFixture {
	t.Helper()
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	return &testFixture{
		t:          t,
		service:    svc,
		store:      store,
		shareName:  shareName,
		rootHandle: rootHandle,
	}
}

func TestRemoveFile_RecyclesWhenTrashEnabled(t *testing.T) {
	t.Parallel()

	t.Run("file moves into #recycle with PayloadID cleared", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "report.pdf", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)

		removed, _, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "report.pdf")
		require.NoError(t, err)
		require.NotNil(t, removed)
		// Cleared PayloadID makes adapters skip block deletion (deferred reaping).
		assert.Equal(t, metadata.PayloadID(""), removed.PayloadID)

		// Original location is gone.
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "report.pdf")
		assert.True(t, metadata.IsNotFoundError(err))

		// File now lives under #recycle, stamped as deleted.
		binHandle, err := fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		require.NoError(t, err)
		moved, err := fx.service.Lookup(fx.rootContext(), binHandle, "report.pdf")
		require.NoError(t, err)
		require.NotNil(t, moved.DeletedAt)
		assert.Equal(t, "report.pdf", moved.OriginalPath)
	})

	t.Run("delete inside #recycle is permanent", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		// Recycle once so #recycle exists with an entry.
		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "a.txt", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)
		_, _, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "a.txt")
		require.NoError(t, err)

		binHandle, err := fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		require.NoError(t, err)

		// Deleting the file already in the bin must be permanent: it does not
		// get re-recycled into a nested #recycle, it is removed.
		_, _, err = fx.service.RemoveFile(fx.rootContext(), binHandle, "a.txt")
		require.NoError(t, err)
		_, err = fx.service.Lookup(fx.rootContext(), binHandle, "a.txt")
		assert.True(t, metadata.IsNotFoundError(err))
		// No nested #recycle created inside the bin.
		_, err = fx.service.GetChild(fx.rootContext().Context, binHandle, metadata.RecycleDirName)
		assert.Error(t, err)
	})

	t.Run("excluded name bypasses the bin", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{
			Enabled:         true,
			ExcludePatterns: []string{"*.tmp"},
		}})

		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "scratch.tmp", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)
		_, _, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "scratch.tmp")
		require.NoError(t, err)

		// No bin created, file permanently gone.
		_, err = fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		assert.Error(t, err)
	})

	t.Run("recycling the same name twice keeps both copies", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		// Create + delete "dup.txt" twice with no sleep between, so both deletes
		// land in the same wall-clock second. The collision must NOT overwrite
		// the first recycled copy.
		for i := 0; i < 2; i++ {
			_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "dup.txt", &metadata.FileAttr{Mode: 0644})
			require.NoError(t, err)
			_, _, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "dup.txt")
			require.NoError(t, err)
		}

		binHandle, err := fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		require.NoError(t, err)

		// Both copies must exist in the bin as two distinct entries.
		page, err := fx.service.ReadDirectory(fx.rootContext(), binHandle, 0, 0)
		require.NoError(t, err)
		var dupCount int
		for _, e := range page.Entries {
			// The first copy keeps the plain name; the collided copy carries a
			// suffix. Both retain OriginalPath = "dup.txt" and a DeletedAt stamp.
			if e.Name == "dup.txt" || strings.HasPrefix(e.Name, "dup.txt (") {
				dupCount++
				child, err := fx.service.GetChild(fx.rootContext().Context, binHandle, e.Name)
				require.NoError(t, err)
				f, err := fx.service.GetFile(fx.rootContext().Context, child)
				require.NoError(t, err)
				require.NotNil(t, f.DeletedAt, "recycled entry %q missing DeletedAt", e.Name)
				assert.Equal(t, "dup.txt", f.OriginalPath)
			}
		}
		assert.Equal(t, 2, dupCount, "both recycled copies should survive the collision")
	})

	t.Run("replace-overwrite rename recycles the victim", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		// Distinct modes stand in for distinct content: Move preserves FileAttr,
		// so the surviving file's Mode tells us whose identity won.
		const modeA = uint32(0640)
		const modeB = uint32(0600)
		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "a", &metadata.FileAttr{Mode: modeA})
		require.NoError(t, err)
		_, _, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "b", &metadata.FileAttr{Mode: modeB})
		require.NoError(t, err)

		// Rename "b" onto "a" — an overwrite. The old "a" must be recycled.
		_, mvErr := fx.service.Move(fx.rootContext(), fx.rootHandle, "b", fx.rootHandle, "a")
		require.NoError(t, mvErr)

		// Source "b" is gone.
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "b")
		assert.True(t, metadata.IsNotFoundError(err))

		// Live "a" now carries b's content (size) and is NOT stamped deleted.
		liveA, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, "a")
		require.NoError(t, err)
		assert.Equal(t, modeB, liveA.Mode&0777, "destination should hold b's content")
		assert.Nil(t, liveA.DeletedAt, "live destination must not be stamped deleted")

		// The old "a" lives under #recycle, stamped, with its original size.
		binHandle, err := fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		require.NoError(t, err)
		recycledA, err := fx.service.Lookup(fx.rootContext(), binHandle, "a")
		require.NoError(t, err)
		require.NotNil(t, recycledA.DeletedAt)
		assert.Equal(t, "a", recycledA.OriginalPath)
		assert.Equal(t, modeA, recycledA.Mode&0777, "recycled victim should keep a's content")
	})

	t.Run("non-clobbering rename creates no recycle entry", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "b", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)

		// "c" does not exist: a plain rename must not touch the bin.
		_, mvErr := fx.service.Move(fx.rootContext(), fx.rootHandle, "b", fx.rootHandle, "c")
		require.NoError(t, mvErr)

		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "c")
		require.NoError(t, err)
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "b")
		assert.True(t, metadata.IsNotFoundError(err))

		// No #recycle bin was ever created.
		_, err = fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		assert.Error(t, err)
	})

	t.Run("non-empty directory recycles as a single subtree", func(t *testing.T) {
		t.Parallel()
		fx := newRecycleFixture(t)
		fx.service.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

		dir, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "docs", &metadata.FileAttr{Mode: 0755})
		require.NoError(t, err)
		dirHandle, err := metadata.EncodeFileHandle(dir)
		require.NoError(t, err)
		_, _, err = fx.service.CreateFile(fx.rootContext(), dirHandle, "inner.txt", &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)

		// RemoveDirectory on a non-empty dir succeeds (recycled, not ErrNotEmpty).
		_, err = fx.service.RemoveDirectory(fx.rootContext(), fx.rootHandle, "docs")
		require.NoError(t, err)

		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "docs")
		assert.True(t, metadata.IsNotFoundError(err))

		binHandle, err := fx.service.GetChild(fx.rootContext().Context, fx.rootHandle, metadata.RecycleDirName)
		require.NoError(t, err)
		moved, err := fx.service.Lookup(fx.rootContext(), binHandle, "docs")
		require.NoError(t, err)
		require.NotNil(t, moved.DeletedAt)
		// The whole subtree moved: inner.txt is still under the moved dir.
		movedHandle, err := metadata.EncodeFileHandle(moved)
		require.NoError(t, err)
		_, err = fx.service.Lookup(fx.rootContext(), movedHandle, "inner.txt")
		require.NoError(t, err)
	})
}
