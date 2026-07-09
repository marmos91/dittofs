package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3PrefixBytes sums the sizes of all objects under benchEnv.Bucket/prefix.
func s3PrefixBytes(ctx context.Context, prefix string) (int64, error) {
	cl, err := newS3Client()
	if err != nil {
		return 0, err
	}
	var total int64
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{
		Bucket: aws.String(benchEnv.Bucket), Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, o := range page.Contents {
			total += aws.ToInt64(o.Size)
		}
	}
	return total, nil
}

// waitS3Settled blocks until the byte total under prefix stops growing — used to
// confirm an rclone (no-dedup, physical==logical) writeback upload has fully
// drained before its unmount, so the remounted cold read isn't racing an
// in-flight upload. Bounded to ~120s; best-effort.
func waitS3Settled(ctx context.Context, prefix string) error {
	var last int64 = -1
	stable := 0
	for i := 0; i < 120; i++ {
		n, err := s3PrefixBytes(ctx, prefix)
		if err != nil {
			return err
		}
		if n > 0 && n == last {
			if stable++; stable >= 4 { // unchanged for ~4 polls ⇒ upload done
				return nil
			}
		} else {
			stable = 0
		}
		last = n
		time.Sleep(time.Second)
	}
	return nil
}

// newS3Client builds an S3 client for a generic (SCW/MinIO/…) endpoint with
// path-style addressing; creds come from the process env (the S3-creds
// invariant), never argv.
func newS3Client() (*s3.Client, error) {
	id, secret, err := s3Creds()
	if err != nil {
		return nil, err
	}
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(benchEnv.Endpoint),
		Region:       "us-east-1", // ignored by generic S3, but the SDK requires one
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(id, secret, ""),
	}), nil
}

// cleanS3Prefix deletes every object under benchEnv.Bucket/prefix so a re-run
// starts from a clean slate. Stale objects would skew a backend's read pass, and
// s3ql/rclone reuse fixed prefixes across runs.
func cleanS3Prefix(ctx context.Context, prefix string) error {
	cl, err := newS3Client()
	if err != nil {
		return err
	}
	bucket := benchEnv.Bucket
	// A ListObjectsV2 page holds ≤1000 keys and DeleteObjects accepts ≤1000, so
	// one delete per page needs no extra batching.
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list %s/%s: %w", bucket, prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, len(page.Contents))
		for i, o := range page.Contents {
			ids[i] = types.ObjectIdentifier{Key: o.Key}
		}
		if _, err := cl.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("delete %s/%s: %w", bucket, prefix, err)
		}
	}
	return nil
}
