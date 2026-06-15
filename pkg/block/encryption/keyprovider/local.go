package keyprovider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

// localPassphraseEnv names the environment variable holding the
// passphrase that unlocks the on-disk key file. Kept out of every
// configuration file by design — it must travel through the process
// environment only.
const localPassphraseEnv = "DITTOFS_ENCRYPTION_PASSPHRASE"

const localPEMType = "DITTOFS ENCRYPTED MASTER KEY"

const localKeyFileVersion = 1

// Argon2 KDF parameter bounds enforced on read. The lower bounds keep
// argon2.IDKey from panicking on a corrupt/tampered file (it panics when
// time < 1 or threads < 1) and reject a key file weaker than was ever
// generated. The upper bounds cap unwrap cost so an attacker who can write
// the key file cannot turn startup into an OOM-kill or a multi-minute
// hang: maxArgonMemoryKiB ≈ 4 GiB and maxArgonTime/maxArgonThreads are far
// above any sane configuration. Defaults written by GenerateKeyFile
// (t=3, m=64 MiB, p=4) sit comfortably inside this window.
const (
	minArgonTime      = 1
	maxArgonTime      = 64
	minArgonMemoryKiB = 8 * 1024
	maxArgonMemoryKiB = 4 * 1024 * 1024
	minArgonThreads   = 1
	maxArgonThreads   = 64
)

// argon2Params holds the KDF parameters persisted alongside the wrapped
// master key. Defaults follow the OWASP 2024 password-storage cheat
// sheet for Argon2id (m=64 MiB, t=3, p=4).
type argon2Params struct {
	Algo      string `json:"algo"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

// localKeyFile is the JSON body wrapped inside the PEM block.
type localKeyFile struct {
	Version          int          `json:"version"`
	MasterKeyID      string       `json:"master_key_id"`
	KDF              argon2Params `json:"kdf"`
	Wrap             string       `json:"wrap"`
	Nonce            string       `json:"nonce"`
	WrappedMasterKey string       `json:"wrapped_master_key"`
}

// localProvider unlocks a single master key from a passphrase-protected
// file and uses AES-256-GCM with a random nonce to wrap per-block keys.
//
// Nonce-reuse safety: with a fresh 96-bit nonce per Wrap call, the
// birthday bound for collision is ~2^48 calls per master key — many
// orders of magnitude beyond any realistic block volume.
type localProvider struct {
	aesGCMKEK
}

func newLocalProvider(cfg Config) (*localProvider, error) {
	if cfg.File == "" {
		return nil, fmt.Errorf("%w: local provider requires a file path", ErrInvalidConfig)
	}
	passphrase := os.Getenv(localPassphraseEnv)
	if passphrase == "" {
		return nil, ErrPassphraseMissing
	}
	raw, err := os.ReadFile(cfg.File)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: read key file: %w", err)
	}
	kf, err := decodeKeyFile(raw)
	if err != nil {
		return nil, err
	}
	masterKey, err := unwrapMasterKey(kf, passphrase)
	if err != nil {
		return nil, err
	}
	return &localProvider{aesGCMKEK: aesGCMKEK{masterKey: masterKey, masterKeyID: kf.MasterKeyID}}, nil
}

// GenerateKeyFile produces the bytes of a fresh passphrase-protected key
// file. The caller is responsible for writing the bytes to disk with
// suitable permissions (mode 0600 is recommended). Exposed so the dfsctl
// CLI can offer an "auto-create on first use" affordance without
// reinventing the encoding.
func GenerateKeyFile(passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, ErrPassphraseMissing
	}
	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		return nil, fmt.Errorf("keyprovider: read master key: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("keyprovider: read salt: %w", err)
	}
	const (
		argTime    = 3
		argMemKiB  = 64 * 1024
		argThreads = 4
	)
	derived := argon2.IDKey([]byte(passphrase), salt, argTime, argMemKiB, argThreads, 32)
	aead, err := newGCM(derived)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("keyprovider: read wrap nonce: %w", err)
	}
	wrapped := aead.Seal(nil, nonce, masterKey, nil)
	kf := localKeyFile{
		Version:     localKeyFileVersion,
		MasterKeyID: uuid.NewString(),
		KDF: argon2Params{
			Algo:      "argon2id",
			Salt:      base64.StdEncoding.EncodeToString(salt),
			Time:      argTime,
			MemoryKiB: argMemKiB,
			Threads:   argThreads,
		},
		Wrap:             "aes-256-gcm",
		Nonce:            base64.StdEncoding.EncodeToString(nonce),
		WrappedMasterKey: base64.StdEncoding.EncodeToString(wrapped),
	}
	body, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("keyprovider: marshal key file: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: localPEMType, Bytes: body}), nil
}

func decodeKeyFile(raw []byte) (*localKeyFile, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != localPEMType {
		return nil, fmt.Errorf("%w: missing %q PEM block", ErrKeyFileCorrupt, localPEMType)
	}
	var kf localKeyFile
	if err := json.Unmarshal(block.Bytes, &kf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyFileCorrupt, err)
	}
	if kf.Version != localKeyFileVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrKeyFileCorrupt, kf.Version)
	}
	if kf.KDF.Algo != "argon2id" {
		return nil, fmt.Errorf("%w: unsupported kdf %q", ErrKeyFileCorrupt, kf.KDF.Algo)
	}
	if kf.Wrap != "aes-256-gcm" {
		return nil, fmt.Errorf("%w: unsupported wrap %q", ErrKeyFileCorrupt, kf.Wrap)
	}
	if kf.MasterKeyID == "" {
		return nil, fmt.Errorf("%w: empty master_key_id", ErrKeyFileCorrupt)
	}
	if err := validateKDFParams(kf.KDF); err != nil {
		return nil, err
	}
	return &kf, nil
}

// validateKDFParams bounds the Argon2 parameters read from disk before they
// reach argon2.IDKey. Without these guards a corrupt or attacker-replaced
// key file with time=0 or threads=0 panics the daemon at startup, and a
// huge memory_kib triggers an OOM-kill — both an availability/DoS vector.
func validateKDFParams(p argon2Params) error {
	if p.Time < minArgonTime || p.Time > maxArgonTime {
		return fmt.Errorf("%w: kdf time %d out of range [%d,%d]", ErrKeyFileCorrupt, p.Time, minArgonTime, maxArgonTime)
	}
	if p.MemoryKiB < minArgonMemoryKiB || p.MemoryKiB > maxArgonMemoryKiB {
		return fmt.Errorf("%w: kdf memory_kib %d out of range [%d,%d]", ErrKeyFileCorrupt, p.MemoryKiB, minArgonMemoryKiB, maxArgonMemoryKiB)
	}
	if p.Threads < minArgonThreads || p.Threads > maxArgonThreads {
		return fmt.Errorf("%w: kdf threads %d out of range [%d,%d]", ErrKeyFileCorrupt, p.Threads, minArgonThreads, maxArgonThreads)
	}
	return nil
}

func unwrapMasterKey(kf *localKeyFile, passphrase string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(kf.KDF.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: salt: %v", ErrKeyFileCorrupt, err)
	}
	nonce, err := base64.StdEncoding.DecodeString(kf.Nonce)
	if err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", ErrKeyFileCorrupt, err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(kf.WrappedMasterKey)
	if err != nil {
		return nil, fmt.Errorf("%w: wrapped_master_key: %v", ErrKeyFileCorrupt, err)
	}
	derived := argon2.IDKey([]byte(passphrase), salt, kf.KDF.Time, kf.KDF.MemoryKiB, kf.KDF.Threads, 32)
	aead, err := newGCM(derived)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, wrapped, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnwrapFailed, err)
	}
	if len(plain) != 32 {
		return nil, fmt.Errorf("%w: unwrapped master key has unexpected length %d", ErrKeyFileCorrupt, len(plain))
	}
	return plain, nil
}

// aesGCMKEK is the shared Wrap / Unwrap / Close implementation used by
// any provider that holds an in-memory 32-byte symmetric KEK. The
// wrapped layout is `nonce || ciphertext-with-tag` under AES-256-GCM.
type aesGCMKEK struct {
	masterKey   []byte
	masterKeyID string
}

func (k *aesGCMKEK) CurrentMasterKeyID() string { return k.masterKeyID }

func (k *aesGCMKEK) Wrap(_ context.Context, blockKey []byte) ([]byte, string, error) {
	if len(blockKey) == 0 {
		return nil, "", fmt.Errorf("keyprovider: empty block key")
	}
	aead, err := newGCM(k.masterKey)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("keyprovider: read nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(blockKey)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, blockKey, nil)
	return out, k.masterKeyID, nil
}

func (k *aesGCMKEK) Unwrap(_ context.Context, wrapped []byte, masterKeyID string) ([]byte, error) {
	if masterKeyID != "" && masterKeyID != k.masterKeyID {
		return nil, fmt.Errorf("%w: have %q want %q", ErrWrongMasterKey, k.masterKeyID, masterKeyID)
	}
	aead, err := newGCM(k.masterKey)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(wrapped) < ns+aead.Overhead() {
		return nil, fmt.Errorf("%w: wrapped payload too short (%d bytes)", ErrUnwrapFailed, len(wrapped))
	}
	nonce, ciphertext := wrapped[:ns], wrapped[ns:]
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnwrapFailed, err)
	}
	return plain, nil
}

// Close zeros the master key bytes in memory. Best-effort — the Go
// runtime makes no guarantee the bytes are not retained on a GC heap.
func (k *aesGCMKEK) Close() error {
	for i := range k.masterKey {
		k.masterKey[i] = 0
	}
	k.masterKey = nil
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("keyprovider: GCM key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: cipher.NewGCM: %w", err)
	}
	return aead, nil
}
