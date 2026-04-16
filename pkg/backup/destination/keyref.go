package destination

import (
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarNamePattern is the POSIX-portable env var name shape. We require
// uppercase to match Unix conventions and avoid operator surprise when
// exporting the variable from a shell.
var envVarNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// aes256KeyLen is the raw key size in bytes for AES-256 (D-09).
const aes256KeyLen = 32

// ResolveKey parses ref (scheme:target) and returns the raw 32-byte AES-256
// key. Callers MUST zero the returned slice immediately after cipher.NewGCM
// consumes it (D-09 defense in depth — minimizes time-in-memory).
//
// Supported schemes:
//
//	env:NAME       — env var named NAME holds 64 lowercase hex characters
//	file:/abs/path — regular file containing exactly 32 raw bytes
//
// Any other scheme (or a bare target with no scheme separator) returns
// ErrIncompatibleConfig. Missing resources return ErrEncryptionKeyMissing.
// Malformed key material returns ErrInvalidKeyMaterial.
func ResolveKey(ref string) ([]byte, error) {
	scheme, target, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" || target == "" {
		return nil, fmt.Errorf("%w: key ref must be scheme:target (got %q)", ErrIncompatibleConfig, ref)
	}
	switch scheme {
	case "env":
		return resolveEnvKey(target)
	case "file":
		return resolveFileKey(target)
	default:
		return nil, fmt.Errorf("%w: unsupported key-ref scheme %q", ErrIncompatibleConfig, scheme)
	}
}

// ValidateKeyRef performs the same scheme/format validation as ResolveKey
// but does NOT load the key material. Safe to call at repo-create time when
// the key may only exist on the production host — validates the reference
// shape without requiring the referenced resource to be present.
func ValidateKeyRef(ref string) error {
	scheme, target, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" || target == "" {
		return fmt.Errorf("%w: key ref must be scheme:target (got %q)", ErrIncompatibleConfig, ref)
	}
	switch scheme {
	case "env":
		if !envVarNamePattern.MatchString(target) {
			return fmt.Errorf("%w: env var name %q does not match [A-Z_][A-Z0-9_]*", ErrIncompatibleConfig, target)
		}
		return nil
	case "file":
		if !strings.HasPrefix(target, "/") {
			return fmt.Errorf("%w: file path must be absolute, got %q", ErrIncompatibleConfig, target)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported key-ref scheme %q", ErrIncompatibleConfig, scheme)
	}
}

// resolveEnvKey resolves an env:NAME reference to 32 raw key bytes.
func resolveEnvKey(name string) ([]byte, error) {
	if !envVarNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: env var name %q does not match [A-Z_][A-Z0-9_]*", ErrIncompatibleConfig, name)
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, fmt.Errorf("%w: env var %s is empty or unset", ErrEncryptionKeyMissing, name)
	}
	if len(raw) != 64 {
		return nil, fmt.Errorf("%w: env var %s must be 64 lowercase hex chars, got %d", ErrInvalidKeyMaterial, name, len(raw))
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: env var %s hex decode: %v", ErrInvalidKeyMaterial, name, err)
	}
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("%w: env var %s decoded to %d bytes (want %d)", ErrInvalidKeyMaterial, name, len(key), aes256KeyLen)
	}
	return key, nil
}

// resolveFileKey resolves a file:/abs/path reference to 32 raw key bytes.
// The path is type-validated (absolute, regular file, exactly 32 bytes)
// before Open is called, so the os.ReadFile below is safe against the
// gosec G304 tainted-input warning.
func resolveFileKey(path string) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w: file path must be absolute, got %q", ErrIncompatibleConfig, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: stat %s: %v", ErrEncryptionKeyMissing, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrIncompatibleConfig, path)
	}
	if info.Size() != aes256KeyLen {
		return nil, fmt.Errorf("%w: %s must be exactly %d bytes, got %d", ErrInvalidKeyMaterial, path, aes256KeyLen, info.Size())
	}
	key, err := os.ReadFile(path) //nolint:gosec // path type-validated above
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrEncryptionKeyMissing, path, err)
	}
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("%w: %s read short (%d of %d bytes)", ErrInvalidKeyMaterial, path, len(key), aes256KeyLen)
	}
	return key, nil
}
