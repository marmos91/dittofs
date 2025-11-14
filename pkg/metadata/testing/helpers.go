package testing

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Default Attribute Factories
// ============================================================================

// DefaultRootDirAttr creates default attributes for a share root directory.
// These are sensible defaults for testing.
func DefaultRootDirAttr() *metadata.FileAttr {
	now := time.Now()
	return &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Mode:  0777, // World-writable for testing purposes
		UID:   0,
		GID:   0,
		Size:  4096,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
}

// DefaultFileAttr creates default attributes for a regular file.
func DefaultFileAttr() *metadata.FileAttr {
	now := time.Now()
	return &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Mode:  0644,
		UID:   1000,
		GID:   1000,
		Size:  0,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
}

// DefaultDirAttr creates default attributes for a directory.
func DefaultDirAttr() *metadata.FileAttr {
	now := time.Now()
	return &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Mode:  0755,
		UID:   1000,
		GID:   1000,
		Size:  4096,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
}

// ============================================================================
// Identity and Authentication Context Factories
// ============================================================================

// uint32Ptr returns a pointer to a uint32 value.
// Helper function for creating Identity structs.
func uint32Ptr(v uint32) *uint32 {
	return &v
}

// RootIdentity returns an identity for root user (UID 0).
func RootIdentity() *metadata.Identity {
	return &metadata.Identity{
		UID: uint32Ptr(0),
		GID: uint32Ptr(0),
	}
}

// UserIdentity returns an identity for a regular user.
func UserIdentity(uid, gid uint32) *metadata.Identity {
	return &metadata.Identity{
		UID: uint32Ptr(uid),
		GID: uint32Ptr(gid),
	}
}

// UserIdentityWithGroups returns an identity for a user with supplementary groups.
func UserIdentityWithGroups(uid, gid uint32, groups []uint32) *metadata.Identity {
	return &metadata.Identity{
		UID:  uint32Ptr(uid),
		GID:  uint32Ptr(gid),
		GIDs: groups, // This is []uint32, not []*uint32
	}
}

// AnonymousIdentity returns an anonymous identity (UID/GID = nobody).
func AnonymousIdentity() *metadata.Identity {
	return &metadata.Identity{
		UID: uint32Ptr(65534), // nobody
		GID: uint32Ptr(65534), // nogroup
	}
}

// RootAuthContext returns an AuthContext for root user.
func RootAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: RootIdentity(),
	}
}

// UserAuthContext returns an AuthContext for a regular user.
func UserAuthContext(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Identity: UserIdentity(uid, gid),
	}
}

// UserAuthContextWithGroups returns an AuthContext for a user with supplementary groups.
func UserAuthContextWithGroups(uid, gid uint32, groups []uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Identity: UserIdentityWithGroups(uid, gid, groups),
	}
}

// AnonymousAuthContext returns an AuthContext for anonymous user.
func AnonymousAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Identity: AnonymousIdentity(),
	}
}

// ============================================================================
// Authentication Context Helpers (Share-Aware)
// ============================================================================

// createRootAuthContext creates an authentication context for the root user bound to a share.
func createRootAuthContext(share *metadata.Share) *metadata.AuthContext {
	_ = share // Keep parameter for API consistency even if not used in struct
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity:   RootIdentity(),
		ClientAddr: "127.0.0.1",
	}
}

// createUserAuthContext creates an authentication context for a regular user bound to a share.
func createUserAuthContext(share *metadata.Share, uid, gid uint32) *metadata.AuthContext {
	_ = share // Keep parameter for API consistency even if not used in struct
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity:   UserIdentity(uid, gid),
		ClientAddr: "127.0.0.1",
	}
}

// ============================================================================
// Share Options Factories
// ============================================================================

// DefaultShareOptions returns a basic ShareOptions configuration for testing.
func DefaultShareOptions() metadata.ShareOptions {
	return metadata.ShareOptions{
		ReadOnly:    false,
		Async:       false,
		RequireAuth: false,
	}
}

// ReadOnlyShareOptions returns ShareOptions for a read-only share.
func ReadOnlyShareOptions() metadata.ShareOptions {
	return metadata.ShareOptions{
		ReadOnly:    true,
		Async:       false,
		RequireAuth: false,
	}
}

// SecureShareOptions returns ShareOptions requiring authentication.
func SecureShareOptions() metadata.ShareOptions {
	return metadata.ShareOptions{
		ReadOnly:           false,
		Async:              false,
		RequireAuth:        true,
		AllowedAuthMethods: []string{"unix"},
	}
}

// AnonymousMappingShareOptions returns ShareOptions that map all users to anonymous.
func AnonymousMappingShareOptions() metadata.ShareOptions {
	return metadata.ShareOptions{
		ReadOnly: false,
		Async:    false,
		IdentityMapping: &metadata.IdentityMapping{
			MapAllToAnonymous:        true,
			MapPrivilegedToAnonymous: false,
		},
	}
}

// ============================================================================
// Share Setup Helpers
// ============================================================================

// createTestShare creates a share with default configuration and returns the share and root handle.
// This is the foundation for most tests - it sets up a basic filesystem namespace.
func createTestShare(t *testing.T, store metadata.MetadataStore, name string) (*metadata.Share, metadata.FileHandle) {
	t.Helper()
	ctx := context.Background()

	rootAttr := DefaultRootDirAttr()
	options := DefaultShareOptions()

	err := store.AddShare(ctx, name, options, rootAttr)
	require.NoError(t, err, "Failed to create test share")

	rootHandle, err := store.GetShareRoot(ctx, name)
	require.NoError(t, err, "Failed to get share root")

	share, err := store.FindShare(ctx, name)
	require.NoError(t, err, "Failed to find created share")

	return share, rootHandle
}

// ============================================================================
// File Creation Helpers
// ============================================================================

// createTestFile creates a regular file with default attributes.
func createTestFile(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()

	attr := DefaultFileAttr()
	// Use the auth context's identity for ownership
	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.Create(authCtx, parentHandle, name, attr)
	require.NoError(t, err, "Failed to create test file: %s", name)

	return handle
}

// createTestFileWithMode creates a regular file with a specific permission mode.
func createTestFileWithMode(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, mode uint32) metadata.FileHandle {
	t.Helper()

	attr := DefaultFileAttr()
	attr.Mode = mode
	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.Create(authCtx, parentHandle, name, attr)
	require.NoError(t, err, "Failed to create test file with mode: %s", name)

	return handle
}

// createTestFileWithOwner creates a regular file with specific ownership.
func createTestFileWithOwner(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, uid, gid uint32) metadata.FileHandle {
	t.Helper()

	attr := DefaultFileAttr()
	attr.UID = uid
	attr.GID = gid

	handle, err := store.Create(authCtx, parentHandle, name, attr)
	require.NoError(t, err, "Failed to create test file with owner: %s", name)

	return handle
}

// ============================================================================
// Directory Creation Helpers
// ============================================================================

// createTestDirectory creates a directory with default attributes.
func createTestDirectory(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()

	attr := DefaultDirAttr()
	// Use the auth context's identity for ownership
	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.Create(authCtx, parentHandle, name, attr)
	require.NoError(t, err, "Failed to create test directory: %s", name)

	return handle
}

// createTestDirectoryWithMode creates a directory with specific permissions.
func createTestDirectoryWithMode(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, mode uint32) metadata.FileHandle {
	t.Helper()

	attr := DefaultDirAttr()
	attr.Mode = mode
	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.Create(authCtx, parentHandle, name, attr)
	require.NoError(t, err, "Failed to create test directory with mode: %s", name)

	return handle
}

// ============================================================================
// Symlink Creation Helpers
// ============================================================================

// createTestSymlink creates a symbolic link pointing to the specified target.
func createTestSymlink(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, target string) metadata.FileHandle {
	t.Helper()

	now := time.Now()
	attr := &metadata.FileAttr{
		Mode:  0777,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}

	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.CreateSymlink(authCtx, parentHandle, name, target, attr)
	require.NoError(t, err, "Failed to create test symlink: %s -> %s", name, target)

	return handle
}

// ============================================================================
// Special File Creation Helpers
// ============================================================================

// createTestSpecialFile creates a special file (device, socket, or FIFO).
func createTestSpecialFile(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string, fileType metadata.FileType, major, minor uint32) metadata.FileHandle {
	t.Helper()

	now := time.Now()
	attr := &metadata.FileAttr{
		Mode:  0644,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}

	if authCtx.Identity.UID != nil {
		attr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		attr.GID = *authCtx.Identity.GID
	}

	handle, err := store.CreateSpecialFile(authCtx, parentHandle, name, fileType, attr, major, minor)
	require.NoError(t, err, "Failed to create special file: %s", name)

	return handle
}

// ============================================================================
// Permission Modification Helpers
// ============================================================================

// setFilePermissions changes the permission mode of a file.
func setFilePermissions(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, handle metadata.FileHandle, mode uint32) {
	t.Helper()

	attrs := &metadata.SetAttrs{
		Mode: &mode,
	}

	err := store.SetFileAttributes(authCtx, handle, attrs)
	require.NoError(t, err, "Failed to set file permissions to %o", mode)
}

// setDirectoryPermissions changes the permission mode of a directory.
func setDirectoryPermissions(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, handle metadata.FileHandle, mode uint32) {
	t.Helper()
	setFilePermissions(t, store, authCtx, handle, mode)
}

// ============================================================================
// Lookup and Verification Helpers
// ============================================================================

// mustNotLookup performs a lookup and requires it to fail with ErrNotFound.
func mustNotLookup(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, dirHandle metadata.FileHandle, name string) {
	t.Helper()

	_, _, err := store.Lookup(authCtx, dirHandle, name)
	AssertErrorCode(t, metadata.ErrNotFound, err, "Expected lookup to fail for: %s", name)
}

// mustGetFile retrieves file attributes and requires success.
func mustGetFile(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle) *metadata.FileAttr {
	t.Helper()
	ctx := context.Background()

	attr, err := store.GetFile(ctx, handle)
	require.NoError(t, err, "GetFile failed for handle: %v", handle)

	return attr
}

// ============================================================================
// Permission Testing Helpers
// ============================================================================

// ============================================================================
// Content Coordination Helpers
// ============================================================================

// prepareWrite is a helper that prepares a write operation.
func prepareWrite(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, handle metadata.FileHandle, newSize uint64) *metadata.WriteOperation {
	t.Helper()

	intent, err := store.PrepareWrite(authCtx, handle, newSize)
	require.NoError(t, err, "PrepareWrite failed")
	require.NotNil(t, intent, "PrepareWrite returned nil intent")

	return intent
}

// commitWrite is a helper that commits a write operation.
func commitWrite(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, intent *metadata.WriteOperation) *metadata.FileAttr {
	t.Helper()

	attr, err := store.CommitWrite(authCtx, intent)
	require.NoError(t, err, "CommitWrite failed")
	require.NotNil(t, attr, "CommitWrite returned nil attributes")

	return attr
}

// prepareRead is a helper that prepares a read operation.
func prepareRead(t *testing.T, store metadata.MetadataStore, authCtx *metadata.AuthContext, handle metadata.FileHandle) *metadata.ReadMetadata {
	t.Helper()

	readMeta, err := store.PrepareRead(authCtx, handle)
	require.NoError(t, err, "PrepareRead failed")
	require.NotNil(t, readMeta, "PrepareRead returned nil metadata")

	return readMeta
}

// ============================================================================
// Batch Creation Helpers
// ============================================================================

// ============================================================================
// Assertion Helpers
// ============================================================================

// assertFileType verifies a file has the expected type.
func assertFileType(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle, expectedType metadata.FileType) {
	t.Helper()

	attr := mustGetFile(t, store, handle)
	require.Equal(t, expectedType, attr.Type, "File type mismatch")
}

// assertFileMode verifies a file has the expected permission mode.
func assertFileMode(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle, expectedMode uint32) {
	t.Helper()

	attr := mustGetFile(t, store, handle)
	require.Equal(t, expectedMode, attr.Mode, "File mode mismatch")
}

// assertFileOwner verifies a file has the expected owner.
func assertFileOwner(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle, expectedUID, expectedGID uint32) {
	t.Helper()

	attr := mustGetFile(t, store, handle)
	require.Equal(t, expectedUID, attr.UID, "File UID mismatch")
	require.Equal(t, expectedGID, attr.GID, "File GID mismatch")
}

// assertFileSize verifies a file has the expected size.
func assertFileSize(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle, expectedSize uint64) {
	t.Helper()

	attr := mustGetFile(t, store, handle)
	require.Equal(t, expectedSize, attr.Size, "File size mismatch")
}

// ============================================================================
// Time Comparison Helpers
// ============================================================================

// assertTimestampAfter verifies that a timestamp occurred after a reference time.
func assertTimestampAfter(t *testing.T, timestamp, reference time.Time, fieldName string) {
	t.Helper()

	require.True(t, timestamp.After(reference), "%s should be after reference time. Got %v, reference %v", fieldName, timestamp, reference)
}

// assertTimestampUnchanged verifies that a timestamp hasn't changed.
func assertTimestampUnchanged(t *testing.T, timestamp, reference time.Time, fieldName string) {
	t.Helper()

	require.True(t, timestamp.Equal(reference), "%s should not have changed. Got %v, reference %v", fieldName, timestamp, reference)
}

// ============================================================================
// Error Assertion Helpers
// ============================================================================

// AssertErrorCode checks if an error has the expected error code.
// This handles both unwrapped ErrorCode and wrapped StoreError.
func AssertErrorCode(t *testing.T, expected metadata.ErrorCode, err error, msgAndArgs ...any) bool {
	if err == nil {
		return assert.Fail(t, "Expected an error but got nil", msgAndArgs...)
	}

	// Try to unwrap as StoreError
	if storeErr, ok := err.(*metadata.StoreError); ok {
		return assert.Equal(t, expected, storeErr.Code, msgAndArgs...)
	}

	// Fall back to direct comparison (in case implementation returns bare ErrorCode)
	return assert.Equal(t, expected, err, msgAndArgs...)
}

// ============================================================================
// Directory Entry Helpers
// ============================================================================

// countNonSpecialEntries counts directory entries excluding "." and "..".
func countNonSpecialEntries(entries []metadata.DirEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Name != "." && entry.Name != ".." {
			count++
		}
	}
	return count
}

// extractEntryNames extracts names from directory entries, excluding "." and "..".
func extractEntryNames(entries []metadata.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name != "." && entry.Name != ".." {
			names = append(names, entry.Name)
		}
	}
	return names
}
