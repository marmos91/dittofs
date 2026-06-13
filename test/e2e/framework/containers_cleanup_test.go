//go:build e2e

package framework

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/sync/errgroup"
)

// TestCleanupBucket_PaginatesAndDeletesAll verifies that CleanupBucket
// paginates past the 1000-object ListObjectsV2 page boundary and removes
// every object before deleting the bucket. With the pre-fix implementation
// (single un-paginated list) the trailing 500 objects survive and DeleteBucket
// fails with BucketNotEmpty, leaving the bucket present.
func TestCleanupBucket_PaginatesAndDeletesAll(t *testing.T) {
	if !CheckLocalstackAvailable(t) {
		t.Skip("Localstack not available")
	}

	lh := NewLocalstackHelper(t)
	ctx := context.Background()

	bucketName := fmt.Sprintf("cleanup-pagination-%d", rand.Int63())
	if err := lh.CreateBucket(ctx, bucketName); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// Upload 1500 zero-byte objects — one page boundary above 1000.
	const objectCount = 1500
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(50)
	for i := 0; i < objectCount; i++ {
		key := fmt.Sprintf("obj-%05d", i)
		g.Go(func() error {
			_, err := lh.Client.PutObject(gctx, &s3.PutObjectInput{
				Bucket: aws.String(bucketName),
				Key:    aws.String(key),
				Body:   strings.NewReader(""),
			})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("uploading objects: %v", err)
	}

	// Sanity: confirm all objects are present before cleanup.
	if got := len(lh.ListS3Prefix(t, bucketName, "")); got != objectCount {
		t.Fatalf("setup: expected %d objects, got %d", objectCount, got)
	}

	lh.CleanupBucket(ctx, bucketName)

	// The bucket must no longer exist.
	_, err := lh.Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err == nil {
		// Secondary guard: surface how many objects remained.
		remaining := lh.ListS3Prefix(t, bucketName, "")
		t.Fatalf("bucket %s still exists after CleanupBucket; %d objects remain", bucketName, len(remaining))
	}

	// Confirm the error is a not-found (404 / NoSuchBucket), not some other
	// transport failure that would mask a real bug.
	var notFound *types.NotFound
	var noSuchBucket *types.NoSuchBucket
	var respErr *smithyhttp.ResponseError
	switch {
	case errors.As(err, &notFound):
	case errors.As(err, &noSuchBucket):
	case errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404:
	default:
		t.Fatalf("HeadBucket returned unexpected error (want 404/NoSuchBucket): %v", err)
	}
}
