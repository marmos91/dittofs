//go:build e2e

// Package helpers — CAS-related E2E helpers (Phase 11).
//
// These helpers are used by the canonical correctness E2E
// (TestBlockStoreImmutableOverwrites) and the BSCAS-06 external-verifier
// sanity test (TestExternalVerifier_ContentHashHeader). They isolate the
// "talk to S3 directly, bypassing DittoFS" pattern that proves the CAS
// contract holds outside the system.

package helpers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/test/e2e/framework"
)

// ListCASKeys returns the sorted list of object keys under the "cas/"
// prefix in bucketName. Bypasses DittoFS — talks straight to S3 via the
// shared Localstack helper. Used to assert immutability + GC behavior.
func ListCASKeys(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName string) []string {
	t.Helper()

	ctx := context.Background()
	var keys []string
	var continuationToken *string

	for {
		resp, err := lsHelper.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucketName),
			Prefix:            aws.String("cas/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			t.Fatalf("ListCASKeys: list cas/ objects in %s: %v", bucketName, err)
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

// HeadCASObject fetches the head metadata of a single CAS object.
// Returns the resp.Metadata map (lowercased, x-amz-meta- stripped by the
// AWS SDK) and the object's content length.
func HeadCASObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string) (metadata map[string]string, contentLength int64) {
	t.Helper()

	ctx := context.Background()
	resp, err := lsHelper.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadCASObject: HEAD %s/%s: %v", bucketName, key, err)
	}

	if resp.ContentLength != nil {
		contentLength = *resp.ContentLength
	}
	return resp.Metadata, contentLength
}

// GetCASObject fetches the body of a single CAS object directly from S3.
// Returns the raw bytes — no BLAKE3 verification (this is the
// external-verifier path; the test computes BLAKE3 itself).
func GetCASObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string) []byte {
	t.Helper()

	ctx := context.Background()
	resp, err := lsHelper.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetCASObject: GET %s/%s: %v", bucketName, key, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GetCASObject: read body of %s/%s: %v", bucketName, key, err)
	}
	return body
}

// PutCASObject overwrites the body of a CAS key with arbitrary bytes,
// preserving the key. Used by the canonical-correctness test to inject a
// tampered object and confirm the BLAKE3 streaming verifier rejects the
// next read (INV-06 tamper detection). The metadata header is preserved
// as-is (header pre-check should pass; body recompute should fail).
func PutCASObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string, body []byte, metadata map[string]string) {
	t.Helper()

	ctx := context.Background()
	_, err := lsHelper.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String(key),
		Body:     bytes.NewReader(body),
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("PutCASObject: PUT %s/%s: %v", bucketName, key, err)
	}
}

// DeleteCASObject removes a single CAS object directly from S3.
func DeleteCASObject(t *testing.T, lsHelper *framework.LocalstackHelper, bucketName, key string) {
	t.Helper()

	ctx := context.Background()
	_, err := lsHelper.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteCASObject: DELETE %s/%s: %v", bucketName, key, err)
	}
}

// CASKeySetDiff returns (added, removed) keys between before and after
// snapshots — used by the canonical test to log what changed across a
// write/overwrite/GC boundary. Both inputs must be sorted (ListCASKeys
// returns sorted results).
func CASKeySetDiff(before, after []string) (added, removed []string) {
	beforeSet := make(map[string]struct{}, len(before))
	for _, k := range before {
		beforeSet[k] = struct{}{}
	}
	afterSet := make(map[string]struct{}, len(after))
	for _, k := range after {
		afterSet[k] = struct{}{}
	}
	for k := range afterSet {
		if _, ok := beforeSet[k]; !ok {
			added = append(added, k)
		}
	}
	for k := range beforeSet {
		if _, ok := afterSet[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// TriggerBlockGC runs `dfsctl store block gc <share>` via the existing
// CLI runner. Returns nil on success, a non-nil error otherwise.
//
// Phase 11 status: the `dfsctl store block gc` subcommand lands in plan
// 11-07 (PR-C). Until that plan is merged, this helper returns an error
// (the dfsctl binary will reject the unknown subcommand). The canonical
// E2E recognizes that and SKIPS with an explanatory message. After 11-07
// merges, the helper succeeds and the test executes end-to-end.
func TriggerBlockGC(t *testing.T, runner *CLIRunner, shareName string) error {
	t.Helper()

	_, err := runner.Run("store", "block", "gc", shareName)
	if err != nil {
		return fmt.Errorf("dfsctl store block gc %s: %w", shareName, err)
	}
	return nil
}
