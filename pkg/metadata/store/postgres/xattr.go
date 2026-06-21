package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Extended-attribute (xattr) operations delegate to the shared resolver in
// pkg/metadata/xattr.go over the Files interface. Both the Postgres store and
// its transaction satisfy metadata.Files, so the same Resolve* helpers serve
// both. Inline values ride the existing eas JSONB column.

// GetXattr implements metadata.Files.
func (s *PostgresMetadataStore) GetXattr(ctx context.Context, handle metadata.FileHandle, name string) ([]byte, bool, error) {
	return metadata.ResolveGetXattr(ctx, s, handle, name, nil)
}

// SetXattr implements metadata.Files.
func (s *PostgresMetadataStore) SetXattr(ctx context.Context, handle metadata.FileHandle, name string, value []byte) error {
	return metadata.ResolveSetXattr(ctx, s, handle, name, value)
}

// RemoveXattr implements metadata.Files.
func (s *PostgresMetadataStore) RemoveXattr(ctx context.Context, handle metadata.FileHandle, name string) error {
	return metadata.ResolveRemoveXattr(ctx, s, handle, name)
}

// ListXattr implements metadata.Files.
func (s *PostgresMetadataStore) ListXattr(ctx context.Context, handle metadata.FileHandle) ([]string, error) {
	return metadata.ResolveListXattr(ctx, s, handle)
}

// GetXattr implements metadata.Files for the transaction receiver.
func (tx *postgresTransaction) GetXattr(ctx context.Context, handle metadata.FileHandle, name string) ([]byte, bool, error) {
	return metadata.ResolveGetXattr(ctx, tx, handle, name, nil)
}

// SetXattr implements metadata.Files for the transaction receiver.
func (tx *postgresTransaction) SetXattr(ctx context.Context, handle metadata.FileHandle, name string, value []byte) error {
	return metadata.ResolveSetXattr(ctx, tx, handle, name, value)
}

// RemoveXattr implements metadata.Files for the transaction receiver.
func (tx *postgresTransaction) RemoveXattr(ctx context.Context, handle metadata.FileHandle, name string) error {
	return metadata.ResolveRemoveXattr(ctx, tx, handle, name)
}

// ListXattr implements metadata.Files for the transaction receiver.
func (tx *postgresTransaction) ListXattr(ctx context.Context, handle metadata.FileHandle) ([]string, error) {
	return metadata.ResolveListXattr(ctx, tx, handle)
}
