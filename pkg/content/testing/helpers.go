package testing

import (
	"errors"
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AssertErrorIs checks if the error matches the expected error using errors.Is.
func AssertErrorIs(t *testing.T, expected error, actual error) {
	t.Helper()
	if !errors.Is(actual, expected) {
		t.Errorf("Expected error %v, got %v", expected, actual)
	}
}

// mustWriteContent writes content and fails the test if it errors.
func mustWriteContent(t *testing.T, store content.WritableContentStore, id metadata.ContentID, data []byte) {
	t.Helper()
	err := store.WriteContent(testContext(), id, data)
	require.NoError(t, err, "WriteContent should succeed")
}

// mustWriteAt writes data at offset and fails the test if it errors.
func mustWriteAt(t *testing.T, store content.WritableContentStore, id metadata.ContentID, data []byte, offset int64) {
	t.Helper()
	err := store.WriteAt(testContext(), id, data, offset)
	require.NoError(t, err, "WriteAt should succeed")
}

// mustReadContent reads content and fails the test if it errors.
func mustReadContent(t *testing.T, store content.ContentStore, id metadata.ContentID) []byte {
	t.Helper()
	reader, err := store.ReadContent(testContext(), id)
	require.NoError(t, err, "ReadContent should succeed")
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	require.NoError(t, err, "Reading content should succeed")
	return data
}

// mustGetSize gets content size and fails the test if it errors.
func mustGetSize(t *testing.T, store content.ContentStore, id metadata.ContentID) uint64 {
	t.Helper()
	size, err := store.GetContentSize(testContext(), id)
	require.NoError(t, err, "GetContentSize should succeed")
	return size
}

// mustDelete deletes content and fails the test if it errors.
func mustDelete(t *testing.T, store content.WritableContentStore, id metadata.ContentID) {
	t.Helper()
	err := store.Delete(testContext(), id)
	require.NoError(t, err, "Delete should succeed")
}

// mustTruncate truncates content and fails the test if it errors.
func mustTruncate(t *testing.T, store content.WritableContentStore, id metadata.ContentID, size uint64) {
	t.Helper()
	err := store.Truncate(testContext(), id, size)
	require.NoError(t, err, "Truncate should succeed")
}

// assertContentExists checks if content exists.
func assertContentExists(t *testing.T, store content.ContentStore, id metadata.ContentID, expected bool) {
	t.Helper()
	exists, err := store.ContentExists(testContext(), id)
	require.NoError(t, err, "ContentExists should not error")
	assert.Equal(t, expected, exists, "Content existence mismatch")
}

// assertContentEquals checks if content matches expected data.
func assertContentEquals(t *testing.T, store content.ContentStore, id metadata.ContentID, expected []byte) {
	t.Helper()
	actual := mustReadContent(t, store, id)
	assert.Equal(t, expected, actual, "Content data mismatch")
}

// assertContentSize checks if content size matches expected.
func assertContentSize(t *testing.T, store content.ContentStore, id metadata.ContentID, expected uint64) {
	t.Helper()
	actual := mustGetSize(t, store, id)
	assert.Equal(t, expected, actual, "Content size mismatch")
}

// generateTestData creates test data of specified size.
func generateTestData(size int) []byte {
	data := make([]byte, size)
	for i := 0; i < size; i++ {
		data[i] = byte(i % 256)
	}
	return data
}

// generateTestID generates a unique test content ID.
func generateTestID(name string) metadata.ContentID {
	return metadata.ContentID("test-" + name)
}
