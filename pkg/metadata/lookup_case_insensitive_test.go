package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLookupCaseInsensitive_ExactMatch verifies the fast path: when the
// caller's spelling matches the on-disk name byte-for-byte, the result is
// returned without any directory scan.
func TestLookupCaseInsensitive_ExactMatch(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "ExactCase.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "ExactCase.txt")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, "ExactCase.txt", matched)
}

// TestLookupCaseInsensitive_FallbackMatch verifies the scan fallback: an
// upper-case probe finds a mixed-case on-disk entry and returns the original
// on-disk name (case preserved) so callers can pass it to RemoveFile / Move /
// etc. without breaking the canonical spelling.
func TestLookupCaseInsensitive_FallbackMatch(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "TestFile.TXT", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "TESTFILE.TXT")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, "TestFile.TXT", matched, "must return original on-disk casing, not the probe")
}

// TestLookupCaseInsensitive_NotFound returns nil/""/nil when no entry matches
// — this is the contract callers rely on to distinguish "not present" from
// real errors like NotDirectory or permission denied.
func TestLookupCaseInsensitive_NotFound(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "Missing.txt")
	require.NoError(t, err)
	assert.Nil(t, file)
	assert.Empty(t, matched)
}

// TestLookupCaseInsensitive_DirectoryEntry confirms the fallback handles
// directories as well as files (walkPath relies on this for path components).
func TestLookupCaseInsensitive_DirectoryEntry(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "MyDir", &metadata.FileAttr{Mode: 0755})
	require.NoError(t, err)

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "MYDIR")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, "MyDir", matched)
	assert.Equal(t, metadata.FileTypeDirectory, file.Type)
}

// TestLookupCaseInsensitive_NotADirectory surfaces NotDirectory errors from
// the underlying Lookup rather than silently swallowing them — callers must
// see the real SMB status code (STATUS_NOT_A_DIRECTORY).
func TestLookupCaseInsensitive_NotADirectory(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	fileEntry, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "regular.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	fileHandle, err := metadata.EncodeFileHandle(fileEntry)
	require.NoError(t, err)

	// Use the file as if it were a parent directory — must propagate
	// NotDirectory, not return a silent miss.
	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fileHandle, "child")
	require.Error(t, err)
	assert.Nil(t, file)
	assert.Empty(t, matched)
	var storeErr *metadata.StoreError
	if assert.ErrorAs(t, err, &storeErr) {
		assert.Equal(t, metadata.ErrNotDirectory, storeErr.Code)
	}
}

// TestLookupCaseInsensitive_SpecialNames short-circuits "." and ".." through
// the exact-case Lookup — they should never trigger the scan fallback.
func TestLookupCaseInsensitive_SpecialNames(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	// "." resolves to the directory itself.
	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, ".")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, ".", matched)
	assert.Equal(t, metadata.FileTypeDirectory, file.Type)

	// ".." from root resolves to root (no parent ⇒ Lookup returns self).
	parent, matchedDotDot, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "..")
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, "..", matchedDotDot)
	assert.Equal(t, metadata.FileTypeDirectory, parent.Type)
}

// TestLookupCaseInsensitive_PreservesCaseAcrossMultipleEntries makes sure the
// scan picks the actual on-disk match when the directory contains sibling
// entries that differ only by case-equivalent characters at the same offset.
func TestLookupCaseInsensitive_PreservesCaseAcrossMultipleEntries(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	for _, name := range []string{"alpha.txt", "Beta.TXT", "gamma.txt"} {
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, name, &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)
	}

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "BETA.txt")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, "Beta.TXT", matched)
}
