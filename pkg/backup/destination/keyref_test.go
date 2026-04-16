package destination

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- env: scheme ---------------------------------------------------------

func TestResolveKey_EnvHappyPath(t *testing.T) {
	// 64 hex chars = 32 bytes; use "ab" repeated 32 times so the decoded
	// first byte is 0xab (easy assertion without depending on random data).
	t.Setenv("DITTOFS_TEST_KEY_ABC", strings.Repeat("ab", 32))
	key, err := ResolveKey("env:DITTOFS_TEST_KEY_ABC")
	if err != nil {
		t.Fatalf("ResolveKey returned err: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("len(key) = %d, want 32", len(key))
	}
	if key[0] != 0xab {
		t.Fatalf("key[0] = 0x%02x, want 0xab", key[0])
	}
}

func TestResolveKey_EnvMissing(t *testing.T) {
	t.Setenv("DITTOFS_TEST_KEY_MISSING", "")
	_, err := ResolveKey("env:DITTOFS_TEST_KEY_MISSING")
	if !errors.Is(err, ErrEncryptionKeyMissing) {
		t.Fatalf("err = %v, want errors.Is ErrEncryptionKeyMissing", err)
	}
}

func TestResolveKey_EnvWrongLength(t *testing.T) {
	t.Setenv("DITTOFS_TEST_KEY_SHORT", "deadbeef")
	_, err := ResolveKey("env:DITTOFS_TEST_KEY_SHORT")
	if !errors.Is(err, ErrInvalidKeyMaterial) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidKeyMaterial", err)
	}
}

func TestResolveKey_EnvNotHex(t *testing.T) {
	t.Setenv("DITTOFS_TEST_KEY_BADHEX", strings.Repeat("g", 64))
	_, err := ResolveKey("env:DITTOFS_TEST_KEY_BADHEX")
	if !errors.Is(err, ErrInvalidKeyMaterial) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidKeyMaterial", err)
	}
}

func TestResolveKey_EnvNameBad(t *testing.T) {
	_, err := ResolveKey("env:lowercase")
	if !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}

// --- file: scheme --------------------------------------------------------

func TestResolveKey_FileHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ResolveKey("file:" + path)
	if err != nil {
		t.Fatalf("ResolveKey returned err: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("len(key) = %d, want 32", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d: got 0x%02x want 0x%02x", i, got[i], want[i])
		}
	}
}

func TestResolveKey_FileRelativePath(t *testing.T) {
	_, err := ResolveKey("file:rel/path.key")
	if !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}

func TestResolveKey_FileMissing(t *testing.T) {
	// /nonexistent is never a real absolute path on a test machine.
	_, err := ResolveKey("file:/nonexistent/abs/path.key")
	if !errors.Is(err, ErrEncryptionKeyMissing) {
		t.Fatalf("err = %v, want errors.Is ErrEncryptionKeyMissing", err)
	}
}

func TestResolveKey_FileWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short")
	if err := os.WriteFile(path, make([]byte, 31), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := ResolveKey("file:" + path)
	if !errors.Is(err, ErrInvalidKeyMaterial) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidKeyMaterial", err)
	}
}

func TestResolveKey_FileNotRegular(t *testing.T) {
	dir := t.TempDir() // dir itself is not a regular file
	_, err := ResolveKey("file:" + dir)
	if !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}

// --- generic rejections --------------------------------------------------

func TestResolveKey_BareString(t *testing.T) {
	_, err := ResolveKey("bare")
	if !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}

func TestResolveKey_UnknownScheme(t *testing.T) {
	_, err := ResolveKey("kms:foo")
	if !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}

// --- ValidateKeyRef ------------------------------------------------------

func TestValidateKeyRef_FormatOnly(t *testing.T) {
	// ValidateKeyRef must NOT load the env var — the name passes the
	// regex check, so validation returns nil even though the value is unset.
	if err := ValidateKeyRef("env:NONEXISTENT_VAR"); err != nil {
		t.Fatalf("ValidateKeyRef returned err: %v", err)
	}
	// Similarly for file: the path is absolute-shaped; no Stat happens.
	if err := ValidateKeyRef("file:/etc/dittofs/nonexistent.key"); err != nil {
		t.Fatalf("ValidateKeyRef returned err for absolute path: %v", err)
	}
}

func TestValidateKeyRef_RejectsBadScheme(t *testing.T) {
	if err := ValidateKeyRef("http://example.com"); !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
	if err := ValidateKeyRef("bare"); !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
	if err := ValidateKeyRef("env:lowercase"); !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
	if err := ValidateKeyRef("file:relative/path"); !errors.Is(err, ErrIncompatibleConfig) {
		t.Fatalf("err = %v, want errors.Is ErrIncompatibleConfig", err)
	}
}
