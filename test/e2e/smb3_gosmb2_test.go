//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSMB3_GoSMB2_BasicFileOps validates basic file CRUD operations via go-smb2.
// Covers: write, read, stat, rename, delete.
func TestSMB3_GoSMB2_BasicFileOps(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 tests in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	t.Run("WriteAndReadFile", func(t *testing.T) {
		testContent := []byte("Hello, SMB3 via go-smb2!")

		err := share.WriteFile("basic_test.txt", testContent, 0644)
		require.NoError(t, err, "Should write file")

		readContent, err := share.ReadFile("basic_test.txt")
		require.NoError(t, err, "Should read file back")
		assert.Equal(t, testContent, readContent, "Read content should match written content")
	})

	t.Run("StatFile", func(t *testing.T) {
		testContent := []byte("stat test content 12345")

		err := share.WriteFile("stat_test.txt", testContent, 0644)
		require.NoError(t, err, "Should write file for stat")

		info, err := share.Stat("stat_test.txt")
		require.NoError(t, err, "Should stat file")
		assert.Equal(t, int64(len(testContent)), info.Size(), "File size should match content length")
		assert.False(t, info.IsDir(), "Should not be a directory")
	})

	t.Run("RenameFile", func(t *testing.T) {
		originalContent := []byte("rename me")

		err := share.WriteFile("rename_src.txt", originalContent, 0644)
		require.NoError(t, err, "Should write source file")

		err = share.Rename("rename_src.txt", "rename_dst.txt")
		require.NoError(t, err, "Should rename file")

		// Verify renamed file is readable
		readContent, err := share.ReadFile("rename_dst.txt")
		require.NoError(t, err, "Should read renamed file")
		assert.Equal(t, originalContent, readContent, "Renamed file content should match")

		// Verify original is gone
		_, err = share.Stat("rename_src.txt")
		assert.Error(t, err, "Original file should not exist after rename")
	})

	t.Run("DeleteFile", func(t *testing.T) {
		err := share.WriteFile("delete_me.txt", []byte("delete this"), 0644)
		require.NoError(t, err, "Should write file to delete")

		err = share.Remove("delete_me.txt")
		require.NoError(t, err, "Should delete file")

		_, err = share.Stat("delete_me.txt")
		assert.Error(t, err, "Deleted file should not exist")
	})

}

// TestSMB3_GoSMB2_DirectoryOps validates directory operations via go-smb2.
// Covers: mkdir, readdir, rmdir.
func TestSMB3_GoSMB2_DirectoryOps(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 directory tests in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	t.Run("CreateAndListDirectory", func(t *testing.T) {
		err := share.Mkdir("test_dir", 0755)
		require.NoError(t, err, "Should create directory")

		// Create files inside
		for i := range 3 {
			name := fmt.Sprintf("test_dir/file_%d.txt", i)
			err = share.WriteFile(name, []byte(fmt.Sprintf("content %d", i)), 0644)
			require.NoError(t, err, "Should write file %d in directory", i)
		}

		// Read directory entries
		entries, err := share.ReadDir("test_dir")
		require.NoError(t, err, "Should read directory")
		assert.Len(t, entries, 3, "Directory should contain 3 files")

		// Verify file names are present
		names := make(map[string]bool)
		for _, entry := range entries {
			names[entry.Name()] = true
		}
		for i := range 3 {
			expected := fmt.Sprintf("file_%d.txt", i)
			assert.True(t, names[expected], "Directory should contain %s", expected)
		}
	})

	t.Run("RemoveDirectory", func(t *testing.T) {
		err := share.Mkdir("empty_dir", 0755)
		require.NoError(t, err, "Should create empty directory")

		// Verify it exists
		info, err := share.Stat("empty_dir")
		require.NoError(t, err, "Should stat directory")
		assert.True(t, info.IsDir(), "Should be a directory")

		// Remove empty directory
		err = share.Remove("empty_dir")
		require.NoError(t, err, "Should remove empty directory")

		// Verify it's gone
		_, err = share.Stat("empty_dir")
		assert.Error(t, err, "Removed directory should not exist")
	})

}

// TestSMB3_GoSMB2_LargeFile validates writing and reading a 1MB file via go-smb2.
// Tests the data path under SMB3 with significant data volume.
func TestSMB3_GoSMB2_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 large file test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	// Generate 1MB of random data
	const fileSize = 1024 * 1024
	data := make([]byte, fileSize)
	_, err := rand.Read(data)
	require.NoError(t, err, "Should generate random data")

	// Write the large file
	err = share.WriteFile("large_file_1mb.bin", data, 0644)
	require.NoError(t, err, "Should write 1MB file")

	// Read it back
	readData, err := share.ReadFile("large_file_1mb.bin")
	require.NoError(t, err, "Should read 1MB file back")

	// Verify exact byte match
	assert.Equal(t, len(data), len(readData), "File size should match")
	assert.True(t, bytes.Equal(data, readData), "File content should match exactly")

	// Verify via stat
	info, err := share.Stat("large_file_1mb.bin")
	require.NoError(t, err, "Should stat large file")
	assert.Equal(t, int64(fileSize), info.Size(), "File size should be 1MB")

}

// TestSMB3_GoSMB2_SessionSetup validates NTLM session setup with valid and invalid credentials.
func TestSMB3_GoSMB2_SessionSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 session setup test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	t.Run("ValidCredentials", func(t *testing.T) {
		session, err := helpers.ConnectSMB3WithError(t, env.SMBPort, env.Username, env.Password)
		require.NoError(t, err, "Should connect with valid credentials")
		require.NotNil(t, session, "Session should not be nil")
	})

	t.Run("InvalidUsername", func(t *testing.T) {
		_, err := helpers.ConnectSMB3WithError(t, env.SMBPort, "nonexistent_user", env.Password)
		assert.Error(t, err, "Should fail with invalid username")
	})

	t.Run("WrongPassword", func(t *testing.T) {
		_, err := helpers.ConnectSMB3WithError(t, env.SMBPort, env.Username, "wrong_password_123")
		assert.Error(t, err, "Should fail with wrong password")
	})

	t.Run("EmptyCredentials", func(t *testing.T) {
		_, err := helpers.ConnectSMB3WithError(t, env.SMBPort, "", "")
		assert.Error(t, err, "Should fail with empty credentials")
	})

}

// TestSMB3_GoSMB2_Encryption validates that file operations succeed when encryption
// is expected to be negotiated. go-smb2 handles encryption transparently.
// This test verifies that the encrypted data path works end-to-end.
func TestSMB3_GoSMB2_Encryption(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 encryption test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	// Connect and perform operations; go-smb2 negotiates encryption transparently
	// if the server supports it. We verify data integrity through the encrypted path.
	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	// Write data through (potentially encrypted) connection
	testData := []byte("Encryption test: this data traverses the encrypted SMB3 channel")
	err := share.WriteFile("encryption_test.txt", testData, 0644)
	require.NoError(t, err, "Should write file through encrypted connection")

	// Read back and verify
	readData, err := share.ReadFile("encryption_test.txt")
	require.NoError(t, err, "Should read file through encrypted connection")
	assert.Equal(t, testData, readData, "Data should survive encryption round-trip")

	// Verify with a larger payload to exercise encryption with multiple blocks
	largeData := make([]byte, 64*1024) // 64KB
	_, err = rand.Read(largeData)
	require.NoError(t, err, "Should generate random data")

	err = share.WriteFile("encryption_large_test.bin", largeData, 0644)
	require.NoError(t, err, "Should write large file through encrypted connection")

	readLarge, err := share.ReadFile("encryption_large_test.bin")
	require.NoError(t, err, "Should read large file through encrypted connection")
	assert.True(t, bytes.Equal(largeData, readLarge), "Large file data should survive encryption round-trip")

}

// TestSMB3_GoSMB2_Signing validates that file operations succeed with signing.
// go-smb2 handles signing transparently. This test verifies data integrity
// through the signed connection path.
func TestSMB3_GoSMB2_Signing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 signing test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	// Connect; go-smb2 negotiates signing per the server's requirements
	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	// Write data through signed connection
	testData := []byte("Signing test: message integrity verified via SMB3 signing")
	err := share.WriteFile("signing_test.txt", testData, 0644)
	require.NoError(t, err, "Should write file through signed connection")

	// Read back and verify integrity
	readData, err := share.ReadFile("signing_test.txt")
	require.NoError(t, err, "Should read file through signed connection")
	assert.Equal(t, testData, readData, "Data should survive signing round-trip")

	// Multiple sequential operations to test signing state continuity
	for i := range 5 {
		name := fmt.Sprintf("signing_seq_%d.txt", i)
		content := []byte(fmt.Sprintf("signed message %d", i))

		err = share.WriteFile(name, content, 0644)
		require.NoError(t, err, "Should write signed file %d", i)

		read, err := share.ReadFile(name)
		require.NoError(t, err, "Should read signed file %d", i)
		assert.Equal(t, content, read, "Signed file %d content should match", i)
	}

}

// TestSMB3_GoSMB2_MultipleFiles creates 50 files, lists them, and verifies all are present.
// Tests directory enumeration under load.
func TestSMB3_GoSMB2_MultipleFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB3 go-smb2 multiple files test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)

	session := helpers.ConnectSMB3(t, env.SMBPort, env.Username, env.Password)
	share := helpers.MountSMB3Share(t, session, env.ShareName)

	const fileCount = 50

	// Create a directory for the test
	err := share.Mkdir("multi_files", 0755)
	require.NoError(t, err, "Should create directory for multiple files")

	// Create 50 files with unique names and content
	expectedContent := make(map[string][]byte)
	for i := range fileCount {
		name := fmt.Sprintf("multi_files/file_%03d.txt", i)
		content := []byte(fmt.Sprintf("File content for item %d - unique identifier: %d", i, i*31337))
		expectedContent[fmt.Sprintf("file_%03d.txt", i)] = content

		err := share.WriteFile(name, content, 0644)
		require.NoError(t, err, "Should write file %d of %d", i+1, fileCount)
	}

	// List directory and verify all files are present
	entries, err := share.ReadDir("multi_files")
	require.NoError(t, err, "Should read directory with %d files", fileCount)
	assert.Len(t, entries, fileCount, "Directory should contain exactly %d files", fileCount)

	// Verify all expected files are in the listing
	foundNames := make(map[string]bool)
	for _, entry := range entries {
		foundNames[entry.Name()] = true
	}

	for i := range fileCount {
		expected := fmt.Sprintf("file_%03d.txt", i)
		assert.True(t, foundNames[expected], "Directory listing should contain %s", expected)
	}

	// Read back each file and verify content
	for i := range fileCount {
		name := fmt.Sprintf("multi_files/file_%03d.txt", i)
		expectedKey := fmt.Sprintf("file_%03d.txt", i)

		readContent, err := share.ReadFile(name)
		require.NoError(t, err, "Should read file %d back", i)
		assert.Equal(t, expectedContent[expectedKey], readContent,
			"File %d content should match", i)
	}

}
