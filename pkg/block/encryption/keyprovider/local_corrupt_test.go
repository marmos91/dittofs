package keyprovider

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// loadKeyFile parses a freshly-generated key file back into its struct so
// tests can mutate individual fields and re-encode a corrupted variant.
func loadKeyFile(t *testing.T, passphrase string) localKeyFile {
	t.Helper()
	raw, err := GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("decode generated key file: nil block")
	}
	var kf localKeyFile
	if err := json.Unmarshal(block.Bytes, &kf); err != nil {
		t.Fatalf("unmarshal generated key file: %v", err)
	}
	return kf
}

// writeMutatedKeyFile re-encodes kf into a PEM key file on disk and returns
// the path.
func writeMutatedKeyFile(t *testing.T, kf localKeyFile) string {
	t.Helper()
	body, err := json.Marshal(kf)
	if err != nil {
		t.Fatalf("marshal mutated key file: %v", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: localPEMType, Bytes: body})
	path := filepath.Join(t.TempDir(), "mutated.key")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write mutated key file: %v", err)
	}
	return path
}

// TestLocal_DecodeKeyFile_CorruptFields exercises every guard in
// decodeKeyFile that rejects a structurally-valid-but-unsupported key
// file. Each case mutates exactly one field of an otherwise-valid file.
func TestLocal_DecodeKeyFile_CorruptFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(kf *localKeyFile)
	}{
		{"unsupported_version", func(kf *localKeyFile) { kf.Version = 99 }},
		{"unsupported_kdf", func(kf *localKeyFile) { kf.KDF.Algo = "scrypt" }},
		{"unsupported_wrap", func(kf *localKeyFile) { kf.Wrap = "chacha20" }},
		{"empty_master_key_id", func(kf *localKeyFile) { kf.MasterKeyID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kf := loadKeyFile(t, testPassphrase)
			tc.mutate(&kf)
			path := writeMutatedKeyFile(t, kf)
			t.Setenv(localPassphraseEnv, testPassphrase)
			_, err := newLocalProvider(Config{Kind: KindLocal, File: path})
			if !errors.Is(err, ErrKeyFileCorrupt) {
				t.Fatalf("%s: want ErrKeyFileCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// TestLocal_UnwrapMasterKey_BadBase64 covers the base64-decode guards in
// unwrapMasterKey for the salt, nonce, and wrapped-key fields.
func TestLocal_UnwrapMasterKey_BadBase64(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(kf *localKeyFile)
	}{
		{"bad_salt", func(kf *localKeyFile) { kf.KDF.Salt = "!!!not-base64!!!" }},
		{"bad_nonce", func(kf *localKeyFile) { kf.Nonce = "!!!not-base64!!!" }},
		{"bad_wrapped", func(kf *localKeyFile) { kf.WrappedMasterKey = "!!!not-base64!!!" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kf := loadKeyFile(t, testPassphrase)
			tc.mutate(&kf)
			path := writeMutatedKeyFile(t, kf)
			t.Setenv(localPassphraseEnv, testPassphrase)
			_, err := newLocalProvider(Config{Kind: KindLocal, File: path})
			if !errors.Is(err, ErrKeyFileCorrupt) {
				t.Fatalf("%s: want ErrKeyFileCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// TestLocal_MissingFileConfig covers the empty-File and missing-file
// guards in newLocalProvider.
func TestLocal_MissingFileConfig(t *testing.T) {
	t.Setenv(localPassphraseEnv, testPassphrase)

	if _, err := newLocalProvider(Config{Kind: KindLocal, File: ""}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty File: want ErrInvalidConfig, got %v", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist.key")
	_, err := newLocalProvider(Config{Kind: KindLocal, File: missing})
	if err == nil {
		t.Fatal("missing file: want error, got nil")
	}
}

// TestLocal_EmptyBlockKey verifies Wrap rejects a zero-length block key.
func TestLocal_EmptyBlockKey(t *testing.T) {
	p := newLocalForTest(t)
	if _, _, err := p.Wrap(context.Background(), nil); err == nil {
		t.Fatal("Wrap with empty block key: want error, got nil")
	}
}

// TestLocal_Unwrap_ShortPayload verifies a too-short wrapped payload is
// rejected before AEAD.Open.
func TestLocal_Unwrap_ShortPayload(t *testing.T) {
	p := newLocalForTest(t)
	if _, err := p.Unwrap(context.Background(), []byte{0x01, 0x02}, p.CurrentMasterKeyID()); !errors.Is(err, ErrUnwrapFailed) {
		t.Fatalf("Unwrap short payload: want ErrUnwrapFailed, got %v", err)
	}
}
