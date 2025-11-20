package badger

import (
	"context"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// GetAllContentIDs returns all ContentIDs referenced by metadata.
//
// This method scans all files across all shares and collects their ContentIDs.
// It's used by garbage collection to identify which content is still in use.
//
// Implementation Details:
//   - Iterates through all file records in BadgerDB
//   - Filters for regular files with non-empty ContentIDs
//   - Deduplicates ContentIDs (hard links may share content)
//   - Periodically checks context for cancellation
//
// Performance:
// For large filesystems (millions of files), this may take several minutes.
// The method scans the entire database but is optimized to:
//   - Use prefix iteration (efficient with LSM-trees)
//   - Check context every 1000 files for cancellation
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all referenced ContentIDs (deduplicated)
//   - error: Returns error if scan fails or context is cancelled
func (s *BadgerMetadataStore) GetAllContentIDs(ctx context.Context) ([]metadata.ContentID, error) {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use map for deduplication (hard links may share ContentIDs)
	contentIDs := make(map[metadata.ContentID]struct{})
	processedCount := 0

	err := s.db.View(func(txn *badger.Txn) error {
		// Set up iterator for all file records
		// File records have key prefix: "f:"
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte("f:")
		opts.PrefetchValues = true // We need to read file data

		it := txn.NewIterator(opts)
		defer it.Close()

		// Iterate through all file records
		for it.Rewind(); it.Valid(); it.Next() {
			// Check for cancellation periodically (every 1000 files)
			processedCount++
			if processedCount%1000 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}

			item := it.Item()

			// Decode file data to get attributes
			err := item.Value(func(val []byte) error {
				fileData, err := decodeFileData(val)
				if err != nil {
					// Skip corrupted entries
					return nil
				}

				// Only collect ContentIDs from regular files
				if fileData.Attr.Type == metadata.FileTypeRegular && fileData.Attr.ContentID != "" {
					contentIDs[fileData.Attr.ContentID] = struct{}{}
				}

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to scan metadata: %w", err)
	}

	// Convert map to slice
	result := make([]metadata.ContentID, 0, len(contentIDs))
	for id := range contentIDs {
		result = append(result, id)
	}

	return result, nil
}
