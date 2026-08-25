package badger_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	blockpkg "github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// 2^63: one above MaxInt64, so it parses as a uint64 but not as an offset.
const highBitOffset = "9223372036854775808"

func newChunkIndexStore(t *testing.T) *badger.BadgerMetadataStore {
	t.Helper()
	store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// A row whose ID suffix exceeds MaxInt64 sits at an offset the covering guard
// cannot resolve, so the indexed lookup must refuse rather than answer "no chunk
// here" — the reader zero-fills a hole, which would serve zeros over a row whose
// range is merely unknown.
func TestGetFileChunkAtOffsetRefusesHighBitRow(t *testing.T) {
	ctx := context.Background()
	store := newChunkIndexStore(t)

	const payload = "p"
	if err := store.Put(ctx, &metadata.FileChunk{ID: payload + "/" + highBitOffset, DataSize: 4096}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.GetFileChunkAtOffset(ctx, payload, 100)
	if err == nil {
		t.Fatalf("got (%v, nil), want ErrManifestInconsistent: an unplaceable row was reported as a hole", got != nil)
	}
	if !errors.Is(err, blockpkg.ErrManifestInconsistent) {
		t.Fatalf("err = %v, want ErrManifestInconsistent", err)
	}
}

// One unreadable row must not make the whole payload unreadable: when another
// row covers the offset, that row still answers the read.
func TestGetFileChunkAtOffsetServesCoveredOffsetDespiteHighBitRow(t *testing.T) {
	ctx := context.Background()
	store := newChunkIndexStore(t)

	const payload = "p"
	covering := &metadata.FileChunk{ID: payload + "/0", DataSize: 4096}
	for _, row := range []*metadata.FileChunk{
		covering,
		{ID: payload + "/" + highBitOffset, DataSize: 4096},
	} {
		if err := store.Put(ctx, row); err != nil {
			t.Fatalf("Put(%s): %v", row.ID, err)
		}
	}

	got, err := store.GetFileChunkAtOffset(ctx, payload, 100)
	if err != nil {
		t.Fatalf("GetFileChunkAtOffset: %v", err)
	}
	if got == nil || got.ID != covering.ID {
		t.Fatalf("got %v, want the covering row %q", got, covering.ID)
	}
}
