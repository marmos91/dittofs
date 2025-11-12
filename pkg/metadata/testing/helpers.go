package testing

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
)

// DefaultRootDirAttr creates default attributes for a share root directory.
// These are sensible defaults for testing.
func DefaultRootDirAttr() *metadata.FileAttr {
	now := time.Now()
	return &metadata.FileAttr{
		Type:  metadata.FileTypeDirectory,
		Mode:  0755,
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
		Identity: RootIdentity(),
	}
}

// UserAuthContext returns an AuthContext for a regular user.
func UserAuthContext(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Identity: UserIdentity(uid, gid),
	}
}

// AnonymousAuthContext returns an AuthContext for anonymous user.
func AnonymousAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Identity: AnonymousIdentity(),
	}
}

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
