// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains batch operations for the S3 content store, including
// listing all content and batch deletion for garbage collection.
package s3

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ListAllContent returns all content IDs in the S3 bucket.
//
// This lists all objects with the configured key prefix and returns their
// content IDs (which are the full file paths).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all content IDs (file paths)
//   - error: Returns error for S3 failures or context cancellation
func (s *S3ContentStore) ListAllContent(ctx context.Context) ([]metadata.ContentID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var contentIDs []metadata.ContentID

	prefix := s.keyPrefix
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}

			// Remove prefix to get content ID (which is the file path)
			key := *obj.Key
			if s.keyPrefix != "" && len(key) > len(s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}

			contentIDs = append(contentIDs, metadata.ContentID(key))
		}
	}

	return contentIDs, nil
}

// DeleteBatch removes multiple content items in one operation.
//
// S3 supports batch deletes of up to 1000 objects at a time. This implementation
// automatically chunks larger batches.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Returns error for catastrophic failures or context cancellation
func (s *S3ContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	failures := make(map[metadata.ContentID]error)

	// S3 allows max 1000 objects per delete request
	const maxBatchSize = 1000

	for i := 0; i < len(ids); i += maxBatchSize {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(ids); j++ {
				failures[ids[j]] = ctx.Err()
			}
			return failures, ctx.Err()
		}

		end := i + maxBatchSize
		if end > len(ids) {
			end = len(ids)
		}

		batch := ids[i:end]

		// Build delete objects input
		objects := make([]types.ObjectIdentifier, len(batch))
		for j, id := range batch {
			key := s.getObjectKey(id)
			objects[j] = types.ObjectIdentifier{
				Key: aws.String(key),
			}
		}

		// Execute batch delete
		result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{
				Objects: objects,
				Quiet:   aws.Bool(false),
			},
		})
		if err != nil {
			for _, id := range batch {
				failures[id] = err
			}
			continue
		}

		// Check for individual errors
		for _, deleteErr := range result.Errors {
			if deleteErr.Key == nil {
				continue
			}

			// Find the ContentID for this key (remove prefix to get path)
			key := *deleteErr.Key
			if s.keyPrefix != "" && len(key) > len(s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}

			id := metadata.ContentID(key)
			errMsg := "unknown error"
			if deleteErr.Code != nil && deleteErr.Message != nil {
				errMsg = fmt.Sprintf("%s: %s", *deleteErr.Code, *deleteErr.Message)
			}
			failures[id] = errors.New(errMsg)
		}
	}

	return failures, nil
}
