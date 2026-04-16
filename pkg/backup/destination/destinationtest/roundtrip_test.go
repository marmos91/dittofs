// Applies the cross-driver conformance suite to the local filesystem
// destination driver. This test exists in the _test package so imports of
// fs/ stay test-only. No network required — runs under `go test ./...`.
package destinationtest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/destination/destinationtest"
	destfs "github.com/marmos91/dittofs/pkg/backup/destination/fs"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestConformance_FSDriver runs the entire destinationtest.Run suite
// against the filesystem driver. Every subtest gets a fresh t.TempDir()
// root so parallel subtests (and leftover state from Put/Delete) cannot
// bleed between cases.
func TestConformance_FSDriver(t *testing.T) {
	destinationtest.Run(t, func(t *testing.T, keyRef string) destination.Destination {
		dir := t.TempDir()
		repo := &models.BackupRepo{
			ID:                "fs-conformance",
			Kind:              models.BackupRepoKindLocal,
			EncryptionEnabled: keyRef != "",
			EncryptionKeyRef:  keyRef,
		}
		require.NoError(t, repo.SetConfig(map[string]any{
			"path":         dir,
			"grace_window": "24h",
		}))
		s, err := destfs.New(context.Background(), repo)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
