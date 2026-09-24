package common

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

type cloneRangeStore struct {
	metadata.Store
	beforeTransaction func()
}

func (s *cloneRangeStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	if s.beforeTransaction != nil {
		hook := s.beforeTransaction
		s.beforeTransaction = nil
		hook()
	}
	return s.Store.WithTransaction(ctx, fn)
}

func setCloneTestSize(t *testing.T, ms metadata.Store, handle metadata.FileHandle, size uint64) {
	t.Helper()
	file, err := ms.GetFile(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	file.Size = size
	if err := ms.UpdateAttrs(context.Background(), file); err != nil {
		t.Fatal(err)
	}
}

func TestCloneRangeRevalidatesTransactionSizes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourceSize uint64
		destSize   uint64
		wantCode   metadata.ErrorCode
	}{
		{"destination_grew", 4096, 8192, metadata.ErrNotSupported},
		{"source_grew", 8192, 4096, metadata.ErrNotSupported},
		{"source_shrank", 2048, 4096, metadata.ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
			coord := &fakeCoordinator{}
			bs := newCloneTestEngineWithMS(t, coord, ms)
			refs := []block.ChunkRef{{Hash: block.ContentHash{1}, Size: 4096}}
			src := putTestFile(t, ms, "/src", "range-src", refs, 4096)
			dst := putTestFile(t, ms, "/dst", "range-dst", nil, 4096)
			store := &cloneRangeStore{Store: ms, beforeTransaction: func() {
				// The drain has finished, but the clone has not read the rows
				// it will replace. A preflight check cannot protect this window.
				setCloneTestSize(t, ms, src, tc.sourceSize)
				setCloneTestSize(t, ms, dst, tc.destSize)
			}}
			err := CloneWholeFile(ctx, bs, store, nil, src, dst, "range-dst", 4096)
			var se *metadata.StoreError
			if !errors.As(err, &se) || se.Code != tc.wantCode {
				t.Fatalf("clone error = %v, want code %v", err, tc.wantCode)
			}
			after, err := ms.GetFile(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			if after.Size != tc.destSize || len(after.Blocks) != 0 || len(coord.incrementCalls) != 0 {
				t.Fatalf("rejected clone mutated destination or refs: size=%d blocks=%d increments=%d", after.Size, len(after.Blocks), len(coord.incrementCalls))
			}
		})
	}
}

func TestLocalCloneRangePreservesGrowthDuringCopy(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, _ := newLocalOnlyTestEngine(t, &fakeCoordinator{}, ms)
	src := putTestFile(t, ms, "/src", "range-src", nil, 4096)
	dst := putTestFile(t, ms, "/dst", "range-dst", nil, 4096)
	source := bytes.Repeat([]byte{0x11}, 4096)
	tail := bytes.Repeat([]byte{0x33}, 4096)
	writeAndSeal(t, ctx, bs, "range-src", source)
	writeAndSeal(t, ctx, bs, "range-dst", bytes.Repeat([]byte{0x22}, 4096))
	grew := false
	store := &cloneRangeStore{Store: ms, beforeTransaction: func() {
		// The copy has validated its bounds and written the prefix, but the
		// final size transaction has not read the destination yet.
		head := make([]byte, len(source))
		if _, err := bs.ReadAt(ctx, "range-dst", head, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(head, source) {
			t.Fatal("growth hook ran before the copied prefix was written")
		}
		grew = true
		if _, err := bs.WriteAt(ctx, "range-dst", nil, tail, 4096); err != nil {
			t.Fatal(err)
		}
		if _, err := bs.Flush(ctx, "range-dst"); err != nil {
			t.Fatal(err)
		}
		if err := bs.DrainRollups(ctx); err != nil {
			t.Fatal(err)
		}
		setCloneTestSize(t, ms, dst, 8192)
	}}
	if err := CloneWholeFile(ctx, bs, store, nil, src, dst, "range-dst", 4096); err != nil {
		t.Fatal(err)
	}
	if !grew {
		t.Fatal("growth hook was not reached")
	}
	after, err := ms.GetFile(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != 8192 {
		t.Errorf("concurrent growth lost: size=%d, want 8192", after.Size)
	}
	got := make([]byte, 8192)
	if _, err := bs.ReadAt(ctx, "range-dst", got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(source, tail...)) {
		t.Error("range clone did not preserve the concurrently appended tail")
	}
}

func TestLocalCloneRangeRejectsLongerDestination(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, _ := newLocalOnlyTestEngine(t, &fakeCoordinator{}, ms)
	src := putTestFile(t, ms, "/src", "range-src", nil, 4096)
	dst := putTestFile(t, ms, "/dst", "range-dst", nil, 8192)
	previous := bytes.Repeat([]byte{0x22}, 8192)
	writeAndSeal(t, ctx, bs, "range-src", bytes.Repeat([]byte{0x11}, 4096))
	writeAndSeal(t, ctx, bs, "range-dst", previous)
	err := CloneWholeFile(ctx, bs, ms, nil, src, dst, "range-dst", 0)
	var se *metadata.StoreError
	if !errors.As(err, &se) || se.Code != metadata.ErrNotSupported {
		t.Fatalf("clone error = %v, want unsupported", err)
	}
	got := make([]byte, len(previous))
	if _, err := bs.ReadAt(ctx, "range-dst", got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, previous) {
		t.Error("rejected clone modified the destination")
	}
}
