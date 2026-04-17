//go:build integration

// Integration tests for the S3 destination driver. Run with:
//
//	go test -tags=integration ./pkg/backup/destination/s3/... -count=1
//
// Uses the SHARED Localstack container pattern (MEMORY.md: per-test
// containers are forbidden). Set LOCALSTACK_ENDPOINT to reuse an external
// Localstack instance instead of spinning one up.

// These tests live in package s3 (not s3_test) so they can share the
// unexported sharedHelper + WithClock test seam. The driver itself is
// exercised via its public API — New / PutBackup / GetBackup / etc. —
// so the in-package placement does not expand the tested surface.
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3client "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// uniqueBucket generates a Localstack-safe bucket name per test. Bucket
// names must be lowercase, 3..63 chars, no underscores.
func uniqueBucket(t *testing.T) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	// Both '/' (subtest separator) and '_' (common in Go test names) are
	// invalid characters in S3 bucket names — replace with '-'.
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	bucket := "bkp-" + name + "-" + strings.ToLower(ulid.Make().String()[:8])
	// S3 max bucket name is 63 chars; trim to be safe.
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	return bucket
}

// newIntegrationStore is the common setup: create the bucket, register
// teardown, build a BackupRepo pointing at Localstack, construct the
// store via New.
func newIntegrationStore(t *testing.T, bucket, prefix string, encryptionOn bool, keyRef string) destination.Destination {
	t.Helper()
	sharedHelper.createBucket(t, bucket)
	t.Cleanup(func() { sharedHelper.deleteBucket(t, bucket) })

	repo := &models.BackupRepo{
		ID:                ulid.Make().String(),
		Kind:              models.BackupRepoKindS3,
		EncryptionEnabled: encryptionOn,
		EncryptionKeyRef:  keyRef,
	}
	require.NoError(t, repo.SetConfig(map[string]any{
		"bucket":           bucket,
		"region":           "us-east-1",
		"endpoint":         sharedHelper.endpoint,
		"access_key":       "test",
		"secret_key":       "test",
		"prefix":           prefix,
		"force_path_style": true,
		"max_retries":      3,
		"grace_window":     "24h",
	}))
	s, err := New(context.Background(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// randBytes returns n cryptographically-random bytes; used to build
// payloads distinguishable from any accidental reuse.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// mkManifest builds a minimum-viable pre-write manifest. PayloadIDSet is
// non-nil (empty slice is valid per SAFETY-01).
func mkManifest(id string, encrypted bool, keyRef string) *manifest.Manifest {
	return &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        id,
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
		StoreID:         "store-test",
		StoreKind:       "memory",
		Encryption: manifest.Encryption{
			Enabled:   encrypted,
			Algorithm: "aes-256-gcm",
			KeyRef:    keyRef,
		},
		PayloadIDSet: []string{},
	}
}

// TestIntegration_S3_Roundtrip_Unencrypted — single-part upload + no
// encryption. Asserts byte-identical round-trip.
func TestIntegration_S3_Roundtrip_Unencrypted(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "metadata/", false, "")
	id := ulid.Make().String()
	m := mkManifest(id, false, "")
	payload := randBytes(t, 3*1024*1024) // 3 MiB (under multipartPartSize)
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out)
}

// TestIntegration_S3_Roundtrip_Encrypted — multipart-forcing payload
// size (> part size of 5 MiB) exercises both the streaming pipe and the
// multipart upload path.
func TestIntegration_S3_Roundtrip_Encrypted(t *testing.T) {
	keyHex := strings.Repeat("ab", 32)
	t.Setenv("DITTOFS_S3_TEST_KEY", keyHex)
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "metadata/", true, "env:DITTOFS_S3_TEST_KEY")
	id := ulid.Make().String()
	m := mkManifest(id, true, "env:DITTOFS_S3_TEST_KEY")
	payload := randBytes(t, 7*1024*1024) // 7 MiB forces multipart (part size 5 MiB)
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out)
}

// TestIntegration_S3_TamperedPayload_SHA256Mismatch overwrites the stored
// payload with fresh random bytes; the reader should detect the digest
// mismatch on Close.
func TestIntegration_S3_TamperedPayload_SHA256Mismatch(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", false, "")
	id := ulid.Make().String()
	m := mkManifest(id, false, "")
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(randBytes(t, 4096))))

	// Overwrite payload with garbage of the same size.
	_, err := sharedHelper.client.PutObject(context.Background(), &s3client.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(id + "/payload.bin"),
		Body:   bytes.NewReader(randBytes(t, 4096)),
	})
	require.NoError(t, err)

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	_, _ = io.ReadAll(rc)
	err = rc.Close()
	require.ErrorIs(t, err, destination.ErrSHA256Mismatch)
}

// TestIntegration_S3_MissingManifest_ReturnsManifestMissing — restore
// against an id with no manifest (never written, or half-deleted) must
// return ErrManifestMissing so callers never feed an orphan payload
// into a restore.
func TestIntegration_S3_MissingManifest_ReturnsManifestMissing(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", false, "")
	id := ulid.Make().String()
	_, _, err := s.GetBackup(context.Background(), id)
	require.ErrorIs(t, err, destination.ErrManifestMissing)
}

// TestIntegration_S3_GetManifestOnly_Roundtrip exercises the Phase 5
// D-12 cheap manifest-fetch path against Localstack. Published backup →
// parsed manifest with identifying fields populated; no payload GetObject
// is required so this succeeds even when the payload is deleted
// out-of-band (simulating an operator's `aws s3 rm`).
func TestIntegration_S3_GetManifestOnly_Roundtrip(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", false, "")
	id := ulid.Make().String()
	m := mkManifest(id, false, "")
	m.PayloadIDSet = []string{"pid-a", "pid-b"}
	payload := randBytes(t, 8*1024)
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	got, err := s.GetManifestOnly(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, id, got.BackupID)
	require.Equal(t, m.SHA256, got.SHA256)
	require.ElementsMatch(t, []string{"pid-a", "pid-b"}, got.PayloadIDSet)

	// Drop the payload; GetManifestOnly still succeeds.
	_, err = sharedHelper.client.DeleteObject(context.Background(), &s3client.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(id + "/payload.bin"),
	})
	require.NoError(t, err)

	got2, err := s.GetManifestOnly(context.Background(), id)
	require.NoError(t, err, "GetManifestOnly must not depend on payload.bin presence")
	require.Equal(t, id, got2.BackupID)
}

// TestIntegration_S3_GetManifestOnly_MissingReturnsSentinel asserts the
// D-12 error-shape contract: an unknown id surfaces ErrManifestMissing
// so Phase 5 restore pre-flight and block-GC hold can branch cleanly.
func TestIntegration_S3_GetManifestOnly_MissingReturnsSentinel(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", false, "")
	_, err := s.GetManifestOnly(context.Background(), ulid.Make().String())
	require.Error(t, err)
	require.ErrorIs(t, err, destination.ErrManifestMissing)
}

// TestIntegration_S3_OrphanSweep — linear setup:
//  1. createBucket (with teardown registered)
//  2. PutObject the stale payload (exactly once)
//  3. construct store with short grace window + fast-forward clock so
//     the async sweep deletes it immediately
//  4. poll until the object is gone (10s cap)
func TestIntegration_S3_OrphanSweep(t *testing.T) {
	bucket := uniqueBucket(t)
	sharedHelper.createBucket(t, bucket)
	t.Cleanup(func() { sharedHelper.deleteBucket(t, bucket) })

	staleID := ulid.Make().String()
	_, err := sharedHelper.client.PutObject(context.Background(), &s3client.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(staleID + "/payload.bin"),
		Body:   bytes.NewReader([]byte("orphan")),
	})
	require.NoError(t, err)

	// Construct store with short grace window (1ns) and fast-forward
	// clock so the object is treated as older than cutoff.
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3}
	require.NoError(t, repo.SetConfig(map[string]any{
		"bucket":           bucket,
		"region":           "us-east-1",
		"endpoint":         sharedHelper.endpoint,
		"access_key":       "test",
		"secret_key":       "test",
		"force_path_style": true,
		"grace_window":     "1ns",
	}))
	s, err := New(context.Background(), repo,
		WithClock(func() time.Time { return time.Now().Add(time.Hour) }),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := sharedHelper.client.HeadObject(context.Background(), &s3client.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(staleID + "/payload.bin"),
		})
		if err != nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("orphan was not swept within 10s")
}

// TestIntegration_S3_List_And_Delete verifies sort order (ULIDs are
// lexicographically chronological) and manifest-first deletion semantics
// (List immediately excludes the deleted backup).
func TestIntegration_S3_List_And_Delete(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "metadata/", false, "")
	var ids []string
	for i := 0; i < 3; i++ {
		id := ulid.Make().String()
		ids = append(ids, id)
		require.NoError(t, s.PutBackup(context.Background(), mkManifest(id, false, ""), bytes.NewReader([]byte{byte(i)})))
		// Brief pause so ULIDs are monotonically distinct across 1ms
		// boundary and List ordering is deterministic.
		time.Sleep(2 * time.Millisecond)
	}
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 3)
	for i, d := range list {
		require.Equal(t, ids[i], d.ID)
	}
	require.NoError(t, s.Delete(context.Background(), ids[1]))
	list2, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list2, 2)
}

// TestIntegration_S3_ValidateConfig_HappyPath — bucket exists, no
// collisions; ValidateConfig returns nil.
func TestIntegration_S3_ValidateConfig_HappyPath(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "metadata/", false, "")
	require.NoError(t, s.ValidateConfig(context.Background()))
}

// TestIntegration_S3_ValidateConfig_MissingBucket — Localstack returns a
// distinct 404 shape; ValidateConfig must map it to ErrIncompatibleConfig.
func TestIntegration_S3_ValidateConfig_MissingBucket(t *testing.T) {
	bogus := "nonexistent-bucket-" + strings.ToLower(ulid.Make().String())
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3}
	require.NoError(t, repo.SetConfig(map[string]any{
		"bucket":           bogus,
		"region":           "us-east-1",
		"endpoint":         sharedHelper.endpoint,
		"access_key":       "test",
		"secret_key":       "test",
		"force_path_style": true,
	}))
	s, err := New(context.Background(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	err = s.ValidateConfig(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, destination.ErrIncompatibleConfig)
}

// TestIntegration_S3_Duplicate_Rejected — the second PutBackup for the
// same id must fail with ErrDuplicateBackupID (manifest already present).
func TestIntegration_S3_Duplicate_Rejected(t *testing.T) {
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", false, "")
	id := ulid.Make().String()
	require.NoError(t, s.PutBackup(context.Background(), mkManifest(id, false, ""), bytes.NewReader([]byte("a"))))
	err := s.PutBackup(context.Background(), mkManifest(id, false, ""), bytes.NewReader([]byte("b")))
	require.ErrorIs(t, err, destination.ErrDuplicateBackupID)
}

// TestIntegration_S3_WrongKey_DecryptFails — rewrite the manifest to
// point at a different env var holding a different key. The GCM tag on
// the first frame should mismatch → ErrDecryptFailed surfaces from Read.
func TestIntegration_S3_WrongKey_DecryptFails(t *testing.T) {
	keyHex := strings.Repeat("ab", 32)
	t.Setenv("DITTOFS_S3_TEST_KEY_A", keyHex)
	bucket := uniqueBucket(t)
	s := newIntegrationStore(t, bucket, "", true, "env:DITTOFS_S3_TEST_KEY_A")
	id := ulid.Make().String()
	m := mkManifest(id, true, "env:DITTOFS_S3_TEST_KEY_A")
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(randBytes(t, 4096))))

	// Rewrite the stored manifest to reference a DIFFERENT env var
	// (holding different key bytes). Decrypt must fail loud.
	t.Setenv("DITTOFS_S3_TEST_KEY_B", strings.Repeat("cd", 32))
	m.Encryption.KeyRef = "env:DITTOFS_S3_TEST_KEY_B"
	// We need the manifest to validate — SHA-256 and SizeBytes are
	// already filled in by the successful PutBackup above; Marshal
	// keeps them. yaml.v3 is deterministic for struct-tagged types.
	data, err := m.Marshal()
	require.NoError(t, err)
	_, err = sharedHelper.client.PutObject(context.Background(), &s3client.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(id + "/manifest.yaml"),
		Body:   bytes.NewReader(data),
	})
	require.NoError(t, err)

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	_, readErr := io.ReadAll(rc)
	require.Error(t, readErr)
	require.ErrorIs(t, readErr, destination.ErrDecryptFailed)
	_ = rc.Close()
}
