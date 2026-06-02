package metadata_test

import (
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests assert the H9 contract: every mutating MetadataService method
// returns the affected directory's pre- and post-operation attributes, captured
// atomically with the mutation (inside the same store transaction). Protocol
// handlers consume these for WCC data instead of doing a separate GetFile that
// could race a concurrent mutation in the window.

func childHandle(t *testing.T, fx *testFixture, parent metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()
	h, err := fx.service.GetChild(fx.rootContext().Context, parent, name)
	require.NoError(t, err)
	return h
}

// assertBracketsMutation verifies a DirWcc looks like a real before/after pair:
// both halves present and the post-op mtime is not before the pre-op mtime.
func assertBracketsMutation(t *testing.T, wcc *metadata.DirWcc) {
	t.Helper()
	require.NotNil(t, wcc, "mutation must return DirWcc")
	require.NotNil(t, wcc.Before, "DirWcc.Before must be captured")
	require.NotNil(t, wcc.After, "DirWcc.After must be captured")
	assert.False(t, wcc.After.Mtime.Before(wcc.Before.Mtime),
		"post-op mtime must not precede pre-op mtime")
}

func TestWCC_CreateFile_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	pre, err := fx.store.GetFile(fx.rootContext().Context, fx.rootHandle)
	require.NoError(t, err)

	_, wcc, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "f.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
	// Before must equal the directory state observed immediately prior.
	assert.Equal(t, pre.Mtime.UnixNano(), wcc.Before.Mtime.UnixNano(),
		"DirWcc.Before must equal the pre-op directory mtime")
}

func TestWCC_CreateDirectory_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	_, wcc, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "d", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
}

func TestWCC_CreateSymlink_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	_, wcc, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "l", "/target", &metadata.FileAttr{
		Type: metadata.FileTypeSymlink, Mode: 0o777,
	})
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
}

func TestWCC_RemoveFile_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "f.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)

	_, wcc, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "f.txt")
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
}

func TestWCC_RemoveDirectory_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "d", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)

	wcc, err := fx.service.RemoveDirectory(fx.rootContext(), fx.rootHandle, "d")
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
}

func TestWCC_CreateHardLink_ReturnsDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "orig.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)
	target := childHandle(t, fx, fx.rootHandle, "orig.txt")

	wcc, err := fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link.txt", target)
	require.NoError(t, err)
	assertBracketsMutation(t, wcc)
}

func TestWCC_SetFileAttributes_ReturnsFileAttrs(t *testing.T) {
	fx := newTestFixture(t)
	f, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "f.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)
	fh, err := metadata.EncodeFileHandle(f)
	require.NoError(t, err)

	newMode := uint32(0o600)
	wcc, err := fx.service.SetFileAttributes(fx.rootContext(), fh, &metadata.SetAttrs{Mode: &newMode})
	require.NoError(t, err)
	require.NotNil(t, wcc)
	require.NotNil(t, wcc.Before)
	require.NotNil(t, wcc.After)
	// For SETATTR the WCC subject is the file itself.
	assert.EqualValues(t, 0o644, wcc.Before.Mode, "Before must reflect the pre-op file mode")
	assert.EqualValues(t, 0o600, wcc.After.Mode, "After must reflect the post-op file mode")
}

func TestWCC_Move_ReturnsBothDirAttrs(t *testing.T) {
	fx := newTestFixture(t)
	// Source and destination directories.
	_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "src", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	_, _, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dst", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	srcDir := childHandle(t, fx, fx.rootHandle, "src")
	dstDir := childHandle(t, fx, fx.rootHandle, "dst")
	_, _, err = fx.service.CreateFile(fx.rootContext(), srcDir, "f.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)

	rw, err := fx.service.Move(fx.rootContext(), srcDir, "f.txt", dstDir, "f.txt")
	require.NoError(t, err)
	require.NotNil(t, rw)
	assertBracketsMutation(t, rw.FromDir)
	assertBracketsMutation(t, rw.ToDir)
	assert.NotSame(t, rw.FromDir, rw.ToDir, "cross-directory move must report distinct DirWcc")
}

func TestWCC_Move_SameDirectorySharesDirWcc(t *testing.T) {
	fx := newTestFixture(t)
	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "a.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	require.NoError(t, err)

	rw, err := fx.service.Move(fx.rootContext(), fx.rootHandle, "a.txt", fx.rootHandle, "b.txt")
	require.NoError(t, err)
	require.NotNil(t, rw)
	assert.Same(t, rw.FromDir, rw.ToDir, "intra-directory move must share one DirWcc")
	assertBracketsMutation(t, rw.FromDir)
}

// TestWCC_RemoveFile_PreOpAttrsAreAtomic is the core H9 regression guard. It
// runs many concurrent RemoveFile + CreateFile cycles and asserts the pre-op
// directory mtime returned by each RemoveFile never exceeds its post-op mtime —
// i.e. the Before snapshot genuinely precedes the mutation rather than being a
// separately-read value that a concurrent mutation could have advanced past.
func TestWCC_RemoveFile_PreOpAttrsAreAtomic(t *testing.T) {
	fx := newTestFixture(t)
	const workers = 8
	const iters = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := "concurrent.txt"
			for i := 0; i < iters; i++ {
				// Create then remove; ignore ErrAlreadyExists / ErrNoEntity from
				// the inherent races between workers — we only assert the WCC
				// invariant on successful removals.
				_, _, _ = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, name, &metadata.FileAttr{
					Type: metadata.FileTypeRegular, Mode: 0o644,
				})
				_, wcc, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, name)
				if err != nil {
					continue
				}
				require.NotNil(t, wcc)
				if wcc.Before != nil && wcc.After != nil {
					assert.False(t, wcc.After.Mtime.Before(wcc.Before.Mtime),
						"pre-op mtime must not be after post-op mtime (TOCTOU)")
				}
			}
		}(w)
	}
	wg.Wait()
}
