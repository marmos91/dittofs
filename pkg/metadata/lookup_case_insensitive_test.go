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

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "ExactCase.txt", &metadata.FileAttr{Mode: 0644})
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

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "TestFile.TXT", &metadata.FileAttr{Mode: 0644})
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

	_, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "MyDir", &metadata.FileAttr{Mode: 0755})
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

	fileEntry, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "regular.txt", &metadata.FileAttr{Mode: 0644})
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

// loneSurrogate returns the WTF-8 (3-byte) encoding of a surrogate code unit,
// matching how the SMB filename codec preserves unpaired UTF-16 surrogates.
func loneSurrogate(u uint16) string {
	cp := uint32(u)
	return string([]byte{
		byte(0xE0 | (cp >> 12)),
		byte(0x80 | ((cp >> 6) & 0x3F)),
		byte(0x80 | (cp & 0x3F)),
	})
}

// TestLookupCaseInsensitive_LoneSurrogatesDoNotFold guards the smb2.charset
// .Testing invariant at the metadata layer: the unpaired surrogates {U+D800}
// and {U+DC00} are distinct SMB filenames and must not alias. The case-
// insensitive scan fallback uses strings.EqualFold, which decodes both
// (invalid-UTF-8) WTF-8 sequences to U+FFFD and would report them equal —
// causing the second CREATE to collide with the first. The fold guard must
// fall back to byte-exact comparison for malformed names so they stay distinct.
func TestLookupCaseInsensitive_LoneSurrogatesDoNotFold(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	hi := loneSurrogate(0xD800)
	lo := loneSurrogate(0xDC00)
	require.NotEqual(t, hi, lo)

	// Create only the high surrogate. An exact Lookup of {U+D800} succeeds,
	// but a Lookup of {U+DC00} misses and drops into the EqualFold scan.
	hiFile, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, hi, &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)

	// The fallback scan must NOT fold {U+DC00} onto the stored {U+D800}.
	loFound, _, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, lo)
	require.NoError(t, err)
	assert.Nil(t, loFound, "lone low surrogate must not fold onto the stored lone high surrogate")

	// Now create the low surrogate too; both must resolve to distinct files.
	loFile, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, lo, &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err, "second CREATE must not collide with the first")

	hiResolved, _, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, hi)
	require.NoError(t, err)
	require.NotNil(t, hiResolved)
	loResolved, _, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, lo)
	require.NoError(t, err)
	require.NotNil(t, loResolved)

	assert.Equal(t, hiFile.ID, hiResolved.ID)
	assert.Equal(t, loFile.ID, loResolved.ID)
	assert.NotEqual(t, hiResolved.ID, loResolved.ID, "lone surrogates must resolve to distinct files")
}

// TestLookupCaseInsensitive_PreservesCaseAcrossMultipleEntries makes sure the
// scan picks the actual on-disk match when the directory contains sibling
// entries that differ only by case-equivalent characters at the same offset.
func TestLookupCaseInsensitive_PreservesCaseAcrossMultipleEntries(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	for _, name := range []string{"alpha.txt", "Beta.TXT", "gamma.txt"} {
		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, name, &metadata.FileAttr{Mode: 0644})
		require.NoError(t, err)
	}

	file, matched, err := fx.service.LookupCaseInsensitive(fx.rootContext(), fx.rootHandle, "BETA.txt")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, "Beta.TXT", matched)
}
