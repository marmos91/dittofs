//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS3CompatiblePresets exercises the S3-compatible backend preset path
// documented in docs/CONFIGURATION.md against real emulators (MinIO and
// LocalStack). Both are wired exactly as a non-AWS provider would be: a custom
// `endpoint`, which makes the s3 store factory auto-enable path-style
// addressing (force_path_style). A full NFS read/write/overwrite round-trip
// through a share whose remote tier is the emulator proves the preset works
// end-to-end, not just that the config parses.
//
// MinIO is the headline verified provider here (LocalStack is already covered
// by the store matrix); running both confirms the single s3 implementation
// serves distinct S3-compatible backends.
func TestS3CompatiblePresets(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping S3-compatible preset tests in short mode")
	}

	emulators := []struct {
		name      string
		available func(*testing.T) bool
		helper    func(*testing.T) *framework.LocalstackHelper
	}{
		{"minio", framework.CheckMinioAvailable, framework.NewMinioHelper},
		{"localstack", framework.CheckLocalstackAvailable, framework.NewLocalstackHelper},
	}

	for _, em := range emulators {
		t.Run(em.name, func(t *testing.T) {
			if !em.available(t) {
				t.Skipf("Skipping: %s emulator not available", em.name)
			}
			runS3CompatiblePresetTest(t, em.helper(t))
		})
	}
}

func runS3CompatiblePresetTest(t *testing.T, s3Helper *framework.LocalstackHelper) {
	t.Helper()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	shareName := "/export-s3c"
	helpers.SetupS3CompatibleShare(t, runner, shareName, s3Helper)

	nfsPort := helpers.FindFreePort(t)
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

	require.NoError(t,
		helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second),
		"NFS adapter should become enabled")

	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount := mountNFSExport(t, nfsPort, shareName)
	t.Cleanup(mount.Cleanup)

	// Write, read back, overwrite, read back — covers the create + remote-sync
	// path through the S3-compatible backend.
	content := []byte("DittoFS over an S3-compatible backend.")
	file := mount.FilePath("s3c.txt")
	framework.WriteFile(t, file, content)
	t.Cleanup(func() { _ = os.Remove(file) })

	assert.True(t, framework.FileExists(file), "File should exist after creation")
	assert.True(t, bytes.Equal(content, framework.ReadFile(t, file)),
		"Read content should match written content")

	updated := []byte("Overwritten via S3-compatible remote.")
	framework.WriteFile(t, file, updated)
	assert.True(t, bytes.Equal(updated, framework.ReadFile(t, file)),
		"Overwritten content should match")
}
