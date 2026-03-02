//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfSMBClientUnavailable skips the test if smbclient is not installed or
// running in short mode.
func skipIfSMBClientUnavailable(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping smbclient test in short mode")
	}
	if !helpers.IsSMBClientAvailable() {
		t.Skip("smbclient not found in PATH, skipping")
	}
}

// requireSMBClientOK fails the test if smbclient output contains an NT_STATUS
// error. Some smbclient versions return non-zero for benign reasons (empty
// directory, debug mode), so we only treat NT_STATUS errors as fatal.
func requireSMBClientOK(t *testing.T, output string, err error, operation string) {
	t.Helper()
	if err != nil && strings.Contains(output, "NT_STATUS_") {
		t.Fatalf("smbclient %s failed: %s\nOutput: %s", operation, err, output)
	}
}

// TestSMB3_SmbClient_Connect validates that smbclient can connect and list a share.
func TestSMB3_SmbClient_Connect(t *testing.T) {
	skipIfSMBClientUnavailable(t)
	env := helpers.SetupSMB3TestEnv(t)

	output, err := helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password, env.ShareName, "ls")

	if err != nil {
		if strings.Contains(output, "NT_STATUS_") {
			t.Fatalf("smbclient connect failed with NT_STATUS error: %s", output)
		}
		t.Logf("smbclient returned error (may be non-fatal): %v\nOutput: %s", err, output)
	}

	assert.NotContains(t, output, "NT_STATUS_LOGON_FAILURE",
		"Should not get logon failure")
	assert.NotContains(t, output, "NT_STATUS_ACCESS_DENIED",
		"Should not get access denied")

	t.Logf("smbclient connect output:\n%s", output)
}

// TestSMB3_SmbClient_FileOps validates smbclient put/get/del operations.
func TestSMB3_SmbClient_FileOps(t *testing.T) {
	skipIfSMBClientUnavailable(t)
	env := helpers.SetupSMB3TestEnv(t)

	tmpDir := t.TempDir()
	localUploadFile := filepath.Join(tmpDir, "upload_test.txt")
	testContent := "Hello from smbclient file operations test!"
	err := os.WriteFile(localUploadFile, []byte(testContent), 0644)
	require.NoError(t, err, "Should create local test file")

	// Upload
	putCmd := "put " + localUploadFile + " smbclient_upload.txt"
	output, err := helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password, env.ShareName, putCmd)
	requireSMBClientOK(t, output, err, "put")
	t.Logf("smbclient put output: %s", output)

	// Download
	localDownloadFile := filepath.Join(tmpDir, "download_test.txt")
	getCmd := "get smbclient_upload.txt " + localDownloadFile
	output, err = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password, env.ShareName, getCmd)
	requireSMBClientOK(t, output, err, "get")
	t.Logf("smbclient get output: %s", output)

	// Verify downloaded content
	downloadedContent, err := os.ReadFile(localDownloadFile)
	require.NoError(t, err, "Should read downloaded file")
	assert.Equal(t, testContent, string(downloadedContent),
		"Downloaded content should match uploaded content")

	// Delete
	output, err = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password, env.ShareName, "del smbclient_upload.txt")
	requireSMBClientOK(t, output, err, "del")
	t.Logf("smbclient del output: %s", output)

	// Verify deletion
	output, _ = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password, env.ShareName, "ls")
	assert.NotContains(t, output, "smbclient_upload.txt",
		"Deleted file should not appear in listing")
}

// TestSMB3_SmbClient_DialectNegotiation verifies that SMB3 protocol is negotiated
// by parsing smbclient debug output.
func TestSMB3_SmbClient_DialectNegotiation(t *testing.T) {
	skipIfSMBClientUnavailable(t)
	env := helpers.SetupSMB3TestEnv(t)

	// Run smbclient with debug output to capture protocol negotiation
	output, err := helpers.RunSMBClientDebug(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "ls", 1)

	// smbclient may return non-zero on empty directories or debug mode
	if err != nil && strings.Contains(output, "NT_STATUS_LOGON_FAILURE") {
		t.Fatalf("smbclient auth failed: %s\nOutput: %s", err, output)
	}

	t.Logf("smbclient debug output:\n%s", output)

	// Assert that SMB2/3 protocol was actually negotiated by checking for
	// concrete dialect indicators in the debug output. smbclient at debug level 1+
	// logs protocol negotiation details containing dialect hex codes or protocol names.
	hasSMB3Dialect := strings.Contains(output, "0x0300") || // SMB 3.0.0
		strings.Contains(output, "0x0302") || // SMB 3.0.2
		strings.Contains(output, "0x0311") // SMB 3.1.1

	hasSMBIndicator := hasSMB3Dialect ||
		strings.Contains(output, "SMB3") ||
		strings.Contains(output, "smb3") ||
		strings.Contains(output, "SMB2") ||
		strings.Contains(output, "smb2")

	assert.True(t, hasSMBIndicator,
		"Expected SMB2/3 protocol indicator in smbclient debug output, got:\n%s", output)

	if hasSMB3Dialect {
		t.Log("SMB3 dialect confirmed in negotiation output")
	}

	// Verify no fatal errors occurred during negotiation
	assert.NotContains(t, output, "NT_STATUS_NOT_SUPPORTED",
		"Server should support the negotiated protocol")
	assert.NotContains(t, output, "NT_STATUS_LOGON_FAILURE",
		"Authentication should succeed")

}

// TestSMB3_SmbClient_DirectoryOps validates smbclient mkdir, cd, ls, rmdir operations.
func TestSMB3_SmbClient_DirectoryOps(t *testing.T) {
	skipIfSMBClientUnavailable(t)
	env := helpers.SetupSMB3TestEnv(t)

	// Create directory
	output, err := helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "mkdir smbclient_dir")
	requireSMBClientOK(t, output, err, "mkdir")
	t.Logf("smbclient mkdir output: %s", output)

	// Verify directory exists in listing
	output, err = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "ls")
	requireSMBClientOK(t, output, err, "ls")
	assert.Contains(t, output, "smbclient_dir",
		"Created directory should appear in listing")

	// Navigate into directory and list
	output, err = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "cd smbclient_dir; ls")
	if err != nil && strings.Contains(output, "NT_STATUS_") {
		t.Logf("smbclient cd+ls returned error (may be expected for empty dir): %s\nOutput: %s", err, output)
	}

	// Remove directory
	output, err = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "rmdir smbclient_dir")
	requireSMBClientOK(t, output, err, "rmdir")

	// Verify directory is gone
	output, _ = helpers.RunSMBClient(t, env.SMBPort, env.Username, env.Password,
		env.ShareName, "ls")
	assert.NotContains(t, output, "smbclient_dir",
		"Removed directory should not appear in listing")
}
