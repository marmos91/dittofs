//go:build e2e

// Package helpers — packed-block E2E helpers.
//
// These helpers mirror the CAS helpers in cas.go but target the packed
// "blocks/" object namespace that the live write path now produces. They
// isolate the "talk to S3 directly, bypassing DittoFS" pattern that proves
// the blocks-only storage contract holds outside the system: that new
// writes land as blocks/ objects (not per-chunk cas/ objects), that GC
// frees them, and that a tampered block is rejected on read (fail-closed).
package helpers

import (
	"bytes"
	"context"
	"io"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/test/e2e/framework"
)

// ListBlockKeys returns the sorted list of object keys under the "blocks/"
// prefix in bucketName. Bypasses DittoFS — talks straight to S3 via the
// shared Localstack helper. Used to assert that new writes produce packed
// block objects and that GC reaps them.
func ListBlockKeys(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName string) []string {
	t.Helper()

	ctx := context.Background()
	var keys []string
	var continuationToken *string

	for {
		resp, err := lsHelper.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucketName),
			Prefix:            aws.String("blocks/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			t.Fatalf("ListBlockKeys: list blocks/ objects in %s: %v", bucketName, err)
		}

		for _, obj := range resp.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}

		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		continuationToken = resp.NextContinuationToken
	}

	sort.Strings(keys)
	return keys
}

// GetBlockObject fetches the raw body of a single blocks/ object directly
// from S3. No BLAKE3 verification — this is the external path; callers use
// it to snapshot and then tamper a block.
func GetBlockObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string) []byte {
	t.Helper()

	ctx := context.Background()
	resp, err := lsHelper.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetBlockObject: GET %s/%s: %v", bucketName, key, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GetBlockObject: read body of %s/%s: %v", bucketName, key, err)
	}
	return body
}

// PutBlockObject overwrites the body of a blocks/ key with arbitrary bytes,
// preserving the key. Used by the corruption test to inject a tampered
// block and confirm the read path fails closed (BLAKE3 verification catches
// the mismatch on the next remote fetch).
func PutBlockObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string, body []byte) {
	t.Helper()

	ctx := context.Background()
	_, err := lsHelper.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("PutBlockObject: PUT %s/%s: %v", bucketName, key, err)
	}
}
