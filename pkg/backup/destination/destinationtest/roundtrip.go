// Package destinationtest is a cross-driver conformance suite for
// destination.Destination. Call Run with a driver-specific Factory to
// exercise every behavioral invariant a conforming driver must satisfy:
// byte-identical round-trip (cleartext and encrypted), multipart-boundary
// payloads, duplicate-id rejection, chronological List ordering, Delete
// atomicity, missing-backup sentinels, and PayloadIDSet preservation.
//
// Drivers that pass this suite satisfy DRV-01, DRV-02, DRV-03, DRV-04.
//
// SHA-256 tamper tests are deliberately skipped here — they require
// storage-layer access the suite does not have. Each driver's own test
// files (pkg/backup/destination/fs/store_test.go and
// pkg/backup/destination/s3/store_integration_test.go) cover the tamper
// cases by mutating payload.bin directly.
package destinationtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
)

// Factory constructs a destination.Destination for one subtest. The
// factory is responsible for every resource (tmp dir, S3 bucket,
// encryption env var) and for registering t.Cleanup hooks to release
// them. A fresh Destination is requested for each subtest so state from
// one case cannot leak into another.
//
// encryptionRef is an opaque key-reference string the factory promises
// to have resolved at the moment the driver is constructed. Pass the
// empty string for unencrypted subtests. When non-empty, the suite's
// setupKey helper has already exported the env var the ref points at,
// so the driver's ResolveKey call will succeed.
type Factory func(t *testing.T, encryptionRef string) destination.Destination

// Run executes the full conformance suite against f. Every subtest
// invokes f exactly once so each case runs with a fresh Destination and
// isolated backing store.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Roundtrip_Unencrypted", func(t *testing.T) { testRoundtrip(t, f, false) })
	t.Run("Roundtrip_Encrypted", func(t *testing.T) { testRoundtrip(t, f, true) })
	t.Run("Roundtrip_Multipart_Sized", func(t *testing.T) { testRoundtripLargeMultipart(t, f) })
	t.Run("SHA256_Mismatch_On_Close", func(t *testing.T) { testSHA256MismatchOnClose(t, f) })
	t.Run("Duplicate_Rejected", func(t *testing.T) { testDuplicateRejected(t, f) })
	t.Run("List_Chronological", func(t *testing.T) { testListChronological(t, f) })
	t.Run("Delete_InverseOrder", func(t *testing.T) { testDeleteInverseOrder(t, f) })
	t.Run("Missing_Backup", func(t *testing.T) { testMissingBackup(t, f) })
	t.Run("PayloadIDSet_Preserved", func(t *testing.T) { testPayloadIDSetPreserved(t, f) })
}

// testKeyHex is fixed test key material: 64 hex characters = 32 decoded
// bytes, satisfying the AES-256 key contract in keyref.go. The value is
// a known constant for deterministic encrypted round-trip tests.
const testKeyHex = "abababababababababababababababababababababababababababababababab"

// testKeyEnvVar is the env var the suite exports during encrypted
// subtests. Scoped via t.Setenv so concurrent tests in different
// packages cannot observe it.
const testKeyEnvVar = "DITTOFS_DESTTEST_KEY"

// setupKey conditionally exports the test key env var and returns the
// key-ref the driver should record in manifests. Empty-string return
// means "no encryption" — factories skip setting EncryptionEnabled.
func setupKey(t *testing.T, encrypted bool) string {
	t.Helper()
	if !encrypted {
		return ""
	}
	t.Setenv(testKeyEnvVar, testKeyHex)
	return "env:" + testKeyEnvVar
}

// randBytes returns n cryptographically-random bytes. Random (not
// zeros) so compression-like encoders that might silently drop runs
// still produce distinguishable round-trips.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// newManifest assembles a minimum-viable pre-write manifest. The driver
// fills in SHA256 and SizeBytes at PutBackup time; the caller is
// responsible for BackupID, StoreID, StoreKind, PayloadIDSet, and the
// Encryption block.
func newManifest(id string, encrypted bool, keyRef string, payloadIDs []string) *manifest.Manifest {
	if payloadIDs == nil {
		payloadIDs = []string{}
	}
	return &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        id,
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
		StoreID:         "store-conformance",
		StoreKind:       "memory",
		Encryption: manifest.Encryption{
			Enabled:   encrypted,
			Algorithm: "aes-256-gcm",
			KeyRef:    keyRef,
		},
		PayloadIDSet: payloadIDs,
	}
}

// testRoundtrip is the canonical byte-identical round-trip gate.
//
// Size assertion is a single if/else on `encrypted`. No sentinel-returning
// helper. Unencrypted payload is stored verbatim so SizeBytes == len(payload).
// Encrypted payload carries the D-05 envelope header (9 bytes) plus per-frame
// nonce (12 B) + ct_len (4 B) + GCM tag (16 B); the exact overhead depends on
// frame count so the assertion is the direction-only "greater than", which is
// the sole form that is both correct and portable across drivers.
func testRoundtrip(t *testing.T, f Factory, encrypted bool) {
	keyRef := setupKey(t, encrypted)
	s := f(t, keyRef)
	id := ulid.Make().String()
	// 256 KiB — well below the 5 MiB S3 multipart threshold so this test
	// exercises the single-part code path on both drivers.
	payload := randBytes(t, 256*1024)
	m := newManifest(id, encrypted, keyRef, []string{"pid-1", "pid-2"})

	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))
	require.NotEmpty(t, m.SHA256, "PutBackup must populate m.SHA256 before returning")

	if encrypted {
		require.Greater(t, m.SizeBytes, int64(len(payload)),
			"encrypted SizeBytes (%d) must exceed plaintext len (%d) — envelope overhead is non-zero",
			m.SizeBytes, len(payload))
	} else {
		require.Equal(t, int64(len(payload)), m.SizeBytes,
			"unencrypted SizeBytes must equal plaintext length")
	}

	got, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	// Close is where SHA-256 mismatch would surface; a clean read-back
	// returns nil. Phase 5 restore MUST always Close() the reader.
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out, "round-trip plaintext must be byte-identical")
	require.Equal(t, m.SHA256, got.SHA256, "manifest round-trip must preserve SHA256")
	require.ElementsMatch(t, []string{"pid-1", "pid-2"}, got.PayloadIDSet)
}

// testRoundtripLargeMultipart exercises the 5 MiB S3 multipart boundary.
// The fs driver treats any size identically; the S3 driver switches from
// single PutObject to manager.Uploader multipart at 5 MiB. Running at
// 7 MiB forces at least two parts and verifies the encrypt+hash pipe
// survives the boundary.
func testRoundtripLargeMultipart(t *testing.T, f Factory) {
	keyRef := setupKey(t, true)
	s := f(t, keyRef)
	id := ulid.Make().String()
	payload := randBytes(t, 7*1024*1024)
	m := newManifest(id, true, keyRef, []string{"pid-big"})
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out, "multipart-boundary round-trip must be byte-identical")
}

// testSHA256MismatchOnClose documents why the tamper case is intentionally
// excluded from the cross-driver suite: no driver-agnostic tamper exists
// without reaching into the storage layer (filesystem path, S3 key).
// Per-driver test files cover this with direct os.WriteFile / S3 PutObject
// mutation.
func testSHA256MismatchOnClose(t *testing.T, f Factory) {
	t.Skip("cross-driver suite cannot tamper without storage-layer access; see per-driver tests")
}

// testDuplicateRejected enforces that a second PutBackup with the same
// BackupID at the same destination is rejected with ErrDuplicateBackupID.
// This protects against orchestrator bugs that retry a completed backup.
func testDuplicateRejected(t *testing.T, f Factory) {
	s := f(t, "")
	id := ulid.Make().String()
	m1 := newManifest(id, false, "", nil)
	require.NoError(t, s.PutBackup(context.Background(), m1, bytes.NewReader([]byte("a"))))
	m2 := newManifest(id, false, "", nil)
	err := s.PutBackup(context.Background(), m2, bytes.NewReader([]byte("b")))
	require.ErrorIs(t, err, destination.ErrDuplicateBackupID)
}

// testListChronological writes four backups spaced by more than 1 ms and
// confirms List returns them in ULID-lexicographic order (which matches
// chronological order by construction). HasManifest must be true on every
// returned entry — List never surfaces orphans.
func testListChronological(t *testing.T, f Factory) {
	s := f(t, "")
	var ids []string
	for i := 0; i < 4; i++ {
		id := ulid.Make().String()
		ids = append(ids, id)
		require.NoError(t, s.PutBackup(context.Background(), newManifest(id, false, "", nil), bytes.NewReader([]byte{byte(i)})))
		// ULID ms-prefix granularity — a 2 ms gap guarantees sorted
		// lexicographic order matches call order.
		time.Sleep(2 * time.Millisecond)
	}
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 4)
	for i, d := range list {
		require.Equal(t, ids[i], d.ID, "ULID lexicographic order must equal chronological order")
		require.True(t, d.HasManifest, "List entries must have manifest present")
	}
}

// testDeleteInverseOrder writes a backup, deletes it, and confirms List
// no longer returns it. The driver contract says Delete removes manifest
// first (inverse of publish) so List exclusion is immediate — that
// guarantees a crash mid-delete leaves the payload discoverable rather
// than half-gone.
func testDeleteInverseOrder(t *testing.T, f Factory) {
	s := f(t, "")
	id := ulid.Make().String()
	require.NoError(t, s.PutBackup(context.Background(), newManifest(id, false, "", nil), bytes.NewReader([]byte("x"))))
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, id, list[0].ID)

	require.NoError(t, s.Delete(context.Background(), id))
	list, err = s.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, list, "Delete must remove the backup from List")
}

// testMissingBackup requests a non-existent ULID and confirms the driver
// returns ErrManifestMissing (the common case) or ErrIncompleteBackup
// (drivers that distinguish "prefix exists, manifest missing"). Either
// sentinel is acceptable — they are documented alternatives in the
// Destination interface comment.
func testMissingBackup(t *testing.T, f Factory) {
	s := f(t, "")
	_, _, err := s.GetBackup(context.Background(), "01JFAKEFAKEFAKEFAKEFAKEFAKE")
	require.Error(t, err)
	if !errors.Is(err, destination.ErrManifestMissing) &&
		!errors.Is(err, destination.ErrIncompleteBackup) {
		t.Fatalf("expected ErrManifestMissing or ErrIncompleteBackup, got %v", err)
	}
}

// testPayloadIDSetPreserved writes a manifest with a populated
// PayloadIDSet and confirms GetBackup returns the same slice. This is
// the Phase 5 block-GC-hold invariant: the manifest round-trip must
// preserve the exact block list or GC could delete backed-up blocks.
func testPayloadIDSetPreserved(t *testing.T, f Factory) {
	s := f(t, "")
	id := ulid.Make().String()
	ids := []string{"alpha", "beta", "gamma"}
	require.NoError(t, s.PutBackup(context.Background(), newManifest(id, false, "", ids), bytes.NewReader([]byte("p"))))
	m, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	_, _ = io.ReadAll(rc)
	_ = rc.Close()
	require.ElementsMatch(t, ids, m.PayloadIDSet)
}
