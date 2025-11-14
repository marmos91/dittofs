package badger

import (
	"context"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// AddShare makes a filesystem path available to clients with specific access rules.
//
// This method creates a new share with a root directory using a BadgerDB transaction
// to ensure atomicity. The share configuration and root directory metadata are both
// persisted to the database.
//
// Implementation Details:
//   - Creates a root directory with the provided attributes
//   - Generates a path-based file handle for the root ("shareName:/")
//   - Initializes the root's link count to 2 ("." and share reference)
//   - Stores all metadata atomically in a single transaction
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: Unique identifier for the share
//   - options: Access control and authentication settings
//   - rootAttr: Initial attributes for the share's root directory
//
// Returns:
//   - error: ErrAlreadyExists if share exists, ErrInvalidArgument if not a directory
func (s *BadgerMetadataStore) AddShare(
	ctx context.Context,
	name string,
	options metadata.ShareOptions,
	rootAttr *metadata.FileAttr,
) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	// Validate root attributes
	if rootAttr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "share root must be a directory",
			Path:    name,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Execute in transaction for atomicity
	return s.db.Update(func(txn *badger.Txn) error {
		// Check if share already exists
		_, err := txn.Get(keyShare(name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: "share already exists",
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check share existence: %w", err)
		}

		// Complete root directory attributes with defaults
		rootAttrCopy := *rootAttr
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

		// Generate deterministic handle for root: "shareName:/"
		rootHandle, isPathBased := generateFileHandle(name, "/")

		// If hash-based, store the reverse mapping
		if !isPathBased {
			if err := storeHashedHandleMapping(txn, rootHandle, name, "/"); err != nil {
				return fmt.Errorf("failed to store handle mapping: %w", err)
			}
		}

		// Create fileData for root directory
		fileData := &fileData{
			Attr:      &rootAttrCopy,
			ShareName: name,
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

		// Store share configuration
		shareData := &shareData{
			Share: metadata.Share{
				Name:    name,
				Options: options,
			},
			RootHandle: rootHandle,
		}
		shareBytes, err := encodeShareData(shareData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyShare(name), shareBytes); err != nil {
			return fmt.Errorf("failed to store share: %w", err)
		}

		return nil
	})
}

// GetShares returns all configured shares.
//
// This scans all share entries in the database using a range query over the
// share key prefix.
//
// Thread Safety: Safe for concurrent use.
//
// Returns:
//   - []Share: List of all share configurations (may be empty)
//   - error: Only context cancellation errors or database errors
func (s *BadgerMetadataStore) GetShares(ctx context.Context) ([]metadata.Share, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var shares []metadata.Share

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = keySharePrefix()

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				sd, err := decodeShareData(val)
				if err != nil {
					return err
				}
				shares = append(shares, sd.Share)
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list shares: %w", err)
	}

	return shares, nil
}

// FindShare retrieves a share configuration by name.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The share name to look up
//
// Returns:
//   - *Share: The share configuration
//   - error: ErrNotFound if share doesn't exist
func (s *BadgerMetadataStore) FindShare(ctx context.Context, name string) (*metadata.Share, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var share *metadata.Share

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyShare(name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    name,
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			sd, err := decodeShareData(val)
			if err != nil {
				return err
			}
			share = &sd.Share
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return share, nil
}

// DeleteShare removes a share configuration.
//
// WARNING: This only removes the share configuration. It does NOT remove
// the underlying files, disconnect clients, or clean up sessions.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The share name to delete
//
// Returns:
//   - error: ErrNotFound if share doesn't exist
func (s *BadgerMetadataStore) DeleteShare(ctx context.Context, name string) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Check if share exists
		_, err := txn.Get(keyShare(name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    name,
			}
		}
		if err != nil {
			return err
		}

		// Delete the share
		if err := txn.Delete(keyShare(name)); err != nil {
			return fmt.Errorf("failed to delete share: %w", err)
		}

		return nil
	})
}

// GetShareRoot returns the root directory handle for a share.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The name of the share
//
// Returns:
//   - FileHandle: Root directory handle for the share
//   - error: ErrNotFound if share doesn't exist
func (s *BadgerMetadataStore) GetShareRoot(ctx context.Context, name string) (metadata.FileHandle, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var rootHandle metadata.FileHandle

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyShare(name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    name,
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			sd, err := decodeShareData(val)
			if err != nil {
				return err
			}
			rootHandle = sd.RootHandle
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return rootHandle, nil
}

// RecordShareMount records that a client has successfully mounted a share.
//
// This creates a session tracking record in the database. Sessions are stored
// with a composite key format: "m:shareName|clientAddr"
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: The name of the share being mounted
//   - clientAddr: The network address of the client
//
// Returns:
//   - error: ErrNotFound if share doesn't exist
func (s *BadgerMetadataStore) RecordShareMount(ctx context.Context, shareName, clientAddr string) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Verify share exists
		_, err := txn.Get(keyShare(shareName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    shareName,
			}
		}
		if err != nil {
			return err
		}

		// Create session record
		session := &metadata.ShareSession{
			ShareName:  shareName,
			ClientAddr: clientAddr,
			MountedAt:  time.Now(),
		}

		sessionBytes, err := encodeShareSession(session)
		if err != nil {
			return err
		}

		// Store with upsert semantics (replaces if exists)
		if err := txn.Set(keySession(shareName, clientAddr), sessionBytes); err != nil {
			return fmt.Errorf("failed to record share mount: %w", err)
		}

		return nil
	})
}

// GetActiveShares returns all currently active share sessions.
//
// This scans all session entries in the database using a range query over the
// session key prefix.
//
// Thread Safety: Safe for concurrent use.
//
// Returns:
//   - []ShareSession: List of all active sessions (may be empty)
//   - error: Only context cancellation or database errors
func (s *BadgerMetadataStore) GetActiveShares(ctx context.Context) ([]metadata.ShareSession, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Initialize to empty slice (not nil) to match test expectations
	sessions := []metadata.ShareSession{}

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = keySessionPrefix()

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				session, err := decodeShareSession(val)
				if err != nil {
					return err
				}
				sessions = append(sessions, *session)
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list active shares: %w", err)
	}

	return sessions, nil
}

// RemoveShareMount removes a specific client's mount session of a share.
//
// This operation is idempotent - removing a non-existent session succeeds.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: The name of the share being unmounted
//   - clientAddr: The network address of the client
//
// Returns:
//   - error: Only context cancellation or database errors (not ErrNotFound)
func (s *BadgerMetadataStore) RemoveShareMount(ctx context.Context, shareName, clientAddr string) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Delete the session (idempotent - no error if doesn't exist)
		err := txn.Delete(keySession(shareName, clientAddr))
		if err != nil && err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to remove share mount: %w", err)
		}

		return nil
	})
}
