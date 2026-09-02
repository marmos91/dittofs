package badger

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func benchListStore(tb testing.TB, children int) (*BadgerMetadataStore, metadata.FileHandle) {
	tb.Helper()
	ctx := context.Background()
	store, err := NewBadgerMetadataStore(ctx, BadgerMetadataStoreConfig{DBPath: tb.TempDir()})
	if err != nil {
		tb.Fatalf("NewBadgerMetadataStore: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })

	if _, err := store.CreateRootDirectory(ctx, "/bench", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	}); err != nil {
		tb.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := store.GetRootHandle(ctx, "/bench")
	if err != nil {
		tb.Fatalf("GetRootHandle: %v", err)
	}

	for i := 0; i < children; i++ {
		name := fmt.Sprintf("file-%06d", i)
		fullPath := "/" + name
		handle, err := store.GenerateHandle(ctx, "/bench", fullPath)
		if err != nil {
			tb.Fatalf("GenerateHandle: %v", err)
		}
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			tb.Fatalf("DecodeFileHandle: %v", err)
		}
		file := &metadata.File{
			ShareName: "/bench", Path: fullPath, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000},
		}
		if err := store.UpdateAttrs(ctx, file); err != nil {
			tb.Fatalf("UpdateAttrs: %v", err)
		}
		if err := store.SetParent(ctx, handle, rootHandle); err != nil {
			tb.Fatalf("SetParent: %v", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
			tb.Fatalf("SetChild: %v", err)
		}
		if err := store.SetLinkCount(ctx, handle, 1); err != nil {
			tb.Fatalf("SetLinkCount: %v", err)
		}
	}
	return store, rootHandle
}

// BenchmarkListChildren measures the real ListChildren against a names-only
// walk of the same prefix — the floor an attrs-optional variant could reach.
func BenchmarkListChildren(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		store, rootHandle := benchListStore(b, n)
		ctx := context.Background()

		for _, mode := range []struct {
			name  string
			attrs metadata.ChildAttrs
		}{{"WithAttrs", metadata.WithAttrs}, {"NamesOnly", metadata.NamesOnly}} {
			b.Run(fmt.Sprintf("%s/n=%d", mode.name, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					entries, _, err := store.ListChildren(ctx, rootHandle, "", n, mode.attrs)
					if err != nil || len(entries) != n {
						b.Fatalf("got %d entries, err %v", len(entries), err)
					}
				}
			})
		}
	}
}
