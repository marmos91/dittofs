package keyprovider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gemalto/kmip-go/ttlv"
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

// kmipRotationEnv stages the TLS material and returns a Config template
// plus a helper that starts a fake serving the given responses in order.
func kmipRotationEnv(t *testing.T) (Config, func(...ttlv.TTLV) *fakeKMIPServer) {
	t.Helper()
	dir := t.TempDir()
	srvCert, srvKey, srvPEM := genSelfSigned(t, dir, "server")
	cliCert, cliKey, _ := genSelfSigned(t, dir, "client")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, srvPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	cfg := Config{
		Kind:       KindKMIP,
		ClientCert: cliCert,
		ClientKey:  cliKey,
		ServerCA:   caPath,
		TimeoutMS:  5000,
	}
	return cfg, func(resps ...ttlv.TTLV) *fakeKMIPServer {
		return startFakeKMIPSeq(t, serverTLSConfig(t, srvCert, srvKey), resps...)
	}
}

// TestKMIP_RetiredUIDStillUnwraps mirrors the local rotation test across
// the KMIP wiring: retired uids are fetched at startup, each uid is its
// own master key id, and a block wrapped under the retired key still
// unwraps once the current uid has moved on.
func TestKMIP_RetiredUIDStillUnwraps(t *testing.T) {
	cfg, serve := kmipRotationEnv(t)
	oldMaterial := bytes.Repeat([]byte{0xA1}, 32)
	newMaterial := bytes.Repeat([]byte{0xB2}, 32)

	// Pre-rotation daemon: only the old uid is configured.
	srv := serve(kmipSuccessResponse(t, "uid-old", oldMaterial))
	oldCfg := cfg
	oldCfg.Endpoint = srv.addr()
	oldCfg.KeyUID = "uid-old"
	before, err := newKMIPProvider(context.Background(), oldCfg)
	if err != nil {
		t.Fatalf("newKMIPProvider(old): %v", err)
	}
	blockKey := bytes.Repeat([]byte{0x44}, 32)
	wrapped, oldID, err := before.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap under old key: %v", err)
	}
	if oldID != "uid-old" {
		t.Fatalf("Wrap recorded id %q, want the configured uid", oldID)
	}
	_ = before.Close()

	// Rotate: current uid becomes uid-new, uid-old is retired. The
	// provider fetches the current key first, then each retired uid in
	// order, so the fake answers new-then-old.
	srv2 := serve(
		kmipSuccessResponse(t, "uid-new", newMaterial),
		kmipSuccessResponse(t, "uid-old", oldMaterial),
	)
	rotated := cfg
	rotated.Endpoint = srv2.addr()
	rotated.KeyUID = "uid-new"
	rotated.RetiredKeyUIDs = []string{"uid-old"}
	after, err := newKMIPProvider(context.Background(), rotated)
	if err != nil {
		t.Fatalf("newKMIPProvider(rotated): %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })

	if !bytes.Equal(after.masterKey, newMaterial) {
		t.Fatalf("current key = %x, want the new material", after.masterKey)
	}
	if got, ok := after.retired["uid-old"]; !ok || !bytes.Equal(got, oldMaterial) {
		t.Fatalf("retired[uid-old] = %x (present=%v), want the old material", got, ok)
	}

	got, err := after.Unwrap(context.Background(), wrapped, oldID)
	if err != nil {
		t.Fatalf("Unwrap of pre-rotation block: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap returned %x, want %x", got, blockKey)
	}
}

// TestKMIP_RetiredUIDValidation covers the two startup rejections on the
// KMIP path, where the uid is the id and duplicates are therefore visible
// in config rather than only inside the key material.
func TestKMIP_RetiredUIDValidation(t *testing.T) {
	material := bytes.Repeat([]byte{0xC3}, 32)

	t.Run("current uid also listed as retired", func(t *testing.T) {
		cfg, serve := kmipRotationEnv(t)
		srv := serve(
			kmipSuccessResponse(t, "uid-1", material),
			kmipSuccessResponse(t, "uid-1", material),
		)
		cfg.Endpoint = srv.addr()
		cfg.KeyUID = "uid-1"
		cfg.RetiredKeyUIDs = []string{"uid-1"}
		if _, err := newKMIPProvider(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
	})

	t.Run("same retired uid listed twice", func(t *testing.T) {
		cfg, serve := kmipRotationEnv(t)
		srv := serve(
			kmipSuccessResponse(t, "uid-new", material),
			kmipSuccessResponse(t, "uid-old", material),
			kmipSuccessResponse(t, "uid-old", material),
		)
		cfg.Endpoint = srv.addr()
		cfg.KeyUID = "uid-new"
		cfg.RetiredKeyUIDs = []string{"uid-old", "uid-old"}
		if _, err := newKMIPProvider(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
	})
}

// TestKMIP_UnreadableRetiredUIDDegrades pins the same startup decision as
// the local case: a retired uid the HSM will not serve costs access to
// the blocks under it, not the whole provider.
func TestKMIP_UnreadableRetiredUIDDegrades(t *testing.T) {
	cfg, serve := kmipRotationEnv(t)
	material := bytes.Repeat([]byte{0xD4}, 32)
	// Only one response staged, so the retired uid's fetch connects and
	// then waits for a reply that never comes. A short timeout keeps the
	// test from sitting on the default five seconds.
	srv := serve(kmipSuccessResponse(t, "uid-new", material))
	cfg.Endpoint = srv.addr()
	cfg.KeyUID = "uid-new"
	cfg.RetiredKeyUIDs = []string{"uid-gone"}
	cfg.TimeoutMS = 300

	p, err := newKMIPProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newKMIPProvider with an unfetchable retired uid: %v, want success", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if len(p.retired) != 0 {
		t.Fatalf("retired set holds %d keys, want 0", len(p.retired))
	}
	blockKey := bytes.Repeat([]byte{0x55}, 32)
	wrapped, id, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := p.Unwrap(context.Background(), wrapped, id); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
}

// TestLoadRetiredKeys_ZeroesOnRejection covers the construction-failure
// path directly: both rejections happen after key material is already in
// memory, and a provider that never gets built has no Close to clean up
// after it, so loadRetiredKeys has to zero what it loaded on the way out.
func TestLoadRetiredKeys_ZeroesOnRejection(t *testing.T) {
	// stubLoader hands out key material the test keeps a handle on, so the
	// zeroing is observable after loadRetiredKeys has dropped its own
	// references.
	newStub := func(ids ...string) (retiredKeyLoader, *[][]byte) {
		issued := make([][]byte, 0, len(ids))
		i := 0
		return func(string) (string, []byte, error) {
			key := bytes.Repeat([]byte{0xEE}, 32)
			issued = append(issued, key)
			id := ids[i]
			i++
			return id, key, nil
		}, &issued
	}

	assertAllZeroed := func(t *testing.T, issuedPtr *[][]byte) {
		t.Helper()
		issued := *issuedPtr
		if len(issued) == 0 {
			t.Fatal("loader was never called")
		}
		for n, key := range issued {
			if !bytes.Equal(key, make([]byte, len(key))) {
				t.Fatalf("key %d not zeroed after rejection: %x", n, key)
			}
		}
	}

	t.Run("duplicate retired id", func(t *testing.T) {
		load, issued := newStub("dup", "dup")
		_, err := loadRetiredKeys("current", []string{"a", "b"}, func(ref string) (string, []byte, error) {
			return load(ref)
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
		assertAllZeroed(t, issued)
	})

	t.Run("retired id collides with current", func(t *testing.T) {
		load, issued := newStub("retired-ok", "current")
		_, err := loadRetiredKeys("current", []string{"a", "b"}, func(ref string) (string, []byte, error) {
			return load(ref)
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want ErrInvalidConfig", err)
		}
		assertAllZeroed(t, issued)
	})
}
