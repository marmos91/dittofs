//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenManifestShared_DoesNotBlockDelete is the deterministic Windows
// regression for #1332. The GC mark phase streams manifest.hashes while the
// snapshot lifecycle unlinks it. With bare os.Open the read handle omits
// FILE_SHARE_DELETE, so a concurrent unlink raises ERROR_SHARING_VIOLATION;
// openManifestShared grants the full sharing set so the unlink proceeds against
// the open handle (POSIX-like: the reader keeps reading the detached file).
func TestOpenManifestShared_DoesNotBlockDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.hashes")
	require.NoError(t, os.WriteFile(path, []byte("hash-a\n"), 0o600))

	// A held read handle (the mark side streaming the manifest) must NOT block
	// a concurrent snapshot delete removing the file.
	f, err := openManifestShared(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	require.NoError(t, os.Remove(path),
		"shared read handle must not block manifest delete (#1332)")

	// Baseline contrast: a bare os.Open (the pre-fix path) DOES block the same
	// delete, proving the explicit share mode is what fixes #1332.
	require.NoError(t, os.WriteFile(path, []byte("hash-b\n"), 0o600))
	bare, err := os.Open(path)
	require.NoError(t, err)
	require.Error(t, os.Remove(path),
		"baseline: bare os.Open omits FILE_SHARE_DELETE and blocks delete on Windows")
	require.NoError(t, bare.Close())
	require.NoError(t, os.Remove(path)) // leave dir clean for t.TempDir cleanup
}

// TestOpenManifestShared_MissingPathIsErrNotExist guards that the Windows
// CreateFile path preserves fs.ErrNotExist semantics so streamManifest's TOCTOU
// short-circuit (manifest deleted between Stat and open) still triggers.
func TestOpenManifestShared_MissingPathIsErrNotExist(t *testing.T) {
	_, err := openManifestShared(filepath.Join(t.TempDir(), "does-not-exist.hashes"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
