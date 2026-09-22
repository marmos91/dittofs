package keyprovider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testPassphrase = "correct horse battery staple"

// writeKeyFile creates a fresh passphrase-protected key file in a
// tempdir and returns the path. Tests use it to stage a real provider
// without coupling to the dfsctl CLI.
func writeKeyFile(t *testing.T, passphrase string) string {
	t.Helper()
	keyFileBytes, err := GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(path, keyFileBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func newLocalForTest(t *testing.T) *localProvider {
	t.Helper()
	path := writeKeyFile(t, testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)
	p, err := newLocalProvider(Config{Kind: KindLocal, File: path})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestLocal_WrapUnwrapRoundTrip(t *testing.T) {
	p := newLocalForTest(t)
	blockKey := bytes.Repeat([]byte{0x11}, 32)
	wrapped, id, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if id != p.CurrentMasterKeyID() {
		t.Fatalf("master key id mismatch: %q vs %q", id, p.CurrentMasterKeyID())
	}
	got, err := p.Unwrap(context.Background(), wrapped, id)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap returned %x, want %x", got, blockKey)
	}
}

func TestLocal_WrapProducesUniqueCiphertext(t *testing.T) {
	p := newLocalForTest(t)
	blockKey := bytes.Repeat([]byte{0x22}, 32)
	a, _, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap a: %v", err)
	}
	b, _, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two Wraps of the same key produced identical bytes (nonce reuse?)")
	}
}

func TestLocal_TamperFailsAuth(t *testing.T) {
	p := newLocalForTest(t)
	wrapped, id, err := p.Wrap(context.Background(), bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	wrapped[len(wrapped)-1] ^= 0xFF
	_, err = p.Unwrap(context.Background(), wrapped, id)
	if !errors.Is(err, ErrUnwrapFailed) {
		t.Fatalf("Unwrap on tampered ciphertext: want ErrUnwrapFailed, got %v", err)
	}
}

func TestLocal_WrongMasterKeyID(t *testing.T) {
	p := newLocalForTest(t)
	wrapped, _, err := p.Wrap(context.Background(), bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	_, err = p.Unwrap(context.Background(), wrapped, "some-other-key-uuid")
	if !errors.Is(err, ErrWrongMasterKey) {
		t.Fatalf("Unwrap with wrong id: want ErrWrongMasterKey, got %v", err)
	}
}

func TestLocal_MissingPassphrase(t *testing.T) {
	path := writeKeyFile(t, testPassphrase)
	t.Setenv(localPassphraseEnv, "")
	_, err := newLocalProvider(Config{Kind: KindLocal, File: path})
	if !errors.Is(err, ErrPassphraseMissing) {
		t.Fatalf("missing passphrase: want ErrPassphraseMissing, got %v", err)
	}
}

func TestLocal_WrongPassphrase(t *testing.T) {
	path := writeKeyFile(t, testPassphrase)
	t.Setenv(localPassphraseEnv, "wrong-pass")
	_, err := newLocalProvider(Config{Kind: KindLocal, File: path})
	if !errors.Is(err, ErrUnwrapFailed) {
		t.Fatalf("wrong passphrase: want ErrUnwrapFailed, got %v", err)
	}
}

func TestLocal_CorruptPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.key")
	if err := os.WriteFile(path, []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	t.Setenv(localPassphraseEnv, testPassphrase)
	_, err := newLocalProvider(Config{Kind: KindLocal, File: path})
	if !errors.Is(err, ErrKeyFileCorrupt) {
		t.Fatalf("corrupt PEM: want ErrKeyFileCorrupt, got %v", err)
	}
}

func TestLocal_NewProviderRoute(t *testing.T) {
	path := writeKeyFile(t, testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)
	p, err := NewProvider(context.Background(), Config{Kind: KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if p.CurrentMasterKeyID() == "" {
		t.Fatal("CurrentMasterKeyID is empty")
	}
}

func TestNewProvider_UnknownKind(t *testing.T) {
	_, err := NewProvider(context.Background(), Config{Kind: "totally-bogus"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown kind: want ErrInvalidConfig, got %v", err)
	}
}

func TestNewProvider_MissingKind(t *testing.T) {
	_, err := NewProvider(context.Background(), Config{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing kind: want ErrInvalidConfig, got %v", err)
	}
}
