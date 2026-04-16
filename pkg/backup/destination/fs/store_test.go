package fs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/destination/fs"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// newTestStore constructs a Store rooted at a t.TempDir() directory with
// a 24h grace window. Cleanup closes the store so any goroutines the
// driver holds (none today; future-proofing) are released.
func newTestStore(t *testing.T) (destination.Destination, string) {
	t.Helper()
	dir := t.TempDir()
	repo := &models.BackupRepo{
		ID:                "repo-test",
		Kind:              models.BackupRepoKindLocal,
		EncryptionEnabled: false,
	}
	require.NoError(t, repo.SetConfig(map[string]any{"path": dir, "grace_window": "24h"}))
	s, err := fs.New(context.Background(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// newTestManifest builds a minimal Manifest with all pre-write required
// fields populated. Tests needing encryption pass the repo-level key_ref
// that the driver will resolve at PutBackup time.
func newTestManifest(id string, encrypted bool, keyRef string) *manifest.Manifest {
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
		// Empty non-nil — required by the manifest schema (SAFETY-01).
		PayloadIDSet: []string{},
	}
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// TestFSStore_PutGet_Unencrypted_Roundtrip exercises the most-traveled
// path: write bytes, read them back, confirm they match and that both
// manifest fields the driver owns (SHA256, SizeBytes) got populated.
func TestFSStore_PutGet_Unencrypted_Roundtrip(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	m := newTestManifest(id, false, "")
	payload := randBytes(t, 64*1024)
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	_, err := os.Stat(filepath.Join(root, id, "payload.bin"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, id, "manifest.yaml"))
	require.NoError(t, err)

	require.NotEmpty(t, m.SHA256)
	require.Equal(t, int64(len(payload)), m.SizeBytes)

	got, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	// Close is where SHA-256 mismatch would surface; verify it returns
	// nil on a clean read-back.
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out)
	require.Equal(t, m.SHA256, got.SHA256)
}

// TestFSStore_PutGet_Encrypted_Roundtrip drives the full encrypt → tee →
// disk → verify → decrypt pipeline. Uses env:DITTOFS_FS_TEST_KEY so
// resolveEnvKey's hex-decode path is exercised (not the file-mode path —
// that's covered by keyref_test.go in the destination package).
func TestFSStore_PutGet_Encrypted_Roundtrip(t *testing.T) {
	// 64-char hex = 32 raw bytes = AES-256 key.
	keyHex := strings.Repeat("ab", 32)
	t.Setenv("DITTOFS_FS_TEST_KEY", keyHex)

	s, _ := newTestStore(t)
	id := ulid.Make().String()
	m := newTestManifest(id, true, "env:DITTOFS_FS_TEST_KEY")
	payload := randBytes(t, 1<<20) // 1 MiB — spans multiple encrypt frames
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader(payload)))

	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	out, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, payload, out)

	// SHA-256 is over the CIPHERTEXT (D-04) — validate it's a well-formed
	// 32-byte digest rather than comparing to the plaintext hash.
	dec, err := hex.DecodeString(m.SHA256)
	require.NoError(t, err)
	require.Len(t, dec, 32)
}

// TestFSStore_Perms_0600_0700 asserts the D-14 umask-defensive file and
// directory modes stay exactly 0600 / 0700 after publish.
func TestFSStore_Perms_0600_0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not enforced on Windows")
	}
	s, root := newTestStore(t)
	id := ulid.Make().String()
	m := newTestManifest(id, false, "")
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader([]byte("x"))))

	di, err := os.Stat(filepath.Join(root, id))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "dir must be 0700")
	for _, fn := range []string{"payload.bin", "manifest.yaml"} {
		fi, err := os.Stat(filepath.Join(root, id, fn))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), fn+" must be 0600")
	}
}

// TestFSStore_CrashBeforeRename_ListExcludesTmp simulates a process
// crash after file-fsync but before os.Rename by hand-creating the
// <id>.tmp/ subtree. List must never surface it — only the rename is
// the publish marker.
func TestFSStore_CrashBeforeRename_ListExcludesTmp(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	tmpDir := filepath.Join(root, id+".tmp")
	require.NoError(t, os.Mkdir(tmpDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "payload.bin"), []byte("p"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "manifest.yaml"), []byte("manifest_version: 1\n"), 0o600))
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, list, "tmp dirs must never appear in List")
}

// TestFSStore_MutatedPayload_SHA256Mismatch confirms the verify-while-
// streaming reader reports mismatch on Close (not Read). Tampering after
// publish is the canonical DRV-04 failure mode.
func TestFSStore_MutatedPayload_SHA256Mismatch(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	m := newTestManifest(id, false, "")
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader([]byte("original"))))
	// Overwrite payload.bin in place — same length so io.ReadAll sees no
	// short-read, just bad bytes.
	require.NoError(t, os.WriteFile(filepath.Join(root, id, "payload.bin"), []byte("tampered"), 0o600))
	_, rc, err := s.GetBackup(context.Background(), id)
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	require.NoError(t, err, "Read itself does not fail; mismatch surfaces on Close")
	err = rc.Close()
	require.ErrorIs(t, err, destination.ErrSHA256Mismatch)
}

// TestFSStore_MissingManifest_GetReturnsManifestMissing exercises the
// "looks published but manifest.yaml is gone" branch — either a
// half-deleted backup or a filesystem bug. GetBackup must refuse.
func TestFSStore_MissingManifest_GetReturnsManifestMissing(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, id, "payload.bin"), []byte("p"), 0o600))
	_, _, err := s.GetBackup(context.Background(), id)
	require.ErrorIs(t, err, destination.ErrManifestMissing)
}

// TestFSStore_MissingPayload_GetReturnsIncomplete exercises the inverse:
// manifest-present, payload-absent. Shouldn't occur under a healthy
// Delete (which removes manifest first) but a hand-rolled corruption or
// disk failure can produce it.
func TestFSStore_MissingPayload_GetReturnsIncomplete(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0o700))
	m := newTestManifest(id, false, "")
	// Populate SHA256 with the SHA-256 of empty input so Validate would
	// pass if we still called it; we don't, but a plausible value keeps
	// the manifest parseable.
	m.SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	f, err := os.Create(filepath.Join(root, id, "manifest.yaml"))
	require.NoError(t, err)
	_, err = m.WriteTo(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, _, err = s.GetBackup(context.Background(), id)
	require.ErrorIs(t, err, destination.ErrIncompleteBackup)
}

// TestFSStore_Delete_InverseOrder spot-checks that Delete removes the
// whole backup subtree. The inverse-order invariant (manifest-first) is
// validated by source reading; this test guards the happy path.
func TestFSStore_Delete_InverseOrder(t *testing.T) {
	s, root := newTestStore(t)
	id := ulid.Make().String()
	m := newTestManifest(id, false, "")
	require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader([]byte("p"))))
	require.NoError(t, s.Delete(context.Background(), id))
	_, err := os.Stat(filepath.Join(root, id))
	require.True(t, errors.Is(err, os.ErrNotExist))
}

// TestFSStore_OrphanSweep_On_New confirms D-06 init-time sweep removes
// <id>.tmp/ older than the grace window and preserves fresh ones. Uses
// os.Chtimes to age the stale entry deterministically.
func TestFSStore_OrphanSweep_On_New(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "01JABCDEF.tmp")
	require.NoError(t, os.Mkdir(stale, 0o700))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	fresh := filepath.Join(dir, "01JFRESH.tmp")
	require.NoError(t, os.Mkdir(fresh, 0o700))

	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindLocal}
	require.NoError(t, repo.SetConfig(map[string]any{"path": dir, "grace_window": "24h"}))
	_, err := fs.New(context.Background(), repo)
	require.NoError(t, err)

	_, err = os.Stat(stale)
	require.True(t, errors.Is(err, os.ErrNotExist), "stale tmp must be swept")
	_, err = os.Stat(fresh)
	require.NoError(t, err, "fresh tmp must be preserved")
}

// TestFSStore_ValidateConfig drives the happy path + two rejection
// branches (non-existent path, not-a-directory). The remote-FS warning
// path is not asserted here — detectFilesystemType is a best-effort
// platform probe and has no unit-test hook.
func TestFSStore_ValidateConfig(t *testing.T) {
	dir := t.TempDir()
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindLocal}
	require.NoError(t, repo.SetConfig(map[string]any{"path": dir}))
	s, err := fs.New(context.Background(), repo)
	require.NoError(t, err)
	require.NoError(t, s.ValidateConfig(context.Background()))

	// Non-existent path: New must still construct (sweep no-ops on
	// ReadDir failure) but ValidateConfig must reject.
	repo2 := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindLocal}
	require.NoError(t, repo2.SetConfig(map[string]any{"path": "/definitely/does/not/exist/dittofs-test"}))
	s2, _ := fs.New(context.Background(), repo2)
	if s2 != nil {
		vErr := s2.ValidateConfig(context.Background())
		require.ErrorIs(t, vErr, destination.ErrIncompatibleConfig)
	}

	// Not-a-directory path.
	file := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	repo3 := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindLocal}
	require.NoError(t, repo3.SetConfig(map[string]any{"path": file}))
	s3, err3 := fs.New(context.Background(), repo3)
	if err3 == nil && s3 != nil {
		require.ErrorIs(t, s3.ValidateConfig(context.Background()), destination.ErrIncompatibleConfig)
	}
}

// TestFSStore_DuplicateID_Rejected confirms the double-publish guard.
// ULID collisions are vanishingly rare in practice; far more common is
// an orchestrator retrying a completed backup under the same id.
func TestFSStore_DuplicateID_Rejected(t *testing.T) {
	s, _ := newTestStore(t)
	id := ulid.Make().String()
	m1 := newTestManifest(id, false, "")
	require.NoError(t, s.PutBackup(context.Background(), m1, bytes.NewReader([]byte("a"))))
	m2 := newTestManifest(id, false, "")
	err := s.PutBackup(context.Background(), m2, bytes.NewReader([]byte("b")))
	require.ErrorIs(t, err, destination.ErrDuplicateBackupID)
}

// TestFSStore_List_ChronologicalOrder exercises the D-01 "ULID gives
// chronological ls" property through List. ULIDs generated >=1ms apart
// sort lexicographically in creation order.
func TestFSStore_List_ChronologicalOrder(t *testing.T) {
	s, _ := newTestStore(t)
	var ids []string
	for i := 0; i < 3; i++ {
		id := ulid.Make().String()
		ids = append(ids, id)
		m := newTestManifest(id, false, "")
		require.NoError(t, s.PutBackup(context.Background(), m, bytes.NewReader([]byte{byte(i)})))
		// Force the ULID millisecond prefix to advance so we don't race
		// the entropy tiebreaker in Make().
		time.Sleep(2 * time.Millisecond)
	}
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 3)
	for i, d := range list {
		require.Equal(t, ids[i], d.ID, "ULIDs sort chronologically")
	}
}

// TestFSStore_NilPayloadIDSet_Rejected is a regression test for the
// explicit pre-write field check that replaced the full manifest
// Validate method. nil PayloadIDSet would previously have crashed the
// call inside Validate on the SHA256-is-empty branch before reaching
// PayloadIDSet; the explicit field check surfaces it cleanly with
// ErrIncompatibleConfig BEFORE any files are created.
func TestFSStore_NilPayloadIDSet_Rejected(t *testing.T) {
	s, _ := newTestStore(t)
	id := ulid.Make().String()
	m := &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        id,
		CreatedAt:       time.Now().UTC(),
		StoreID:         "store-test",
		StoreKind:       "memory",
		PayloadIDSet:    nil,
	}
	err := s.PutBackup(context.Background(), m, bytes.NewReader([]byte("x")))
	require.Error(t, err)
	require.ErrorIs(t, err, destination.ErrIncompatibleConfig)
}
