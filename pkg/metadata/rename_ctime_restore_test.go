package metadata_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
	"github.com/stretchr/testify/require"
)

// The SMB rename ChangeTime preserve puts back the ChangeTime the renamed inode
// had before Move stamped its own, but only while that stamp is still what the
// store holds. Both values come from inside Move's transaction, which is what
// makes the restore safe to apply and possible to apply at all.

// newSQLiteRenameFixture builds a Service over sqlite. sqlite (like postgres)
// stores timestamps as Windows FILETIME ticks and truncates on the way in, so
// it is the backend where a value that never went through the store cannot be
// compared against one that did.
func newSQLiteRenameFixture(t *testing.T) (*metadata.Service, metadata.FileHandle, string) {
	t.Helper()
	return registerRenameStore(t, newSQLiteRenameStore(t))
}

// newSQLiteRenameStore builds the bare sqlite store, so a test can wrap it
// before registering it with the Service.
func newSQLiteRenameStore(t *testing.T) *sqlite.SQLiteMetadataStore {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.NewSQLiteMetadataStore(ctx,
		&sqlite.SQLiteMetadataStoreConfig{Path: filepath.Join(t.TempDir(), "m.db"), AutoMigrate: true},
		metadata.FilesystemCapabilities{
			MaxReadSize: 1048576, PreferredReadSize: 1048576,
			MaxWriteSize: 1048576, PreferredWriteSize: 1048576,
			MaxFileSize: 1 << 62, MaxFilenameLen: 255,
			MaxPathLen: 4096, MaxHardLinkCount: 32767,
			SupportsHardLinks: true, SupportsSymlinks: true,
			CaseSensitive: true, CasePreserving: true, TimestampResolution: 1,
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// registerRenameStore creates the share root and wires the store into a Service.
func registerRenameStore(t *testing.T, store metadata.Store) (*metadata.Service, metadata.FileHandle, string) {
	t.Helper()
	ctx := context.Background()
	const share = "/rn"
	root, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	return svc, rootHandle, share
}

// TestRenameCtimeRestore_SurvivesTruncatingBackend asserts the restore actually
// fires on a backend that truncates timestamps.
//
// The value the restore compares against must be one the store returned, not an
// in-memory time.Time: sqlite and postgres keep timestamps as FILETIME ticks
// (100ns), so a wall clock reading full nanoseconds does not survive the round
// trip. Comparing against the unstored value makes the restore a silent no-op —
// green everywhere, and wrong on every SQL backend.
func TestRenameCtimeRestore_SurvivesTruncatingBackend(t *testing.T) {
	svc, rootHandle, share := newSQLiteRenameFixture(t)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "a.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	// A ChangeTime whose nanosecond part is NOT a multiple of 100, so it cannot
	// survive a FILETIME round trip intact. A clock that reads full nanoseconds
	// produces these routinely; one that reads only microseconds never does,
	// which is exactly how this defect hides.
	unaligned := time.Date(2030, 4, 5, 6, 7, 8, 123456789, time.UTC)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Ctime: &unaligned})
	require.NoError(t, err)

	before, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	stored := before.Ctime
	require.False(t, stored.Equal(unaligned),
		"precondition: sqlite must truncate this timestamp, or the test proves nothing")

	_, wcc, err := svc.Move(root, rootHandle, "a.bin", rootHandle, "b.bin")
	require.NoError(t, err)
	require.NotNil(t, wcc)

	require.NoError(t, svc.RestoreChangeTimeIfUnchanged(
		root.Context, handle, wcc.SourceCtime, wcc.SourcePreCtime))

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.True(t, after.Ctime.Equal(stored),
		"ChangeTime = %v; the pre-rename %v was not restored, so the conditional never fired",
		after.Ctime.UTC(), stored.UTC())
}

// TestRenameCtimeRestore_ComparesValuesTheStoreHolds pins the property that
// makes the two tests above meaningful, without depending on the host clock.
//
// The conditional compares an exact instant. A backend that truncates on write
// therefore only ever matches a value that has been through it, so the caller
// must supply one the store returned. This is asserted directly here because
// the natural way to catch it — a wall-clock timestamp that does not survive
// the round trip — is invisible wherever the clock is coarser than the storage
// granularity: a microsecond clock yields nanoseconds that are always a
// multiple of 100 and so always round-trip intact, and only a clock reading
// full nanoseconds shows the restore silently ceasing to fire.
func TestRenameCtimeRestore_ComparesValuesTheStoreHolds(t *testing.T) {
	svc, rootHandle, share := newSQLiteRenameFixture(t)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "e.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	unstored := time.Date(2030, 4, 5, 6, 7, 8, 123456789, time.UTC)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Ctime: &unstored})
	require.NoError(t, err)

	held, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.False(t, held.Ctime.Equal(unstored),
		"precondition: the backend must truncate this timestamp")

	want := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

	// Keyed on the value that was never stored: must not fire.
	require.NoError(t, svc.RestoreChangeTimeIfUnchanged(root.Context, handle, unstored, want))
	got, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.True(t, got.Ctime.Equal(held.Ctime),
		"restore fired on a value the store never held: ChangeTime moved to %v", got.Ctime.UTC())

	// Keyed on the value the store holds: must fire.
	require.NoError(t, svc.RestoreChangeTimeIfUnchanged(root.Context, handle, held.Ctime, want))
	got, err = svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.True(t, got.Ctime.Equal(want),
		"restore did not fire on the stored value: ChangeTime = %v, want %v", got.Ctime.UTC(), want)
}

// TestRenameCtimeRestore_MovePopulatesBothWccTimestamps pins the two timestamps
// Move reports, independently of any restore. They are the only inputs the
// conditional has, so a rename that leaves them zero, swaps them, or reports a
// stamp the store did not take makes the restore either a no-op or a write of
// the wrong value, and no test of the restore itself would say which.
//
// What this deliberately does NOT cover: whether the pre-rename value was read
// inside the rename's transaction or outside it. Absent a concurrent writer the
// two reads return the same bytes — nothing mutates the inode between them and
// the namespace relink does not touch ChangeTime — so the distinction is not
// observable without landing a write inside that window. It matters only under
// concurrency, and it is what keeps an advance committed just before the rename
// from being erased.
func TestRenameCtimeRestore_MovePopulatesBothWccTimestamps(t *testing.T) {
	svc, rootHandle, share := newSQLiteRenameFixture(t)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "f.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	// Pin the starting ChangeTime so the assertions do not depend on clock
	// resolution, and so the "before" value is one the store has truncated. It
	// must be in the past, or the rename's own stamp would not be later than it.
	pinned := time.Date(2001, 4, 5, 6, 7, 8, 123456789, time.UTC)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Ctime: &pinned})
	require.NoError(t, err)
	before, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)

	_, wcc, err := svc.Move(root, rootHandle, "f.bin", rootHandle, "g.bin")
	require.NoError(t, err)
	require.NotNil(t, wcc)

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)

	require.False(t, wcc.SourcePreCtime.IsZero(), "SourcePreCtime must be populated")
	require.False(t, wcc.SourceCtime.IsZero(), "SourceCtime must be populated")
	require.True(t, wcc.SourcePreCtime.Equal(before.Ctime),
		"SourcePreCtime = %v; want the ChangeTime held before the rename, %v",
		wcc.SourcePreCtime.UTC(), before.Ctime.UTC())
	require.True(t, wcc.SourceCtime.Equal(after.Ctime),
		"SourceCtime = %v; want the ChangeTime the store holds after the rename, %v — "+
			"a value that did not come back through the store cannot compare equal on a "+
			"backend that truncates",
		wcc.SourceCtime.UTC(), after.Ctime.UTC())
	require.True(t, wcc.SourceCtime.After(wcc.SourcePreCtime),
		"the rename must advance ChangeTime: pre=%v post=%v",
		wcc.SourcePreCtime.UTC(), wcc.SourceCtime.UTC())
}

// rootAuth is the identity every test here runs as: the restore is not
// permission-gated, so nothing in these tests turns on who the caller is.
func rootAuth() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
	}
}
