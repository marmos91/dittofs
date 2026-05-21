package s3

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/blockstoretest"
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

// TestStore_BlockStoreConformance runs the unified Phase 17 D-09
// BlockStoreConformance suite against the S3 backend.
//
// Skipped unless DITTOFS_S3_ENDPOINT (and the credential pair
// DITTOFS_S3_ACCESS_KEY / DITTOFS_S3_SECRET_KEY) are set in the
// environment — production-style S3 conformance requires either a
// Localstack/MinIO container or a real bucket to exercise the wire
// path. CI wires Localstack; local developers may run with `make
// e2e-s3` or by exporting the env directly.
//
// Per D-09 the S3 backend does NOT implement BlockStoreAppend; only
// BlockStoreConformance runs here. The BSCAS-06 x-amz-meta-content-hash
// header round-trip is exercised by verifier_test.go (the header is an
// fs-internal defense-in-depth marker and is not part of the unified
// Meta surface — D-08).
//
// Plan 17-07 adds the missing Has() method on *Store; until then the
// factory return type does not type-check.
func TestStore_BlockStoreConformance(t *testing.T) {
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

	factory := func(t *testing.T) (blockstore.BlockStore, func()) {
		t.Helper()
		// Per-subtest prefix so subtests do not see each other's objects.
		prefix := "conformance/" + t.Name() + "/"
		cfg := Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			KeyPrefix:      prefix,
			ForcePathStyle: forcePathStyle,
		}
		store, err := NewFromConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewFromConfig: %v", err)
		}
		// Cleanup walks the prefix and deletes every CAS object so the
		// next subtest starts clean. This is best-effort; if the test
		// failed mid-way, residual objects may remain.
		cleanup := func() {
			ctx := context.Background()
			_ = store.Walk(ctx, func(h blockstore.ContentHash, _ blockstore.Meta) error {
				_ = store.Delete(ctx, h)
				return nil
			})
			_ = store.Close()
		}
		return store, cleanup
	}
	blockstoretest.BlockStoreConformance(t, factory)
}
