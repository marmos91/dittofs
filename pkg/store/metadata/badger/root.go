package badger

import (
	"context"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CreateRootDirectory creates a root directory for a share without a parent.
//
// This is a special operation used during share initialization. The root directory
// is created with a new UUID and path "/" and has no parent.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share
//   - attr: Directory attributes (Type must be FileTypeDirectory)
//
// Returns:
//   - *File: Complete file information for the newly created root directory
//   - error: ErrAlreadyExists if root exists, ErrInvalidArgument if not a directory
func (s *BadgerMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
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

	var rootFile *metadata.File

	// Execute in transaction for atomicity
	err := s.db.Update(func(txn *badger.Txn) error {
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
		if rootAttrCopy.CreationTime.IsZero() {
			rootAttrCopy.CreationTime = now
		}

		// Create complete File struct for root directory
		rootFile = &metadata.File{
			ID:        uuid.New(),
			ShareName: shareName,
			Path:      "/",
			FileAttr:  rootAttrCopy,
		}

		// Encode and store file
		fileBytes, err := encodeFile(rootFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(rootFile.ID), fileBytes); err != nil {
			return fmt.Errorf("failed to store root file data: %w", err)
		}

		// Set link count to 2 (. + share reference)
		if err := txn.Set(keyLinkCount(rootFile.ID), encodeUint32(2)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return rootFile, nil
}
