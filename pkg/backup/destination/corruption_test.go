//go:build integration

// Run with: go test -tags=integration ./pkg/backup/destination/... -count=1
// Set LOCALSTACK_ENDPOINT to reuse an external Localstack instance.
package destination_test

import (
	"bytes"
	"context"
	"crypto/rand"
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

// one container per test binary invocation — per-test containers are forbidden
var corruptionLocalstack struct {
	endpoint  string
	client    *awss3.Client
	container testcontainers.Container
}

func TestMain(m *testing.M) {
	cleanup := startLocalstackForCorruption()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

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

func createCorruptionBucket(t *testing.T, name string) {
	t.Helper()
	_, err := corruptionLocalstack.client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

// deleteCorruptionBucket paginates objects and aborts in-flight MPUs before
// deleting; a single ListObjectsV2 page caps at 1000 so a paginator is required.
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

// uniqueBucket produces a Localstack-safe name: lowercase, 3–63 chars, no underscores.
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

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

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

const corruptionStoreID = "store-corruption-test"

// writeManifestRaw bypasses the Destination interface to overwrite manifest.yaml.
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

// writePayloadRaw bypasses the Destination interface to overwrite payload.bin.
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

// deleteManifestRaw bypasses the Destination interface to remove manifest.yaml.
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

type corruptionCase struct {
	name              string
	setup             func(t *testing.T, isS3 bool, root, bucket, id string, m *manifest.Manifest)
	checkManifestOnly bool
	wantErr           error
	// wantStoreID asserts GetManifestOnly returns this StoreID (ownership check belongs to the restore executor, not the destination).
	wantStoreID string
	// wantErrContains asserts err.Error() contains this substring (used where no typed sentinel is exposed).
	wantErrContains string
}

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

	// GetBackup path: SHA256 mismatch surfaces on Close, not Read.
	if tc.wantErr == nil {
		t.Fatalf("unhandled case: GetBackup path for %q requires wantErr", tc.name)
	}
	_, rc, err := dest.GetBackup(ctx, id)
	require.NoError(t, err)
	_, _ = io.ReadAll(rc)
	closeErr := rc.Close()
	require.ErrorIs(t, closeErr, tc.wantErr)
}

// TestCorruption exercises 5 corruption vectors × 2 drivers (FS + S3).
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
				// Destination layer returns the parsed manifest intact; the restore
				// executor is responsible for emitting ErrStoreIDMismatch.
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

func TestManifestVersionGate_RestoreSentinel(t *testing.T) {
	require.NotNil(t, restore.ErrManifestVersionUnsupported)
	require.Contains(t, restore.ErrManifestVersionUnsupported.Error(), "manifest version")
}
