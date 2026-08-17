package keyprovider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeKeyFileAt stages a fresh key file at a caller-chosen name so a
// test can hold several at once, which writeKeyFile's fixed basename
// cannot do.
func writeKeyFileAt(t *testing.T, dir, name, passphrase string) string {
	t.Helper()
	keyFileBytes, err := GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, keyFileBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// TestLocal_RetiredKeyStillUnwraps is the property the whole retired-key
// map exists for: a block wrapped before rotation stays readable after
// the current key changes, provided the old key file is retired rather
// than discarded.
func TestLocal_RetiredKeyStillUnwraps(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeKeyFileAt(t, dir, "old.key", testPassphrase)
	newPath := writeKeyFileAt(t, dir, "new.key", testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)

	// Wrap a block under the old key, as a pre-rotation daemon would.
	before, err := newLocalProvider(Config{Kind: KindLocal, File: oldPath})
	if err != nil {
		t.Fatalf("newLocalProvider(old): %v", err)
	}
	blockKey := bytes.Repeat([]byte{0x11}, 32)
	wrapped, oldID, err := before.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap under old key: %v", err)
	}
	_ = before.Close()

	// Rotate: new key becomes current, old key is retired.
	after, err := newLocalProvider(Config{
		Kind:         KindLocal,
		File:         newPath,
		RetiredFiles: []string{oldPath},
	})
	if err != nil {
		t.Fatalf("newLocalProvider(rotated): %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })

	if after.CurrentMasterKeyID() == oldID {
		t.Fatal("rotated provider still reports the old master key id as current")
	}

	got, err := after.Unwrap(context.Background(), wrapped, oldID)
	if err != nil {
		t.Fatalf("Unwrap of pre-rotation block: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap returned %x, want %x", got, blockKey)
	}

	// New writes go under the new key and read back through the same
	// provider, so rotation does not strand the current key either.
	freshWrapped, freshID, err := after.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap under new key: %v", err)
	}
	if freshID != after.CurrentMasterKeyID() {
		t.Fatalf("Wrap recorded id %q, want current %q", freshID, after.CurrentMasterKeyID())
	}
	if _, err := after.Unwrap(context.Background(), freshWrapped, freshID); err != nil {
		t.Fatalf("Unwrap of post-rotation block: %v", err)
	}
}

// TestLocal_DiscardedKeyStillRejected keeps the failure mode honest: an
// id belonging to no configured key is still ErrWrongMasterKey, so the
// lookup did not turn into a silent accept-anything.
func TestLocal_DiscardedKeyStillRejected(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeKeyFileAt(t, dir, "old.key", testPassphrase)
	newPath := writeKeyFileAt(t, dir, "new.key", testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)

	before, err := newLocalProvider(Config{Kind: KindLocal, File: oldPath})
	if err != nil {
		t.Fatalf("newLocalProvider(old): %v", err)
	}
	wrapped, oldID, err := before.Wrap(context.Background(), bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	_ = before.Close()

	// Rotate WITHOUT retiring the old key — the data-loss case the issue
	// describes.
	after, err := newLocalProvider(Config{Kind: KindLocal, File: newPath})
	if err != nil {
		t.Fatalf("newLocalProvider(rotated): %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })

	if _, err := after.Unwrap(context.Background(), wrapped, oldID); !errors.Is(err, ErrWrongMasterKey) {
		t.Fatalf("Unwrap error = %v, want ErrWrongMasterKey", err)
	}
}

// TestLocal_UnreadableRetiredKeyDegrades pins the startup decision: a
// retired key that cannot be loaded costs access to the blocks under it
// but does not take the provider (and every other share) down with it.
func TestLocal_UnreadableRetiredKeyDegrades(t *testing.T) {
	dir := t.TempDir()
	newPath := writeKeyFileAt(t, dir, "new.key", testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)

	p, err := newLocalProvider(Config{
		Kind:         KindLocal,
		File:         newPath,
		RetiredFiles: []string{filepath.Join(dir, "does-not-exist.key")},
	})
	if err != nil {
		t.Fatalf("newLocalProvider with an unreadable retired key: %v, want success", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// The current key still works.
	blockKey := bytes.Repeat([]byte{0x33}, 32)
	wrapped, id, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := p.Unwrap(context.Background(), wrapped, id); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
}

// TestLocal_DuplicateRetiredIDRejected covers the other half of the
// startup decision: an ambiguous id is a config error, because which key
// Unwrap would pick is otherwise arbitrary.
func TestLocal_DuplicateRetiredIDRejected(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeKeyFileAt(t, dir, "old.key", testPassphrase)
	newPath := writeKeyFileAt(t, dir, "new.key", testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)

	t.Run("same key listed twice", func(t *testing.T) {
		_, err := newLocalProvider(Config{
			Kind:         KindLocal,
			File:         newPath,
			RetiredFiles: []string{oldPath, oldPath},
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
	})

	t.Run("current key also listed as retired", func(t *testing.T) {
		_, err := newLocalProvider(Config{
			Kind:         KindLocal,
			File:         newPath,
			RetiredFiles: []string{newPath},
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
	})
}

// TestLocal_CloseZeroesRetiredKeys guards the Close contract: retired
// material is key material too, and leaving it live after Close would
// keep it resident for the process lifetime.
func TestLocal_CloseZeroesRetiredKeys(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeKeyFileAt(t, dir, "old.key", testPassphrase)
	newPath := writeKeyFileAt(t, dir, "new.key", testPassphrase)
	t.Setenv(localPassphraseEnv, testPassphrase)

	p, err := newLocalProvider(Config{
		Kind:         KindLocal,
		File:         newPath,
		RetiredFiles: []string{oldPath},
	})
	if err != nil {
		t.Fatalf("newLocalProvider: %v", err)
	}
	if len(p.retired) != 1 {
		t.Fatalf("retired set holds %d keys, want 1", len(p.retired))
	}
	// Keep a handle on the backing array so the zeroing is observable
	// after the map entry is dropped.
	var retainedKey []byte
	for _, k := range p.retired {
		retainedKey = k
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(p.retired) != 0 {
		t.Fatalf("retired set still holds %d keys after Close", len(p.retired))
	}
	if !bytes.Equal(retainedKey, make([]byte, len(retainedKey))) {
		t.Fatalf("retired key material not zeroed after Close: %x", retainedKey)
	}
}
