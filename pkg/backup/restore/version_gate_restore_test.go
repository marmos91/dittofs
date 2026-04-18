package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination/fs"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRestoreExecutor_RejectsFutureManifestVersion asserts that a forward-incompatible
// manifest is rejected before any destructive action (no engine open, no swap).
func TestRestoreExecutor_RejectsFutureManifestVersion(t *testing.T) {
	js := newFakeJobStore()
	ss := &fakeStores{}

	m := validManifest()
	m.ManifestVersion = manifest.CurrentVersion + 1

	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return m, nil
		},
	}

	e := New(js, fixedClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	_, err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrManifestVersionUnsupported),
		"expected ErrManifestVersionUnsupported in error chain, got %v", err)

	require.Equal(t, 0, ss.openCount(),
		"fresh engine must NOT be opened on manifest-version rejection")
	require.Equal(t, 0, ss.swapCount(),
		"SwapMetadataStore must NOT be called on manifest-version rejection")

	require.Equal(t, models.BackupStatusFailed, js.finalStatus(),
		"manifest-version rejection → job.status=failed (not interrupted)")
}

// TestManifestParse_RejectsFutureManifestVersion covers the on-disk tamper vector:
// a tampered manifest.yaml is rejected by manifest.Parse before the executor sees it.
func TestManifestParse_RejectsFutureManifestVersion(t *testing.T) {
	dir := t.TempDir()

	repo := &models.BackupRepo{
		ID:         "repo-tamper",
		TargetID:   "store-under-test",
		TargetKind: "metadata",
		Name:       "tamper-test",
		Kind:       models.BackupRepoKindLocal,
	}
	require.NoError(t, repo.SetConfig(map[string]any{
		"path":         dir,
		"grace_window": "24h",
	}))

	ctx := context.Background()
	dest, err := fs.New(ctx, repo)
	require.NoError(t, err)

	valid := &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        "01J000000000000000000TAMPER",
		CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		StoreID:         "store-under-test",
		StoreKind:       "memory",
		SizeBytes:       0,
		SHA256:          "",
		PayloadIDSet:    []string{},
	}
	payload := []byte("dummy-payload")
	require.NoError(t, dest.PutBackup(ctx, valid, bytes.NewReader(payload)))

	got, err := dest.GetManifestOnly(ctx, valid.BackupID)
	require.NoError(t, err, "pre-tamper GetManifestOnly must succeed")
	require.Equal(t, manifest.CurrentVersion, got.ManifestVersion)

	preHash := hashDirTree(t, dir)

	got.ManifestVersion = manifest.CurrentVersion + 1
	tampered, err := got.Marshal()
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, valid.BackupID, "manifest.yaml")
	require.NoError(t, os.WriteFile(manifestPath, tampered, 0o600))

	_, perr := manifest.Parse(tampered)
	require.Error(t, perr, "manifest.Parse must reject future ManifestVersion")
	require.Contains(t, perr.Error(), "unsupported manifest_version",
		"parse error must identify the version-gate failure, got %v", perr)

	_, gerr := dest.GetManifestOnly(ctx, valid.BackupID)
	require.Error(t, gerr, "GetManifestOnly must surface parse failure")
	require.Contains(t, gerr.Error(), "unsupported manifest_version",
		"GetManifestOnly error must preserve the parse-gate message, got %v", gerr)

	postHash := hashDirTree(t, dir)
	require.NotEqual(t, preHash, postHash,
		"manifest.yaml must have been rewritten (sanity check on tamper step)")

	postHashSansManifest := hashDirTreeExcluding(t, dir, manifestPath)
	require.NotEmpty(t, postHashSansManifest,
		"payload.bin must still be present after manifest tamper")
}

func hashDirTree(t *testing.T, root string) string {
	t.Helper()
	return hashDirTreeExcluding(t, root, "")
}

func hashDirTreeExcluding(t *testing.T, root, excludePath string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		if excludePath != "" && p == excludePath {
			return nil
		}
		f, oerr := os.Open(p) //nolint:gosec // test-controlled path under t.TempDir()
		if oerr != nil {
			return oerr
		}
		defer func() { _ = f.Close() }()
		_, _ = h.Write([]byte(p))
		_, cerr := io.Copy(h, f)
		return cerr
	})
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}
