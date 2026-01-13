//go:build integration

package s3

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/store/block"
)

// createTestClient creates an S3 client for testing.
// Uses LOCALSTACK_ENDPOINT environment variable if set,
// otherwise defaults to localhost:4566.
func createTestClient(t *testing.T) *s3.Client {
	t.Helper()

	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}

	ctx := context.Background()
	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("us-east-1"),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
	})

	return client
}

// createTestBucket creates a test bucket and returns a cleanup function.
func createTestBucket(t *testing.T, client *s3.Client, bucketName string) func() {
	t.Helper()

	ctx := context.Background()

	// Create bucket
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Return cleanup function
	return func() {
		// List and delete all objects
		listResp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucketName),
		})
		if err == nil && listResp != nil {
			for _, obj := range listResp.Contents {
				_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(bucketName),
					Key:    obj.Key,
				})
			}
		}

		// Delete bucket
		_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{
			Bucket: aws.String(bucketName),
		})
	}
}

func TestStore_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-write-read")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-write-read",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world from s3")

	// Write block
	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read block
	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}
}

func TestStore_ReadBlockNotFound(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-not-found")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-not-found",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	_, err := store.ReadBlock(ctx, "nonexistent")
	if err != block.ErrBlockNotFound {
		t.Errorf("ReadBlock returned error %v, want %v", err, block.ErrBlockNotFound)
	}
}

func TestStore_ReadBlockRange(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-range")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-range",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read range
	read, err := store.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "hello" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	// Read range from middle
	read, err = store.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func TestStore_DeleteBlock(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-delete")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-delete",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Delete block
	if err := store.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	// Verify block is deleted
	_, err := store.ReadBlock(ctx, blockKey)
	if err != block.ErrBlockNotFound {
		t.Errorf("ReadBlock after delete returned error %v, want %v", err, block.ErrBlockNotFound)
	}
}

func TestStore_DeleteByPrefix(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-delete-prefix")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-delete-prefix",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := store.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// Delete all blocks for share1/content123
	if err := store.DeleteByPrefix(ctx, "share1/content123/"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	// Verify share1/content123 blocks are deleted
	for key := range blocks {
		_, err := store.ReadBlock(ctx, key)
		if len(key) > 17 && key[:17] == "share1/content123" {
			if err != block.ErrBlockNotFound {
				t.Errorf("ReadBlock(%s) after delete returned error %v, want %v", key, err, block.ErrBlockNotFound)
			}
		} else {
			if err != nil {
				t.Errorf("ReadBlock(%s) after delete returned unexpected error: %v", key, err)
			}
		}
	}
}

func TestStore_ListByPrefix(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-list-prefix")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-list-prefix",
		KeyPrefix: "blocks/",
	})
	defer store.Close()

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := store.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// List all blocks for share1/content123
	keys, err := store.ListByPrefix(ctx, "share1/content123/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3. Keys: %v", len(keys), keys)
	}

	// List all blocks for share1
	keys, err = store.ListByPrefix(ctx, "share1/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3. Keys: %v", len(keys), keys)
	}

	// List all blocks
	keys, err = store.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 4 {
		t.Errorf("ListByPrefix returned %d keys, want 4. Keys: %v", len(keys), keys)
	}
}

func TestStore_KeyPrefix(t *testing.T) {
	ctx := context.Background()
	client := createTestClient(t)
	cleanup := createTestBucket(t, client, "test-key-prefix")
	defer cleanup()

	store := New(client, Config{
		Bucket:    "test-key-prefix",
		KeyPrefix: "my-prefix/",
	})
	defer store.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("test data")

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Verify the object was created with the correct key prefix
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("test-key-prefix"),
		Key:    aws.String("my-prefix/" + blockKey),
	})
	if err != nil {
		t.Fatalf("Direct S3 GetObject failed: %v", err)
	}
	resp.Body.Close()

	// Verify we can read it back through the store
	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}
}
