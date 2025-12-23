package badger

import (
	"context"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CreateRootDirectory creates or retrieves the root directory for a share.
//
// This is a special operation used during share initialization. If a root directory
// already exists for this share (from a previous server run), it is returned.
// Otherwise, a new root directory is created with a new UUID and path "/".
//
// This idempotent behavior ensures that metadata persists across server restarts
// when using a persistent store like BadgerDB.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share
//   - attr: Directory attributes (Type must be FileTypeDirectory)
//
// Returns:
//   - *File: Complete file information for the root directory (existing or newly created)
//   - error: ErrInvalidArgument if not a directory, or other errors
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
		// First, check if a share mapping already exists
		item, err := txn.Get(keyShare(shareName))
		if err == nil {
			// Share exists - retrieve the existing root directory
			var existingShareData *shareData
			err = item.Value(func(val []byte) error {
				sd, err := decodeShareData(val)
				if err != nil {
					return err
				}
				existingShareData = sd
				return nil
			})
			if err != nil {
				return fmt.Errorf("failed to decode existing share data: %w", err)
			}

			// Decode the root handle to get the root file ID
			_, rootID, err := metadata.DecodeFileHandle(existingShareData.RootHandle)
			if err != nil {
				return fmt.Errorf("failed to decode existing root handle: %w", err)
			}

			// Get the existing root file
			rootItem, err := txn.Get(keyFile(rootID))
			if err != nil {
				return fmt.Errorf("failed to get existing root file: %w", err)
			}

			err = rootItem.Value(func(val []byte) error {
				rf, err := decodeFile(val)
				if err != nil {
					return err
				}
				rootFile = rf
				return nil
			})
			if err != nil {
				return fmt.Errorf("failed to decode existing root file: %w", err)
			}

			// Check if root directory attributes need to be updated from config
			// This handles the case where the config changed since the share was first created
			needsUpdate := false
			if rootFile.Mode != attr.Mode {
				logger.Info("Updating root directory mode from config",
					"share", shareName,
					"oldMode", fmt.Sprintf("%o", rootFile.Mode),
					"newMode", fmt.Sprintf("%o", attr.Mode))
				rootFile.Mode = attr.Mode
				needsUpdate = true
			}
			if rootFile.UID != attr.UID {
				logger.Info("Updating root directory UID from config",
					"share", shareName,
					"oldUID", rootFile.UID,
					"newUID", attr.UID)
				rootFile.UID = attr.UID
				needsUpdate = true
			}
			if rootFile.GID != attr.GID {
				logger.Info("Updating root directory GID from config",
					"share", shareName,
					"oldGID", rootFile.GID,
					"newGID", attr.GID)
				rootFile.GID = attr.GID
				needsUpdate = true
			}

			if needsUpdate {
				rootFile.Ctime = time.Now()
				fileBytes, err := encodeFile(rootFile)
				if err != nil {
					return fmt.Errorf("failed to encode updated root file: %w", err)
				}
				if err := txn.Set(keyFile(rootID), fileBytes); err != nil {
					return fmt.Errorf("failed to update root file: %w", err)
				}
				logger.Info("Root directory attributes updated from config",
					"share", shareName,
					"rootID", rootFile.ID)
			} else {
				logger.Debug("Reusing existing root directory for share (persisted from previous server run)",
					"share", shareName,
					"rootID", rootFile.ID)
			}
			return nil
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check for existing share: %w", err)
		}

		// Share doesn't exist - create a new root directory
		logger.Debug("Creating new root directory for share", "share", shareName)

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

		// Encode root handle
		rootHandle, err := metadata.EncodeFileHandle(rootFile)
		if err != nil {
			return fmt.Errorf("failed to encode root handle: %w", err)
		}

		// Store share-to-root mapping for persistence across restarts
		shareDataObj := &shareData{
			Share: metadata.Share{
				Name: shareName,
			},
			RootHandle: rootHandle,
		}
		shareBytes, err := encodeShareData(shareDataObj)
		if err != nil {
			return fmt.Errorf("failed to encode share data: %w", err)
		}
		if err := txn.Set(keyShare(shareName), shareBytes); err != nil {
			return fmt.Errorf("failed to store share data: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return rootFile, nil
}
