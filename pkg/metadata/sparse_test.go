package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeSparseFile creates a regular file under the fixture root, then stamps it
// with the given size and block list directly via the store so PunchHole /
// Allocate have a realistic sparse layout to operate on. Returns the handle.
func makeSparseFile(t *testing.T, fx *testFixture, name string, size uint64, blocks []block.BlockRef) metadata.FileHandle {
	t.Helper()
	file, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, name, &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(fx.shareName, file.ID)
	require.NoError(t, err)

	stored, err := fx.store.GetFile(fx.rootContext().Context, handle)
	require.NoError(t, err)
	stored.Size = size
	stored.Blocks = blocks
	stored.PayloadID = metadata.PayloadID("payload-" + name)
	require.NoError(t, fx.store.PutFile(fx.rootContext().Context, stored))
	return handle
}

func bref(offset uint64, size uint32) block.BlockRef {
	return block.BlockRef{Offset: offset, Size: size}
}

func TestService_PunchHole(t *testing.T) {
	t.Run("drops blocks inside the range and keeps size", func(t *testing.T) {
		fx := newTestFixture(t)
		// Dense file [0,300): blocks at 0,100,200 each 100 bytes.
		handle := makeSparseFile(t, fx, "punch.bin", 300, []block.BlockRef{bref(0, 100), bref(100, 100), bref(200, 100)})

		res, err := fx.service.PunchHole(fx.rootContext(), handle, 100, 100)
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, uint64(300), res.File.Size, "size must be unchanged by DEALLOCATE")
		assert.Len(t, res.PreOpBlocks, 3, "pre-op snapshot for reclaim")

		got, err := fx.store.GetFile(fx.rootContext().Context, handle)
		require.NoError(t, err)
		require.Len(t, got.Blocks, 2, "middle block dropped")
		assert.Equal(t, uint64(0), got.Blocks[0].Offset)
		assert.Equal(t, uint64(200), got.Blocks[1].Offset)

		// The punched range is now a hole the hole map reports.
		holeAt, found := block.NextHoleOffset(got.Blocks, got.Size, 0)
		require.True(t, found)
		assert.Equal(t, uint64(100), holeAt)
	})

	t.Run("zero length is a no-op success", func(t *testing.T) {
		fx := newTestFixture(t)
		handle := makeSparseFile(t, fx, "noop.bin", 100, []block.BlockRef{bref(0, 100)})
		res, err := fx.service.PunchHole(fx.rootContext(), handle, 10, 0)
		require.NoError(t, err)
		assert.Nil(t, res.PreOpBlocks)
		got, _ := fx.store.GetFile(fx.rootContext().Context, handle)
		assert.Len(t, got.Blocks, 1)
	})

	t.Run("punch at EOF is a no-op success", func(t *testing.T) {
		fx := newTestFixture(t)
		handle := makeSparseFile(t, fx, "eof.bin", 100, []block.BlockRef{bref(0, 100)})
		res, err := fx.service.PunchHole(fx.rootContext(), handle, 200, 50)
		require.NoError(t, err)
		assert.Nil(t, res.PreOpBlocks)
	})

	t.Run("directory rejected", func(t *testing.T) {
		fx := newTestFixture(t)
		_, err := fx.service.PunchHole(fx.rootContext(), fx.rootHandle, 0, 10)
		require.Error(t, err)
	})
}

func TestService_Allocate(t *testing.T) {
	t.Run("extends size past EOF", func(t *testing.T) {
		fx := newTestFixture(t)
		handle := makeSparseFile(t, fx, "alloc.bin", 100, []block.BlockRef{bref(0, 100)})

		f, err := fx.service.Allocate(fx.rootContext(), handle, 50, 500) // 50+500 = 550 > 100
		require.NoError(t, err)
		assert.Equal(t, uint64(550), f.Size)

		got, _ := fx.store.GetFile(fx.rootContext().Context, handle)
		assert.Equal(t, uint64(550), got.Size)
		// The extended region is a hole (no block refs beyond 100).
		segs := block.Segments(got.Blocks, got.Size)
		require.GreaterOrEqual(t, len(segs), 2)
		last := segs[len(segs)-1]
		assert.Equal(t, block.SegmentHole, last.Kind)
		assert.Equal(t, uint64(550), last.End)
	})

	t.Run("range within current size is a no-op", func(t *testing.T) {
		fx := newTestFixture(t)
		handle := makeSparseFile(t, fx, "within.bin", 1000, []block.BlockRef{bref(0, 1000)})
		f, err := fx.service.Allocate(fx.rootContext(), handle, 100, 200)
		require.NoError(t, err)
		assert.Equal(t, uint64(1000), f.Size, "size unchanged when range fits")
	})

	t.Run("directory rejected", func(t *testing.T) {
		fx := newTestFixture(t)
		_, err := fx.service.Allocate(fx.rootContext(), fx.rootHandle, 0, 10)
		require.Error(t, err)
	})
}
