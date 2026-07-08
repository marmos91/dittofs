package backend

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// cleanS3Prefix deletes every object under bucket/prefix so a re-run starts from
// a clean slate. Two reasons this matters: juicefs `format` refuses a non-empty
// data prefix (its meta db lives on the disposable VM, so a fresh VM can never
// re-attach to prior S3 data — it's orphaned junk), and stale objects from an
// earlier run would skew a backend's read pass.
//
// Uses the vendored AWS SDK with path-style addressing for generic S3 endpoints;
// creds come from the process env (the S3-creds invariant), never argv.
func cleanS3Prefix(ctx context.Context, endpoint, bucket, prefix string) error {
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	cl := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1", // ignored by generic S3, but the SDK requires one
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(id, secret, ""),
	})
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
