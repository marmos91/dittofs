//go:build integration

// Integration tests for destination corruption vectors. Run with:
//
//	go test -tags=integration ./pkg/backup/destination/... -count=1
//
// Uses the SHARED Localstack container pattern (MEMORY.md: per-test
// containers are forbidden). Set LOCALSTACK_ENDPOINT to reuse an
// external Localstack instance instead of spinning one up.
//
// The tests here bypass the Destination interface to plant corrupted
// bytes directly on disk (FS driver) or via raw s3.Client.PutObject (S3
// driver), then assert the exact sentinel error surfaces at the
// documented boundary. Every production corruption mode that could
// silently cause data loss must have a failing-closed test here.
package destination_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/destination/fs"
	destinations3 "github.com/marmos91/dittofs/pkg/backup/destination/s3"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/backup/restore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// corruptionLocalstack owns the Localstack container + S3 client shared
// across every subtest in this file. One container per test binary
// invocation (MEMORY.md: per-test containers are forbidden).
var corruptionLocalstack struct {
	endpoint  string
	client    *awss3.Client
	container testcontainers.Container
}

// TestMain manages the Localstack container lifetime. LOCALSTACK_ENDPOINT
// env var, when set, bypasses container management entirely (useful on
// CI runners where Docker-in-Docker is unavailable).
func TestMain(m *testing.M) {
	cleanup := startLocalstackForCorruption()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// startLocalstackForCorruption starts (or reuses) Localstack and returns
// a cleanup callback. Any failure is fatal — corruption tests cannot run
// against a partial environment.
func startLocalstackForCorruption() func() {
	ctx := context.Background()

	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		client := initS3Client(endpoint)
		corruptionLocalstack.endpoint = endpoint
		corruptionLocalstack.client = client
		return func() {}
	}

	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.0",
		ExposedPorts: []string{"4566/tcp"},
		Env: map[string]string{
			"SERVICES":              "s3",
			"DEFAULT_REGION":        "us-east-1",
			"EAGER_SERVICE_LOADING": "1",
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("4566/tcp"),
			wait.ForHTTP("/_localstack/health").
				WithPort("4566/tcp").
				WithStartupTimeout(90*time.Second),
		),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("localstack start: %v", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		log.Fatalf("localstack host: %v", err)
	}
	port, err := c.MappedPort(ctx, "4566")
	if err != nil {
		_ = c.Terminate(ctx)
		log.Fatalf("localstack port: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	corruptionLocalstack.endpoint = endpoint
	corruptionLocalstack.container = c
	corruptionLocalstack.client = initS3Client(endpoint)
	return func() { _ = c.Terminate(context.Background()) }
}

// initS3Client builds a path-style S3 client pointing at endpoint with
// Localstack-accepted static "test"/"test" credentials.
func initS3Client(endpoint string) *awss3.Client {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// createCorruptionBucket creates the given bucket in Localstack. Fatals
// on error — the subtest cannot continue with a missing bucket.
func createCorruptionBucket(t *testing.T, name string) {
	t.Helper()
	_, err := corruptionLocalstack.client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

// deleteCorruptionBucket drains and removes bucket. Best-effort: cleanup
// errors are swallowed so a failed test still reports its real cause.
// Aborts in-flight multipart uploads before DeleteBucket to prevent leaks.
// Uses a paginator so buckets with >1000 objects are fully drained (a
// single ListObjectsV2 page caps at 1000, leaving residual objects that
// would block DeleteBucket).
func deleteCorruptionBucket(t *testing.T, name string) {
	t.Helper()
	paginator := awss3.NewListObjectsV2Paginator(corruptionLocalstack.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(name),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			break
		}
		for _, o := range page.Contents {
			_, _ = corruptionLocalstack.client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
				Bucket: aws.String(name),
				Key:    o.Key,
			})
		}
	}
	mpu, err := corruptionLocalstack.client.ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(name),
	})
	if err == nil {
		for _, u := range mpu.Uploads {
			_, _ = corruptionLocalstack.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(name),
				Key:      u.Key,
				UploadId: u.UploadId,
			})
		}
	}
	_, _ = corruptionLocalstack.client.DeleteBucket(context.Background(), &awss3.DeleteBucketInput{
		Bucket: aws.String(name),
	})
}

// uniqueBucket returns a Localstack-safe bucket name per test. Bucket
// names must be lowercase, 3..63 chars, no underscores.
func uniqueBucket(t *testing.T) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	bucket := "crp-" + name + "-" + strings.ToLower(ulid.Make().String()[:8])
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	return bucket
}

// randBytes returns n cryptographically-random bytes; used to build
// payloads distinguishable from accidental reuse across vectors.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// mkManifest builds a minimum-viable pre-write manifest for the
// corruption tests. PayloadIDSet is non-nil (empty slice is valid per
// SAFETY-01). StoreKind is "memory" because the tests don't care about
// cross-engine compatibility.
func mkManifest(id, storeID string) *manifest.Manifest {
	return &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        id,
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
		StoreID:         storeID,
		StoreKind:       "memory",
		Encryption:      manifest.Encryption{Enabled: false},
		PayloadIDSet:    []string{},
	}
}

// newFSDestination constructs an FS driver rooted at t.TempDir(). Returns
// the destination and the on-disk root path so corruption vectors can
// write raw bytes directly onto payload.bin / manifest.yaml.
func newFSDestination(t *testing.T) (destination.Destination, string) {
	t.Helper()
	dir := t.TempDir()
	repo := &models.BackupRepo{
		ID:   ulid.Make().String(),
		Kind: models.BackupRepoKindLocal,
	}
	require.NoError(t, repo.SetConfig(map[string]any{
		"path":         dir,
		"grace_window": "24h",
	}))
	s, err := fs.New(context.Background(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// newS3Destination constructs an S3 driver pointing at a fresh unique
// bucket in the shared Localstack. Returns the destination and the
// bucket name so corruption vectors can PutObject tampered bytes.
func newS3Destination(t *testing.T) (destination.Destination, string) {
	t.Helper()
	bucket := uniqueBucket(t)
	createCorruptionBucket(t, bucket)
	t.Cleanup(func() { deleteCorruptionBucket(t, bucket) })

	repo := &models.BackupRepo{
		ID:   ulid.Make().String(),
		Kind: models.BackupRepoKindS3,
	}
	require.NoError(t, repo.SetConfig(map[string]any{
		"bucket":           bucket,
		"region":           "us-east-1",
		"endpoint":         corruptionLocalstack.endpoint,
		"access_key":       "test",
		"secret_key":       "test",
		"force_path_style": true,
		"max_retries":      3,
		"grace_window":     "24h",
	}))
	s, err := destinations3.New(context.Background(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, bucket
}

// TestCorruptionHelpers_Smoke proves the helpers are wired correctly
// before any corruption vector exercises them. PutBackup + GetBackup
// round-trip on both drivers must succeed byte-for-byte.
func TestCorruptionHelpers_Smoke(t *testing.T) {
	t.Run("FS", func(t *testing.T) {
		dest, _ := newFSDestination(t)
		smokeRoundtrip(t, dest)
	})
	t.Run("S3", func(t *testing.T) {
		dest, _ := newS3Destination(t)
		smokeRoundtrip(t, dest)
	})
}

func smokeRoundtrip(t *testing.T, dest destination.Destination) {
	t.Helper()
	ctx := context.Background()
	id := ulid.Make().String()
	payload := randBytes(t, 1024)
	m := mkManifest(id, "store-smoke")

	require.NoError(t, dest.PutBackup(ctx, m, bytes.NewReader(payload)))

	_, rc, err := dest.GetBackup(ctx, id)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, got)
}

// ----------------------------------------------------------------------
// Task 2: TestCorruption table — 5 vectors × 2 drivers = 10 subtests.
//
// Each vector:
//  1. Constructs a fresh Destination (FS or S3).
//  2. PutBackup a valid 8 KiB payload.
//  3. Injects the vector's mutation by bypassing the Destination interface
//     (raw os.WriteFile/Remove for FS, raw s3.Client.PutObject/DeleteObject
//     for S3).
//  4. Calls GetBackup (or GetManifestOnly) and asserts the exact sentinel
//     error surfaces at the documented boundary.
//
// The manifest-version gate is double-covered: once at the Destination
// layer (Parse+Validate wraps the "unsupported manifest_version" string
// into ErrDestinationUnavailable) and once at the restore sentinel layer
// (TestManifestVersionGate_RestoreSentinel below confirms the sentinel
// is non-nil + matchable via errors.Is; Plan 02 exercises the full
// restore-executor path).
// ----------------------------------------------------------------------

const corruptionStoreID = "store-corruption-test"

// writeManifestRaw overwrites the manifest at <root-or-bucket>/<id>/manifest.yaml
// by bypassing the Destination interface. For FS: os.WriteFile. For S3:
// raw PutObject via the shared Localstack client.
func writeManifestRaw(t *testing.T, isS3 bool, root, bucket, id string, data []byte) {
	t.Helper()
	if isS3 {
		_, err := corruptionLocalstack.client.PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(id + "/manifest.yaml"),
			Body:   bytes.NewReader(data),
		})
		require.NoError(t, err)
		return
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, id, "manifest.yaml"), data, 0o600))
}

// writePayloadRaw overwrites the payload at <root-or-bucket>/<id>/payload.bin
// by bypassing the Destination interface. Used for TruncatedPayload and
// BitFlipPayload vectors.
func writePayloadRaw(t *testing.T, isS3 bool, root, bucket, id string, data []byte) {
	t.Helper()
	if isS3 {
		_, err := corruptionLocalstack.client.PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(id + "/payload.bin"),
			Body:   bytes.NewReader(data),
		})
		require.NoError(t, err)
		return
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, id, "payload.bin"), data, 0o600))
}

// deleteManifestRaw removes the manifest file by bypassing the Destination
// interface. Used for the MissingManifest vector.
func deleteManifestRaw(t *testing.T, isS3 bool, root, bucket, id string) {
	t.Helper()
	if isS3 {
		_, err := corruptionLocalstack.client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(id + "/manifest.yaml"),
		})
		require.NoError(t, err)
		return
	}
	require.NoError(t, os.Remove(filepath.Join(root, id, "manifest.yaml")))
}

// corruptionCase captures the matrix entry for a single corruption
// vector. `setup` applies the raw mutation; the expected assertion is
// selected from (wantErr, wantStoreID, wantErrContains) — exactly one of
// these three branches fires per case.
type corruptionCase struct {
	name string
	// setup applies the vector's raw mutation. `isS3` chooses the backend;
	// one of `root` (FS tempdir) or `bucket` (S3 bucket) is the empty
	// string, depending on the driver.
	setup func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest)
	// checkManifestOnly selects GetManifestOnly over GetBackup.
	checkManifestOnly bool
	// wantErr — if non-nil, assert require.ErrorIs on the returned error.
	wantErr error
	// wantStoreID — if non-empty, assert GetManifestOnly returns a parsed
	// manifest with this StoreID (Destination layer is agnostic to store
	// identity; the restore executor is the layer that emits
	// restore.ErrStoreIDMismatch).
	wantStoreID string
	// wantErrContains — if non-empty, assert the returned error's message
	// contains this substring. Used for the manifest-version gate where
	// the wrapped error is ErrDestinationUnavailable but the root cause
	// string is "unsupported manifest_version <n>".
	wantErrContains string
}

// runCorruptionCase is the per-subtest body shared by FS and S3.
func runCorruptionCase(t *testing.T, dest destination.Destination, root, bucket string, isS3 bool, tc corruptionCase) {
	t.Helper()
	ctx := context.Background()
	id := ulid.Make().String()
	payload := randBytes(t, 8192)
	m := mkManifest(id, corruptionStoreID)

	require.NoError(t, dest.PutBackup(ctx, m, bytes.NewReader(payload)))
	tc.setup(t, isS3, root, bucket, id, m)

	if tc.checkManifestOnly {
		got, err := dest.GetManifestOnly(ctx, id)
		switch {
		case tc.wantErr != nil:
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wantErr)
		case tc.wantStoreID != "":
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, tc.wantStoreID, got.StoreID)
		case tc.wantErrContains != "":
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErrContains)
		default:
			t.Fatalf("corruptionCase %q: no assertion configured", tc.name)
		}
		return
	}

	// GetBackup path: mismatch surfaces on Close (not Read).
	if tc.wantErr == nil {
		t.Fatalf("unhandled case: GetBackup path for %q requires wantErr", tc.name)
	}
	_, rc, err := dest.GetBackup(ctx, id)
	require.NoError(t, err)
	_, _ = io.ReadAll(rc)
	closeErr := rc.Close()
	require.ErrorIs(t, closeErr, tc.wantErr)
}

// TestCorruption is the 5-vector × 2-driver matrix. Total: 10 subtests.
// Each vector asserts a specific sentinel at a specific boundary — no
// generic "returns error" checks. See plan 07-01 for the full vector
// table and the rationale behind each sentinel choice.
func TestCorruption(t *testing.T) {
	cases := []corruptionCase{
		{
			name: "TruncatedPayload",
			setup: func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest) {
				writePayloadRaw(t, isS3, root, bucket, id, []byte("truncated-"))
			},
			wantErr: destination.ErrSHA256Mismatch,
		},
		{
			name: "BitFlipPayload",
			setup: func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest) {
				// Same length as the published payload — Read sees no short-read,
				// only bad bytes; mismatch surfaces on Close.
				writePayloadRaw(t, isS3, root, bucket, id, randBytes(t, 8192))
			},
			wantErr: destination.ErrSHA256Mismatch,
		},
		{
			name: "MissingManifest",
			setup: func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest) {
				deleteManifestRaw(t, isS3, root, bucket, id)
			},
			checkManifestOnly: true,
			wantErr:           destination.ErrManifestMissing,
		},
		{
			name: "WrongStoreID",
			setup: func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest) {
				// Rewrite manifest.yaml with StoreID="wrong-store-id". The
				// manifest is still structurally valid (version=1, all
				// required fields present) — only the ownership field
				// points at a foreign store. Destination layer returns the
				// parsed manifest intact; the restore executor is the
				// layer that emits restore.ErrStoreIDMismatch.
				tampered := *m
				tampered.StoreID = "wrong-store-id"
				data, err := tampered.Marshal()
				require.NoError(t, err)
				writeManifestRaw(t, isS3, root, bucket, id, data)
			},
			checkManifestOnly: true,
			wantStoreID:       "wrong-store-id",
		},
		{
			name: "ManifestVersionUnsupported",
			setup: func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest) {
				// Rewrite manifest.yaml with manifest_version=2. Both drivers
				// wrap Parse+Validate errors as ErrDestinationUnavailable
				// with the root-cause string from manifest.Validate
				// ("unsupported manifest_version 2 (this build supports 1)")
				// included in err.Error(). Plan 02 wires the restore
				// executor to re-surface restore.ErrManifestVersionUnsupported.
				tampered := *m
				tampered.ManifestVersion = 2
				data, err := tampered.Marshal()
				require.NoError(t, err)
				writeManifestRaw(t, isS3, root, bucket, id, data)
			},
			checkManifestOnly: true,
			wantErrContains:   "unsupported manifest_version",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"/FS", func(t *testing.T) {
			dest, root := newFSDestination(t)
			runCorruptionCase(t, dest, root, "", false, tc)
		})
		t.Run(tc.name+"/S3", func(t *testing.T) {
			dest, bucket := newS3Destination(t)
			runCorruptionCase(t, dest, "", bucket, true, tc)
		})
	}
}

// TestManifestVersionGate_RestoreSentinel proves the Phase-5 sentinel
// exists, is matchable via errors.Is, and carries the expected text.
// Plan 02 of Phase 7 exercises the full restore-executor path that
// re-surfaces this sentinel after the destination layer's Parse+Validate
// rejects a forward-incompatible manifest.
func TestManifestVersionGate_RestoreSentinel(t *testing.T) {
	require.NotNil(t, restore.ErrManifestVersionUnsupported)
	require.Contains(t, restore.ErrManifestVersionUnsupported.Error(), "manifest version")
}
