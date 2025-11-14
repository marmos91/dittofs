package testing

import (
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/content"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunSeekableTests executes all SeekableContentStore operation tests.
func (suite *StoreTestSuite) RunSeekableTests(t *testing.T) {
	t.Run("ReadContentSeekable_Basic", suite.testReadContentSeekableBasic)
	t.Run("ReadContentSeekable_SeekStart", suite.testReadContentSeekableSeekStart)
	t.Run("ReadContentSeekable_SeekEnd", suite.testReadContentSeekableSeekEnd)
	t.Run("ReadContentSeekable_SeekCurrent", suite.testReadContentSeekableSeekCurrent)
	t.Run("ReadContentSeekable_NotFound", suite.testReadContentSeekableNotFound)
}

// ============================================================================
// ReadContentSeekable Tests
// ============================================================================

func (suite *StoreTestSuite) testReadContentSeekableBasic(t *testing.T) {
	store := suite.NewStore()
	seekable, ok := store.(content.SeekableContentStore)
	if !ok {
		t.Skip("Store does not implement SeekableContentStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("seekable-basic")
	testData := []byte("0123456789")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Read with seeking
	reader, err := seekable.ReadContentSeekable(testContext(), id)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Read all
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, data)
}

func (suite *StoreTestSuite) testReadContentSeekableSeekStart(t *testing.T) {
	store := suite.NewStore()
	seekable, ok := store.(content.SeekableContentStore)
	if !ok {
		t.Skip("Store does not implement SeekableContentStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("seekable-seek-start")
	testData := []byte("0123456789ABCDEF")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Read with seeking
	reader, err := seekable.ReadContentSeekable(testContext(), id)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Seek to position 5 from start
	pos, err := reader.Seek(5, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(5), pos)

	// Read 4 bytes
	buf := make([]byte, 4)
	n, err := io.ReadFull(reader, buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte("5678"), buf)
}

func (suite *StoreTestSuite) testReadContentSeekableSeekEnd(t *testing.T) {
	store := suite.NewStore()
	seekable, ok := store.(content.SeekableContentStore)
	if !ok {
		t.Skip("Store does not implement SeekableContentStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("seekable-seek-end")
	testData := []byte("0123456789")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Read with seeking
	reader, err := seekable.ReadContentSeekable(testContext(), id)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Seek to 4 bytes before end
	pos, err := reader.Seek(-4, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(6), pos)

	// Read remaining bytes
	buf, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, []byte("6789"), buf)
}

func (suite *StoreTestSuite) testReadContentSeekableSeekCurrent(t *testing.T) {
	store := suite.NewStore()
	seekable, ok := store.(content.SeekableContentStore)
	if !ok {
		t.Skip("Store does not implement SeekableContentStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("seekable-seek-current")
	testData := []byte("0123456789ABCDEFGHIJ")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Read with seeking
	reader, err := seekable.ReadContentSeekable(testContext(), id)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Read first 5 bytes
	buf := make([]byte, 5)
	_, err = io.ReadFull(reader, buf)
	require.NoError(t, err)
	assert.Equal(t, []byte("01234"), buf)

	// Seek forward 3 bytes from current position
	pos, err := reader.Seek(3, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(8), pos)

	// Read 4 bytes
	buf = make([]byte, 4)
	_, err = io.ReadFull(reader, buf)
	require.NoError(t, err)
	assert.Equal(t, []byte("89AB"), buf)

	// Seek backward 6 bytes from current position
	pos, err = reader.Seek(-6, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(6), pos)

	// Read 2 bytes
	buf = make([]byte, 2)
	_, err = io.ReadFull(reader, buf)
	require.NoError(t, err)
	assert.Equal(t, []byte("67"), buf)
}

func (suite *StoreTestSuite) testReadContentSeekableNotFound(t *testing.T) {
	store := suite.NewStore()
	seekable, ok := store.(content.SeekableContentStore)
	if !ok {
		t.Skip("Store does not implement SeekableContentStore")
	}

	id := generateTestID("seekable-notfound")

	// Read non-existent content should error
	_, err := seekable.ReadContentSeekable(testContext(), id)
	AssertErrorIs(t, content.ErrContentNotFound, err)
}
