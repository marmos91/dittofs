package badger

import (
	"context"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CreateRootDirectory creates a root directory for a share without a parent.
//
// This is a special operation used during share initialization. The root directory
// is created with a handle in the format "shareName:/" and has no parent.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share (used to generate root handle)
//   - attr: Directory attributes (Type must be FileTypeDirectory)
//
// Returns:
//   - FileHandle: Handle of the newly created root directory
//   - error: ErrAlreadyExists if root exists, ErrInvalidArgument if not a directory
func (s *BadgerMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (metadata.FileHandle, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate attributes
	if attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "root must be a directory",
			Path:    shareName,
		}
	}

	// Generate deterministic handle for root: "shareName:/"
	rootHandle, isPathBased := generateFileHandle(shareName, "/")

	// Execute in transaction for atomicity
	err := s.db.Update(func(txn *badger.Txn) error {
		// Check if root already exists - if so, just return success (idempotent)
		_, err := txn.Get(keyFile(rootHandle))
		if err == nil {
			// Root already exists, this is OK (idempotent operation)
			return nil
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check root existence: %w", err)
		}

		// Root doesn't exist, create it
		// Complete root directory attributes with defaults
		rootAttrCopy := *attr
		if rootAttrCopy.Mode == 0 {
			rootAttrCopy.Mode = 0755
		}
		now := time.Now()
		if rootAttrCopy.Atime.IsZero() {
			rootAttrCopy.Atime = now
		}
		if rootAttrCopy.Mtime.IsZero() {
			rootAttrCopy.Mtime = now
		}
		if rootAttrCopy.Ctime.IsZero() {
			rootAttrCopy.Ctime = now
		}

		// If hash-based, store the reverse mapping
		if !isPathBased {
			if err := storeHashedHandleMapping(txn, rootHandle, shareName, "/"); err != nil {
				return fmt.Errorf("failed to store handle mapping: %w", err)
			}
		}

		// Create fileData for root directory
		fileData := &fileData{
			Attr:      &rootAttrCopy,
			ShareName: shareName,
		}
		fileBytes, err := encodeFileData(fileData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(rootHandle), fileBytes); err != nil {
			return fmt.Errorf("failed to store root file data: %w", err)
		}

		// Set link count to 2 (. + share reference)
		if err := txn.Set(keyLinkCount(rootHandle), encodeUint32(2)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return rootHandle, nil
}
