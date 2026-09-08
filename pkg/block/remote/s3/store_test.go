package s3

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"no scheme", "s3.cubbit.eu", "https://s3.cubbit.eu"},
		{"no scheme with port", "s3.fr-par.scw.cloud:443", "https://s3.fr-par.scw.cloud:443"},
		{"no scheme with path", "s3.example.com/custom", "https://s3.example.com/custom"},
		{"scheme in path", "s3.example.com/path://foo", "https://s3.example.com/path://foo"},
		{"https scheme", "https://s3.cubbit.eu", "https://s3.cubbit.eu"},
		{"http scheme", "http://localhost:4566", "http://localhost:4566"},
		{"non-http scheme", "s3://my-bucket", "s3://my-bucket"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeEndpoint(tt.endpoint)
			if got != tt.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

// TestS3_RemoteBlockStoreConformance_Endpoint runs the unified
// RemoteBlockStoreConformance suite against a real S3 endpoint, exercising
// the production wire path rather than the in-process mock.
//
// Skipped unless DITTOFS_S3_ENDPOINT (and the credential pair
// DITTOFS_S3_ACCESS_KEY / DITTOFS_S3_SECRET_KEY) are set in the
// environment. CI wires Localstack; local developers may run with `make
// e2e-s3` or by exporting the env directly.
func TestS3_RemoteBlockStoreConformance_Endpoint(t *testing.T) {
	endpoint := os.Getenv("DITTOFS_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DITTOFS_S3_ENDPOINT not set; skipping S3 conformance suite. Set the env var (with DITTOFS_S3_ACCESS_KEY/DITTOFS_S3_SECRET_KEY/DITTOFS_S3_BUCKET) to run against Localstack or MinIO.")
	}
	bucket := os.Getenv("DITTOFS_S3_BUCKET")
	if bucket == "" {
		bucket = "dittofs-conformance"
	}
	accessKey := os.Getenv("DITTOFS_S3_ACCESS_KEY")
	secretKey := os.Getenv("DITTOFS_S3_SECRET_KEY")
	region := os.Getenv("DITTOFS_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	forcePathStyle := true
	if v := os.Getenv("DITTOFS_S3_FORCE_PATH_STYLE"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			forcePathStyle = parsed
		}
	}

	blockstoretest.RemoteBlockStoreConformance(t, func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		// Per-subtest prefix so subtests do not see each other's objects.
		cfg := Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			KeyPrefix:      "conformance/" + t.Name() + "/",
			ForcePathStyle: forcePathStyle,
		}
		store, err := NewFromConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewFromConfig: %v", err)
		}
		// Cleanup walks the prefix and deletes every block object so the next
		// subtest starts clean. Best-effort: if the test failed mid-way,
		// residual objects may remain.
		cleanup := func() {
			ctx := context.Background()
			var ids []string
			_ = store.WalkBlocks(ctx, func(blockID string, _ block.Meta) error {
				ids = append(ids, blockID)
				return nil
			})
			for _, id := range ids {
				_ = store.DeleteBlock(ctx, id)
			}
			_ = store.Close()
		}
		return store, cleanup
	})
}

// TestS3_RemoteBlockStoreConformance runs the unified
// RemoteBlockStoreConformance suite against the S3 backend using the
// in-process mockS3 server (no Localstack/MinIO required). All subtests
// exercise the block-keyed (non-CAS) surface: PutBlock/GetBlock/
// GetBlockRange/DeleteBlock/WalkBlocks.
func TestS3_RemoteBlockStoreConformance(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		store, mock := newTestStore(t)
		// Force multi-page pagination so WalkBlocks_EnumeratesAll exercises
		// the paginator code path with the 5 blocks the suite inserts.
		mock.mu.Lock()
		mock.listPageSize = 2
		mock.mu.Unlock()
		return store, func() { _ = store.Close() }
	})
}
