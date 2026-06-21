package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Extended-attribute (xattr) operations delegate to the shared resolver in
// pkg/metadata/xattr.go, which unifies the inline EA backing with named-stream
// child entities over the Files interface. The memory store and its
// transaction both satisfy metadata.Files, so the same Resolve* helpers serve
// both. The bare store layer has no block-store reader wired, so stream-backed
// values are surfaced by the Service tier; here the inline backing is
// authoritative for Get.

// GetXattr implements metadata.Files.
func (store *MemoryMetadataStore) GetXattr(ctx context.Context, handle metadata.FileHandle, name string) ([]byte, bool, error) {
	return metadata.ResolveGetXattr(ctx, store, handle, name, nil)
}

// SetXattr implements metadata.Files.
func (store *MemoryMetadataStore) SetXattr(ctx context.Context, handle metadata.FileHandle, name string, value []byte) error {
	return metadata.ResolveSetXattr(ctx, store, handle, name, value)
}

// RemoveXattr implements metadata.Files.
func (store *MemoryMetadataStore) RemoveXattr(ctx context.Context, handle metadata.FileHandle, name string) error {
	return metadata.ResolveRemoveXattr(ctx, store, handle, name)
}

// ListXattr implements metadata.Files.
func (store *MemoryMetadataStore) ListXattr(ctx context.Context, handle metadata.FileHandle) ([]string, error) {
	return metadata.ResolveListXattr(ctx, store, handle)
}

// GetXattr implements metadata.Files for the transaction receiver.
func (tx *memoryTransaction) GetXattr(ctx context.Context, handle metadata.FileHandle, name string) ([]byte, bool, error) {
	return metadata.ResolveGetXattr(ctx, tx, handle, name, nil)
}

// SetXattr implements metadata.Files for the transaction receiver.
func (tx *memoryTransaction) SetXattr(ctx context.Context, handle metadata.FileHandle, name string, value []byte) error {
	return metadata.ResolveSetXattr(ctx, tx, handle, name, value)
}

// RemoveXattr implements metadata.Files for the transaction receiver.
func (tx *memoryTransaction) RemoveXattr(ctx context.Context, handle metadata.FileHandle, name string) error {
	return metadata.ResolveRemoveXattr(ctx, tx, handle, name)
}

// ListXattr implements metadata.Files for the transaction receiver.
func (tx *memoryTransaction) ListXattr(ctx context.Context, handle metadata.FileHandle) ([]string, error) {
	return metadata.ResolveListXattr(ctx, tx, handle)
}
