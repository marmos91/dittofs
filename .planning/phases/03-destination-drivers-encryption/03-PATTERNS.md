# Phase 3: Destination Drivers + Encryption — Pattern Map

**Mapped:** 2026-04-16
**Files analyzed:** 10 new files (5 top-level package + 2 `fs/` + 2 `s3/` + 1 shared conformance helper)
**Analogs found:** 10 / 10 (100% coverage)

Phase 3 sits **parallel** to `pkg/blockstore/remote` — a new top-level driver
package with its own sub-drivers. The single closest source of patterns is
`pkg/blockstore/remote/{s3,memory}` for the AWS client bootstrap, `Config`
shape, conformance-test layout, and Localstack integration harness. Factory +
Registry (`map[string]Factory`) is **new** to this phase — no prior
equivalent exists in `pkg/blockstore/`; the existing code dispatches via
`switch storeType` in `pkg/controlplane/runtime/shares/service.go`. Phase 3
replaces that idiom with a first-class registry. Error sentinels, crypto
primitives, SHA-256 tee, and atomic tmp+rename all have concrete prior art
that the new code must mirror.

---

## File Classification

| New / Modified File | Role | Data Flow | Closest Analog | Match Quality |
|---------------------|------|-----------|----------------|---------------|
| `pkg/backup/destination/destination.go` | interface + registry | — (contract) | `pkg/blockstore/remote/remote.go` (interface) + `pkg/metadata/backup.go` (sentinels pattern) | role-match (interface pkg); registry itself new |
| `pkg/backup/destination/errors.go` | sentinels | — | `pkg/blockstore/errors.go` (top of file, `var (…)` block) | exact (same idiom) |
| `pkg/backup/destination/envelope.go` | utility (AES-256-GCM streaming codec) | streaming transform (plaintext ↔ ciphertext framed) | `internal/adapter/smb/encryption/gcm_encryptor.go` (AEAD construction) + `pkg/metadata/store/memory/backup.go` (envelope header + magic + length framing) | role-match (crypto) + framing analog |
| `pkg/backup/destination/keyref.go` | utility (scheme parser) | file-I/O / env-lookup (batch on open) | `pkg/metadata/errors/errors.go` validate helpers + ad-hoc path resolution (no direct analog — new surface) | partial (stdlib idioms only) |
| `pkg/backup/destination/hash.go` | utility (SHA-256 tee writer) | streaming transform | `pkg/metadata/store/badger/backup.go:203` `io.MultiWriter(w, crc)` | exact (swap `crc32` for `sha256.New()`) |
| `pkg/backup/destination/fs/store.go` | driver (local FS) | file-I/O (write: stream → tmp file → rename; read: open → tee-hash → decrypt → reader) | `pkg/blockstore/local/fs/flush.go` §`syncFile` (fsync pattern) + `pkg/metadata/store/memory/backup.go` (envelope-length framing) | role-match; atomic rename idiom new but obvious |
| `pkg/backup/destination/fs/store_test.go` | unit test | tmp-dir + in-process | `pkg/metadata/store/memory/backup_test.go` + `pkg/blockstore/local/fs/fs_test.go` | exact |
| `pkg/backup/destination/s3/store.go` | driver (S3) | streaming multipart upload + range-get download | `pkg/blockstore/remote/s3/store.go` (client + Config + options) + `pkg/controlplane/runtime/blockstoreprobe/probe.go` (connectivity ping) | **exact** — copy-paste Config + `NewFromConfig` body; add `manager.Uploader` for multipart |
| `pkg/backup/destination/s3/store_test.go` | integration test | Localstack shared container | `pkg/blockstore/gc/gc_integration_test.go` (`//go:build integration`, shared helper, `TestMain`) + `pkg/blockstore/remote/s3/store_test.go` (normalizeEndpoint table test template) | exact |
| `pkg/backup/destination/storetest/suite.go` | conformance-suite helper (cross-driver — optional per planner) | plug-in via `Factory` | `pkg/blockstore/remote/remotetest/suite.go` + `pkg/metadata/storetest/backup_conformance.go` | exact |

---

## Pattern Assignments

### `pkg/backup/destination/destination.go` (interface + registry)

**Analog for interface layout:** `pkg/blockstore/remote/remote.go` — small, doc-commented, no implementation, compile-time satisfaction checks live in the driver files.

**Imports pattern** (`pkg/blockstore/remote/remote.go:3-7`):

```go
package remote

import (
	"context"

	"github.com/marmos91/dittofs/pkg/health"
)
```

**Interface-doc style** (same file, lines 9-14):

```go
// RemoteStore defines the interface for remote block storage backends.
// Blocks are immutable chunks of data stored with a string key.
//
// Key format: "{payloadID}/block-{blockIdx}"
// Example: "export/file.txt/block-0"
type RemoteStore interface {
```

**Apply:** follow the same doc-above-type convention for `Destination`, `BackupDescriptor`, `Factory`. Put compile-time satisfaction checks (`var _ destination.Destination = (*Store)(nil)`) in each driver file, **not** here.

**Factory + Registry (new surface — no direct prior art)** — implement minimally:

```go
// Factory constructs a Destination from a parsed driver-specific config.
// Drivers register into Registry via init() in their own package.
type Factory func(ctx context.Context, cfg *models.BackupRepo) (Destination, error)

// Registry maps backup_repos.kind → Factory.
// Drivers register from their own init() to avoid import cycles here.
var Registry = map[string]Factory{}

// Register adds a Factory for the given kind. Called from driver init().
// Panics on duplicate registration — programmer error.
func Register(kind string, f Factory) {
	if _, dup := Registry[kind]; dup {
		panic("destination: duplicate factory for kind " + kind)
	}
	Registry[kind] = f
}
```

Alternative: no `Register` function, fill the map at `cmd/dfs/main.go` startup (explicit, no init-order surprises). Planner decides — lean toward **explicit registration at main** to match the existing "no magic" culture in `pkg/controlplane/runtime/shares/service.go:954-1038` which uses a plain `switch`.

**BackupRepo config parsing pattern** — already established, **must reuse** (`pkg/controlplane/models/backup.go:89-114`):

```go
func (r *BackupRepo) GetConfig() (map[string]any, error) {
    if r.ParsedConfig != nil {
        return r.ParsedConfig, nil
    }
    if r.Config == "" {
        return make(map[string]any), nil
    }
    var cfg map[string]any
    if err := json.Unmarshal([]byte(r.Config), &cfg); err != nil {
        return nil, err
    }
    r.ParsedConfig = cfg
    return cfg, nil
}
```

**Apply:** `Factory` takes `*models.BackupRepo`, calls `repo.GetConfig()`, then unmarshals the `map[string]any` into a driver-private typed `Config` struct. Exactly mirrors `pkg/controlplane/runtime/shares/service.go:986-1038` `CreateRemoteStoreFromConfig`.

---

### `pkg/backup/destination/errors.go` (sentinels)

**Analog:** `pkg/blockstore/errors.go` (the `var (…)` block at lines 11-148).

**Existing sentinel pattern** (`pkg/blockstore/errors.go:11-27`):

```go
// Standard block store errors. Protocol handlers should check for these errors
// and map them to appropriate protocol-specific error codes.
var (
	// ErrContentNotFound indicates the requested content does not exist.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNoEnt (2)
	//   - SMB: STATUS_OBJECT_NAME_NOT_FOUND
	//   - HTTP: 404 Not Found
	ErrContentNotFound = errors.New("content not found")
```

**Apply** — keep the same `errors.New` + doc-comment-per-sentinel shape. D-07's full list:

```go
package destination

import "errors"

// Transient / retryable. Orchestrator (Phase 4 scheduler, Phase 5 restore
// trigger) is the single place where retry is implemented.
var (
	ErrDestinationUnavailable = errors.New("destination unavailable")
	ErrDestinationThrottled   = errors.New("destination throttled")
)

// Permanent / do-not-retry.
var (
	ErrIncompatibleConfig   = errors.New("incompatible destination config")
	ErrPermissionDenied     = errors.New("permission denied")
	ErrDuplicateBackupID    = errors.New("duplicate backup id")
	ErrSHA256Mismatch       = errors.New("sha256 mismatch on read-back")
	ErrManifestMissing      = errors.New("manifest.yaml missing for backup id")
	ErrEncryptionKeyMissing = errors.New("encryption key not resolvable")
	ErrInvalidKeyMaterial   = errors.New("invalid key material (not 32 bytes)")
	ErrDecryptFailed        = errors.New("decrypt failed (wrong key, tampered, or truncated)")
	ErrIncompleteBackup     = errors.New("incomplete backup (payload present, manifest absent)")
)
```

**Do NOT use `fmt.Errorf` for sentinel identity.** Wrap with `fmt.Errorf("...: %w", err)` at **call sites** — matches Phase 2 idiom (`pkg/metadata/store/badger/backup.go:196` `fmt.Errorf("%w: write header: %v", metadata.ErrBackupAborted, err)`).

---

### `pkg/backup/destination/envelope.go` (AES-256-GCM streaming codec per D-05)

**Primary analog:** `internal/adapter/smb/encryption/gcm_encryptor.go` — the **correct** stdlib-GCM construction idiom in this repo.

**Imports + construction pattern** (`internal/adapter/smb/encryption/gcm_encryptor.go:3-34`):

```go
import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "fmt"
)

func NewGCMEncryptor(key []byte) (*aeadEncryptor, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("create AES cipher: %w", err)
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("create GCM: %w", err)
    }
    return &aeadEncryptor{aead: gcm}, nil
}
```

**Framing / envelope-header analog:** `pkg/metadata/store/memory/backup.go:18-46` — the existing 20-byte envelope (`MDFS` magic + format version + payload length + CRC) shows exactly the structural style D-05 calls for. Phase 3 adds per-frame nonce+ciphertext+tag rather than a single envelope, but the **magic + version + reject-on-mismatch** layer is identical.

```go
// From pkg/metadata/store/memory/backup.go:35-39
const (
	memoryBackupMagic          uint32 = 0x4d444653 // "MDFS"
	memoryBackupEnvelopeHeader        = 4 + 4 + 8 + 4
)
```

**Apply D-05 wire format** — one `io.Writer`-shaped streaming encrypter + one `io.Reader`-shaped streaming decrypter. Suggested skeleton:

```go
const (
    envelopeMagic        = uint32(0x44465331) // "DFS1"
    envelopeVersion      = uint8(1)
    defaultFrameSize     = 4 * 1024 * 1024 // 4 MiB, D-05
    gcmNonceSize         = 12              // cipher.NewGCM default
    gcmTagSize           = 16
)

// aadData / aadFinal — per-frame AAD prefix (D-05 reorder+truncation resistance)
var (
    aadDataTag  = []byte("data")
    aadFinalTag = []byte("final")
)

// encryptWriter writes the envelope header then chunks plaintext into
// D-05 frames. Close() MUST be called — it emits the final `final`-tagged
// frame which the reader requires for truncation-resistance.
type encryptWriter struct {
    w         io.Writer
    aead      cipher.AEAD
    buf       []byte // plaintext accumulator, capped at frameSize
    counter   uint64
    frameSize int
    err       error // sticky
}

func newEncryptWriter(w io.Writer, key []byte, frameSize int) (*encryptWriter, error) { ... }
func (e *encryptWriter) Write(p []byte) (int, error) { ... }
func (e *encryptWriter) Close() error { /* emit final-tagged frame */ }

// decryptReader is the inverse: parses header, reads one frame at a time,
// returns ErrDecryptFailed on any tag mismatch or EOF without `final`.
type decryptReader struct { ... }
```

**Key zeroing** (D-09) — after `cipher.NewGCM(block)` returns, scrub the raw 32 bytes:

```go
key := resolveKey(ref) // from keyref.go
block, err := aes.NewCipher(key)
if err != nil { /* zero + return */ }
gcm, err := cipher.NewGCM(block)
for i := range key { key[i] = 0 } // defense-in-depth
if err != nil { return nil, err }
```

**Reuse decision:** do **not** import `internal/adapter/smb/encryption` — it is in `internal/` (not importable cross-module-root by convention) and has SMB-specific semantics (nonce from a counter). Phase 3's envelope generates random 12-byte nonces per frame and is independent.

---

### `pkg/backup/destination/keyref.go` (scheme parser per D-08 + D-09)

**No direct prior art in the repo** — this is a new utility surface. Follow the stdlib-only idiom of `pkg/metadata/store/memory/backup.go` (no external deps, explicit error wrapping).

**Apply** — the two loaders:

```go
package destination

import (
    "encoding/hex"
    "fmt"
    "os"
    "regexp"
    "strings"
)

var envVarNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ResolveKey parses ref (scheme:target) and returns the raw 32-byte key.
// Caller MUST zero the returned slice after cipher.NewGCM constructs the AEAD.
func ResolveKey(ref string) ([]byte, error) {
    scheme, target, ok := strings.Cut(ref, ":")
    if !ok || scheme == "" || target == "" {
        return nil, fmt.Errorf("%w: key ref must be scheme:target", ErrIncompatibleConfig)
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

func resolveEnvKey(name string) ([]byte, error) {
    if !envVarNamePattern.MatchString(name) {
        return nil, fmt.Errorf("%w: env var name %q not [A-Z_][A-Z0-9_]*", ErrIncompatibleConfig, name)
    }
    raw := strings.TrimSpace(os.Getenv(name))
    if raw == "" {
        return nil, fmt.Errorf("%w: env var %s", ErrEncryptionKeyMissing, name)
    }
    if len(raw) != 64 {
        return nil, fmt.Errorf("%w: env var %s must be 64 hex chars, got %d", ErrInvalidKeyMaterial, name, len(raw))
    }
    key, err := hex.DecodeString(raw)
    if err != nil {
        return nil, fmt.Errorf("%w: hex decode: %v", ErrInvalidKeyMaterial, err)
    }
    return key, nil
}

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
    if info.Size() != 32 {
        return nil, fmt.Errorf("%w: %s must be exactly 32 bytes, got %d", ErrInvalidKeyMaterial, path, info.Size())
    }
    key, err := os.ReadFile(path) //nolint:gosec // path validated above
    if err != nil {
        return nil, fmt.Errorf("%w: read %s: %v", ErrEncryptionKeyMissing, path, err)
    }
    if len(key) != 32 {
        return nil, fmt.Errorf("%w: %s read short (%d of 32)", ErrInvalidKeyMaterial, path, len(key))
    }
    return key, nil
}

// ValidateKeyRef performs the same scheme/format checks as ResolveKey
// but does NOT load the key material. Used by ValidateConfig at repo create.
func ValidateKeyRef(ref string) error { ... }
```

**Error-wrap pattern** mirrors `pkg/metadata/store/badger/backup.go:196` `fmt.Errorf("%w: …: %v", sentinel, cause)` — wrap with sentinel via `%w`, keep concrete cause via `%v`.

---

### `pkg/backup/destination/hash.go` (SHA-256 tee writer)

**Analog:** `pkg/metadata/store/badger/backup.go:200-203`.

```go
// CRC covers every byte between header and trailer. We feed the hasher
// in parallel with the writer so the CRC stays cheap (no second pass).
crc := crc32.NewIEEE()
crcw := io.MultiWriter(w, crc)
```

**Apply — direct swap of `crc32` for `crypto/sha256`:**

```go
package destination

import (
    "crypto/sha256"
    "encoding/hex"
    "hash"
    "io"
)

// hashTeeWriter wraps an underlying writer with a SHA-256 hasher.
// Everything written to it passes through to w and is hashed in parallel.
// Sum() returns the hex-encoded digest — matches manifest.SHA256 field format.
type hashTeeWriter struct {
    dst io.Writer
    h   hash.Hash
    mw  io.Writer
    n   int64
}

func newHashTeeWriter(dst io.Writer) *hashTeeWriter {
    h := sha256.New()
    return &hashTeeWriter{dst: dst, h: h, mw: io.MultiWriter(dst, h)}
}

func (t *hashTeeWriter) Write(p []byte) (int, error) {
    n, err := t.mw.Write(p)
    t.n += int64(n)
    return n, err
}

func (t *hashTeeWriter) Sum() string   { return hex.EncodeToString(t.h.Sum(nil)) }
func (t *hashTeeWriter) Size() int64   { return t.n }
```

D-04 explicit invariant: SHA-256 is computed **over ciphertext** (what's written to storage), so the tee wrapper sits **outside** the GCM encrypter in the write path. Ordering chain (D-04 write path, already locked):

```
engine stream  →  encryptWriter  →  hashTeeWriter  →  destination.Put
                                   ↑ computes manifest.sha256
```

---

### `pkg/backup/destination/fs/store.go` (local FS driver per D-03, D-14)

**Primary analog (factory + Config parsing):** `pkg/controlplane/runtime/shares/service.go:961-975` `case "fs":` block — shows the `GetConfig() → path extraction → pathutil.ExpandPath → MkdirAll → New()` chain.

**Secondary analog (fsync):** `pkg/blockstore/local/fs/flush.go:204-214`.

```go
// From pkg/blockstore/local/fs/flush.go:204-214
// syncFile opens a file and calls fsync on it.
func syncFile(path string) error {
    f, err := os.OpenFile(path, os.O_RDONLY, 0)
    if err != nil {
        return err
    }
    err = f.Sync()
    _ = f.Close()
    return err
}
```

**Apply** — driver skeleton:

```go
package fs

import (
    "context"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"

    "github.com/marmos91/dittofs/pkg/backup/destination"
    "github.com/marmos91/dittofs/pkg/backup/manifest"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Compile-time interface satisfaction — mirror pkg/blockstore/remote/s3/store.go:33
var _ destination.Destination = (*Store)(nil)

// Config holds the parsed JSON config from BackupRepo.Config.
type Config struct {
    Path         string `json:"path"`
    GraceWindow  string `json:"grace_window,omitempty"` // e.g. "24h" (D-06)
}

// Store is a local-filesystem implementation of Destination (D-03, D-14).
type Store struct {
    root         string
    graceWindow  time.Duration
}

// New constructs a Store. Called from destination.Registry["local"].
func New(ctx context.Context, repo *models.BackupRepo) (*Store, error) {
    raw, err := repo.GetConfig()
    if err != nil { return nil, err }
    // decode map → Config
    path, _ := raw["path"].(string)
    if path == "" {
        return nil, fmt.Errorf("%w: path is required", destination.ErrIncompatibleConfig)
    }
    ...
    s := &Store{root: path, graceWindow: gw}
    if err := s.sweepOrphans(ctx); err != nil { /* log, don't fail */ }
    return s, nil
}
```

**D-03 atomic publish chain:**

```
// Write both files under <id>.tmp/ (0700)
tmpDir := filepath.Join(s.root, id + ".tmp")
if err := os.MkdirAll(tmpDir, 0700); err != nil { return err }

// 1. Write payload.bin (0600)
payloadPath := filepath.Join(tmpDir, "payload.bin")
pf, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
// ... stream through encrypt→hash→file, then:
if err := pf.Sync(); err != nil { ... }
if err := pf.Close(); err != nil { ... }

// 2. Populate m.SHA256 / m.SizeBytes (from hashTeeWriter), write manifest.yaml (0600)
mf, err := os.OpenFile(filepath.Join(tmpDir, "manifest.yaml"),
    os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
if _, err := m.WriteTo(mf); err != nil { ... }
if err := mf.Sync(); err != nil { ... }
_ = mf.Close()

// 3. fsync the tmp DIR (so the directory entries hit disk)
if dir, err := os.Open(tmpDir); err == nil {
    _ = dir.Sync()
    _ = dir.Close()
}

// 4. Atomic rename — the publish marker
finalDir := filepath.Join(s.root, id)
if err := os.Rename(tmpDir, finalDir); err != nil {
    return fmt.Errorf("atomic rename: %w", err)
}
// Optionally fsync s.root here too for strict durability.
```

**D-14 ValidateConfig** — follow the style of `pkg/controlplane/runtime/blockstoreprobe/probe.go:147-206` (read-only probe, return descriptive error):

```go
func (s *Store) ValidateConfig(ctx context.Context) error {
    info, err := os.Stat(s.root)
    if err != nil {
        return fmt.Errorf("%w: stat %s: %v", destination.ErrIncompatibleConfig, s.root, err)
    }
    if !info.IsDir() {
        return fmt.Errorf("%w: %s is not a directory", destination.ErrIncompatibleConfig, s.root)
    }
    // Linux: check /proc/mounts for nfs/cifs/fuse.* parent → warn not reject
    if fstype := detectFilesystemType(s.root); isRemoteFS(fstype) {
        logger.Warn("destination/fs: repo root on remote filesystem — rename atomicity not guaranteed",
            "path", s.root, "fstype", fstype)
    }
    return nil
}
```

---

### `pkg/backup/destination/fs/store_test.go`

**Analog:** `pkg/metadata/store/memory/backup_test.go` + `pkg/blockstore/local/fs/fs_test.go` (both use `t.TempDir()` scaffolding).

**Test skeleton pattern** (typical in the repo — `t.TempDir` + constructor + round-trip):

```go
func TestFSStore_PutGet_Roundtrip(t *testing.T) {
    dir := t.TempDir()
    repo := &models.BackupRepo{ID: "r1", Kind: models.BackupRepoKindLocal}
    repo.SetConfig(map[string]any{"path": dir})

    s, err := fs.New(ctx, repo)
    require.NoError(t, err)
    defer s.Close()

    m := &manifest.Manifest{
        ManifestVersion: manifest.CurrentVersion,
        BackupID:        ulid.Make().String(),
        ...
    }
    require.NoError(t, s.PutBackup(ctx, m, bytes.NewReader(payload)))
    // Verify files exist, 0600/0700 perms, manifest-last ordering
    // Round-trip GetBackup and compare bytes
}
```

Add dedicated tests for:
1. Crash-simulation (write manifest but skip rename → assert `List` treats as incomplete)
2. Encryption round-trip (set `m.Encryption.Enabled = true`, point key to `t.TempDir()/key`)
3. Permission checks (0600 files / 0700 dirs — use `os.Stat` + `Mode().Perm()`)
4. SHA-256-on-readback mismatch (mutate payload.bin, expect `ErrSHA256Mismatch` from `GetBackup` reader)
5. Orphan sweep on `New`

---

### `pkg/backup/destination/s3/store.go` (S3 driver per D-02, D-06, D-13)

**Primary analog:** `pkg/blockstore/remote/s3/store.go` — **copy-paste the AWS client bootstrap body**. This is the single most important reuse in the phase.

**Imports to copy verbatim** (`pkg/blockstore/remote/s3/store.go:4-26`):

```go
import (
    "bytes"
    "context"
    "crypto/tls"
    "errors"
    "fmt"
    "io"
    "net"
    "net/http"
    "strings"
    "sync"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/aws/retry"
    awsconfig "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/aws/aws-sdk-go-v2/service/s3/types"
)
```

**Add for Phase 3:** `"github.com/aws/aws-sdk-go-v2/feature/s3/manager"` for multipart uploads (D-02) — the STACK.md confirms it's transitively available.

**Config struct — mirror field-for-field from D-12:**

```go
// Config fields mirror pkg/blockstore/remote/s3.Config so operators
// copy-paste between block-store and backup-repo configs.
type Config struct {
    Bucket         string `json:"bucket"`
    Region         string `json:"region,omitempty"`
    Endpoint       string `json:"endpoint,omitempty"`
    AccessKey      string `json:"access_key,omitempty"`
    SecretKey      string `json:"secret_key,omitempty"`
    Prefix         string `json:"prefix,omitempty"`          // note: 'prefix' (D-12) not 'key_prefix'
    ForcePathStyle bool   `json:"force_path_style,omitempty"`
    MaxRetries     int    `json:"max_retries,omitempty"`
    GraceWindow    string `json:"grace_window,omitempty"`    // D-06, e.g. "24h"
}
```

**NewFromConfig pattern** (`pkg/blockstore/remote/s3/store.go:83-164`) — **copy-paste the whole function body**, including the HTTP transport tuning, retry policy, endpoint normalization, and path-style override. Only substitutions:
- Return type becomes `*Store` of the new package
- Struct field names match `Config` above (rename `KeyPrefix` → `Prefix` per D-12)
- Add `manager.Uploader` construction at the bottom:

```go
client := s3.NewFromConfig(awsCfg, s3Opts...)

uploader := manager.NewUploader(client, func(u *manager.Uploader) {
    u.PartSize = 5 * 1024 * 1024 // 5 MiB, SDK default (D-02 discretion)
    u.Concurrency = 5
})

return &Store{
    client:   client,
    uploader: uploader,
    bucket:   config.Bucket,
    prefix:   config.Prefix,
    graceWindow: parseGrace(config.GraceWindow),
}, nil
```

**Reuse `normalizeEndpoint` + `isNotFoundError`:** these are private helpers in `pkg/blockstore/remote/s3`. Either (a) duplicate (cheap, ~30 LoC each) or (b) factor into `internal/awsclient/` as the research SUMMARY suggests. **Planner picks.** Phase 2's 02-PATTERNS.md precedent leans toward duplication over premature refactor.

**PutBackup implementation skeleton (D-02 two-phase commit):**

```go
func (s *Store) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
    id := m.BackupID
    payloadKey  := s.fullKey(id + "/payload.bin")
    manifestKey := s.fullKey(id + "/manifest.yaml")

    // Pipe: encrypt → hash-tee → SDK manager.Uploader (streaming multipart).
    pr, pw := io.Pipe()
    tee := newHashTeeWriter(pw)
    encW, err := newEncryptWriter(tee, key, defaultFrameSize) // or passthrough if m.Encryption.Enabled == false

    uploadErr := make(chan error, 1)
    go func() {
        defer close(uploadErr)
        _, err := io.Copy(encW, payload)
        if err == nil {
            err = encW.Close() // emit final-tagged frame
        }
        _ = pw.CloseWithError(err)
        uploadErr <- err
    }()

    // 1. Upload payload.bin (streaming, possibly GB-scale)
    _, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(payloadKey),
        Body:   pr,
    })
    if err != nil {
        return classifyS3Error(err) // → ErrDestinationUnavailable | ErrPermissionDenied | ...
    }
    if err := <-uploadErr; err != nil { return err }

    // 2. Fill manifest, upload (small, atomic). This is the PUBLISH MARKER.
    m.SHA256    = tee.Sum()
    m.SizeBytes = tee.Size()
    data, err := m.Marshal()
    if err != nil { return err }
    _, err = s.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(manifestKey),
        Body:   bytes.NewReader(data),
    })
    return err
}
```

**ValidateConfig (D-06 + D-13)** — mirrors `pkg/controlplane/runtime/blockstoreprobe/probe.go:147-206` plus two extra checks:

1. `HeadBucket` ping (already shown in `s3/store.go:436-449`)
2. Bucket-lifecycle rule warning: `GetBucketLifecycleConfiguration` → look for `AbortIncompleteMultipartUpload`; emit warning (not error) if missing.
3. Prefix-collision hard-reject: query `BlockStoreConfigStore.ListBlockStores(ctx, models.BlockStoreKindRemote)`, for each remote with `type='s3'` and matching bucket, compute `strings.HasPrefix(a, b) || strings.HasPrefix(b, a)` over (normalized) prefixes. Reject via `ErrIncompatibleConfig`.

**Store takes a narrow store interface** — not the composite `Store`. Define locally:

```go
type blockStoreLister interface {
    ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error)
}
```

This matches the "narrowest interface" culture noted in `02-PATTERNS.md` and `pkg/controlplane/store/interface.go:9-12`.

---

### `pkg/backup/destination/s3/store_test.go`

**Analog pair (both needed):**
1. `pkg/blockstore/remote/s3/store_test.go` — the **tiny** unit-test template (just `TestNormalizeEndpoint` table test)
2. `pkg/blockstore/gc/gc_integration_test.go` — the **full** Localstack shared-container harness

**Build-tag + shared container pattern** (`pkg/blockstore/gc/gc_integration_test.go:1-94`):

```go
//go:build integration

package s3

import (
    "context"
    "fmt"
    "log"
    "os"
    "testing"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    awsconfig "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/testcontainers/testcontainers-go"
    "github.com/testcontainers/testcontainers-go/wait"
)

var sharedHelper *localstackHelper

func TestMain(m *testing.M) {
    cleanup := startSharedLocalstack()
    code := m.Run()
    cleanup()
    os.Exit(code)
}
```

**MEMORY.md reminder (global):** do NOT per-test containers — always use the shared-container pattern. This is a repo-wide rule; violating it causes `TestCollectGarbage_S3`-style flake (exit 245 from container contention).

**External Localstack opt-out** (`pkg/blockstore/gc/gc_integration_test.go:41-46`):

```go
if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
    helper := &localstackHelper{endpoint: endpoint}
    helper.initClient()
    sharedHelper = helper
    return func() {}
}
```

**Apply** — Phase 3 `s3/store_test.go` creates an isolated bucket per test (`t.Name()` suffixed), runs a full `PutBackup → GetBackup` round-trip with:
- Plain payload (no encryption)
- Encrypted payload (env-var key)
- Corrupted-payload negative test (`DeleteObject` + `PutObject` with garbage → expect `ErrSHA256Mismatch` from `GetBackup` reader)
- Manifest-missing negative test (`DeleteObject` of manifest → `List` excludes, `GetBackup` returns `ErrManifestMissing`)
- Orphan-sweep test (upload payload, skip manifest, wait grace-window-short, call `New` → orphan deleted)

---

### `pkg/backup/destination/storetest/suite.go` (optional conformance helper)

**Analog:** `pkg/blockstore/remote/remotetest/suite.go` + `pkg/metadata/storetest/backup_conformance.go`.

**Factory pattern** (`pkg/blockstore/remote/remotetest/suite.go:15`):

```go
type Factory func(t *testing.T) remote.RemoteStore
```

**Apply:**

```go
package storetest

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/backup/destination"
)

// Factory constructs a Destination backed by some driver (caller-chosen).
// The t.Cleanup hook SHOULD invoke Close() and any purge steps (tmp dir,
// Localstack bucket reset).
type Factory func(t *testing.T) destination.Destination

// Run runs the full conformance suite against the destination returned
// by f. Both fs.Store and s3.Store test files can call Run(t, fFS) /
// Run(t, fS3) to get identical behavior coverage.
func Run(t *testing.T, f Factory) {
    t.Run("PutGet_Roundtrip", func(t *testing.T) { ... })
    t.Run("ManifestLastInvariant", func(t *testing.T) { ... })
    t.Run("SHA256Verify", func(t *testing.T) { ... })
    t.Run("Encryption_Roundtrip", func(t *testing.T) { ... })
    t.Run("IncompleteExcludedFromList", func(t *testing.T) { ... })
    t.Run("DeleteInverseOrder", func(t *testing.T) { ... })
}
```

This file is **planner's discretion** — 02-PATTERNS.md shipped a conformance suite; 03 can either follow suit or skip until Phase 4 exposes a second consumer.

---

## Shared Patterns

### Authentication / Key Resolution

**Source:** `pkg/backup/destination/keyref.go` (new, per D-08)
**Apply to:** Both drivers (`fs/store.go`, `s3/store.go`) — every `PutBackup` and `GetBackup` call resolves the key via `destination.ResolveKey(repo.EncryptionKeyRef)` **at the moment of the operation**, not cached. Matches D-08.

### Error Handling

**Source:** `pkg/backup/destination/errors.go` (D-07 sentinels) + wrap idiom from `pkg/metadata/store/badger/backup.go:196`
**Apply to:** All driver code — **no internal retries** beyond AWS SDK's own. Every error returns up to orchestrator. Wrap:

```go
return fmt.Errorf("%w: getobject %s/%s: %v", destination.ErrDestinationUnavailable, s.bucket, key, err)
```

### S3-Error Classification

**Source:** `pkg/blockstore/remote/s3/store.go:465-481` (`isNotFoundError`) — extend with the same pattern for other AWS error types.
**Apply to:** `pkg/backup/destination/s3/store.go` — add `classifyS3Error(err) error` that maps:
- `*types.NoSuchBucket`, `*smithy.GenericAPIError{Code:"AccessDenied"}` → `ErrPermissionDenied`
- HTTP 429, `SlowDown` → `ErrDestinationThrottled`
- HTTP 5xx, network/DNS errors → `ErrDestinationUnavailable`
- everything else → wrap with `%w` of the underlying

### Validation

**Source:** `pkg/controlplane/runtime/blockstoreprobe/probe.go:147-206` (the probe-style connectivity check)
**Apply to:** `ValidateConfig(ctx) error` on both drivers. For S3, also call `HeadBucket` + `GetBucketLifecycleConfiguration` + block-store prefix-collision query.

### Compile-Time Interface Checks

**Source:** `pkg/blockstore/remote/s3/store.go:33` `var _ remote.RemoteStore = (*Store)(nil)`
**Apply to:** `pkg/backup/destination/fs/store.go` and `pkg/backup/destination/s3/store.go` — exactly one line at top of each file:

```go
var _ destination.Destination = (*Store)(nil)
```

### AWS SDK Client Bootstrap

**Source:** `pkg/blockstore/remote/s3/store.go:83-164` `NewFromConfig`
**Apply to:** `pkg/backup/destination/s3/store.go` `New` / `NewFromConfig` — **copy the whole function body verbatim**, then add `manager.Uploader` construction at the bottom. Planner decides whether to factor into `internal/awsclient/` (SUMMARY-recommended) or duplicate (02-PATTERNS.md precedent).

### Integration Test Harness

**Source:** `pkg/blockstore/gc/gc_integration_test.go:1-130` (shared Localstack container, TestMain, external-endpoint opt-out)
**Apply to:** `pkg/backup/destination/s3/store_test.go` — copy `startSharedLocalstack`, `localstackHelper`, `initClient`, `createBucket`, and `TestMain` nearly verbatim.

### BackupRepo Config Parsing

**Source:** `pkg/controlplane/models/backup.go:89-114` (`GetConfig/SetConfig` on `BackupRepo`)
**Apply to:** Both driver `New` functions — take `*models.BackupRepo`, call `repo.GetConfig()`, unmarshal `map[string]any` into a private typed `Config`. Mirrors `pkg/controlplane/runtime/shares/service.go:986-1038`.

### Manifest Write Ordering (D-04)

**Source:** `pkg/backup/manifest/manifest.go` — already complete. Driver only populates `SHA256`, `SizeBytes`, `Encryption` at call time.
**Apply to:** Both drivers — **invariant:** manifest.yaml is always the LAST object written. The `hashTeeWriter` and `encryptWriter` feed into the payload sink; after payload close, driver fills `m.SHA256` / `m.SizeBytes` and writes manifest. This invariant is the whole point of D-11 (`PutBackup` is the single enforcement point).

---

## No Analog Found

No files without a close match. `Factory` / `Registry` at the package level is **new surface** in the repo — closest existing precedent is the `switch storeType` dispatch in `pkg/controlplane/runtime/shares/service.go:954-1038`, which is **not** a registry but the same idea. Planner may either:

1. Introduce a real `map[string]Factory` (D-11 — what CONTEXT.md says), OR
2. Keep a `switch repo.Kind` in the caller (more consistent with existing culture, less "framework").

MEMORY.md does not favor registries; existing code consistently uses `switch` dispatch. Lean toward option 2 **or** toward an explicit `Register(kind, f)` call from `cmd/dfs/main.go` so there is no init-order magic.

---

## Metadata

**Analog search scope:**
- `pkg/blockstore/remote/` (s3, memory, remotetest)
- `pkg/blockstore/local/fs/` (atomic I/O, fsync)
- `pkg/blockstore/gc/` (integration test harness)
- `pkg/blockstore/errors.go` (sentinels)
- `pkg/metadata/backup.go` + `pkg/metadata/store/{memory,badger}/backup.go` (envelope framing, tee-hash)
- `pkg/backup/manifest/manifest.go` (manifest codec — reuse as-is)
- `pkg/controlplane/models/backup.go` (config JSON convention)
- `pkg/controlplane/store/backup.go` + `interface.go` (sub-interface composition)
- `pkg/controlplane/runtime/shares/service.go` (store-factory `switch` dispatch)
- `pkg/controlplane/runtime/blockstoreprobe/probe.go` (probe-style ValidateConfig)
- `internal/adapter/smb/encryption/gcm_encryptor.go` (AES-GCM stdlib idiom)

**Files scanned:** ~25

**Pattern extraction date:** 2026-04-16

---

## Structured Return

**Phase:** 3 - Destination Drivers + Encryption
**Files classified:** 10
**Analogs found:** 10 / 10

### Coverage
- Files with exact analog: 7 (`errors.go`, `hash.go`, `fs/store_test.go`, `s3/store.go`, `s3/store_test.go`, `storetest/suite.go`, `destination.go` interface style)
- Files with role-match analog: 3 (`envelope.go`, `fs/store.go`, `destination.go` registry surface)
- Files with no analog: 0 (registry is a new surface but has prior art via `switch` dispatch)

### Key Patterns Identified
- **S3 driver is a near-copy-paste** of `pkg/blockstore/remote/s3/store.go` — Config struct, `NewFromConfig`, `normalizeEndpoint`, `isNotFoundError` all transfer verbatim; only additions are `manager.Uploader` (multipart) and D-06 orphan sweep.
- **SHA-256 tee is a one-line swap** from `pkg/metadata/store/badger/backup.go:203` (`io.MultiWriter(w, crc)` → `io.MultiWriter(w, sha256.New())`).
- **AES-256-GCM construction** mirrors `internal/adapter/smb/encryption/gcm_encryptor.go` (same two-step `aes.NewCipher` → `cipher.NewGCM`); D-05's frame format is **new** but structurally parallels `pkg/metadata/store/memory/backup.go`'s 20-byte envelope.
- **Sentinel error style** is settled: top-of-file `var (…)` block, `errors.New(...)`, per-error doc comment. Matches `pkg/blockstore/errors.go` and `pkg/metadata/backup.go`. Never `fmt.Errorf` for identity; wrap at call sites with `fmt.Errorf("%w: …: %v", sentinel, cause)`.
- **Localstack harness for integration tests** must use the shared-container pattern from `pkg/blockstore/gc/gc_integration_test.go` — per-test containers are a MEMORY.md "don't" rule.
- **BackupRepo Config parsing** reuses `models.BackupRepo.GetConfig() map[string]any` — drivers unmarshal that into their typed `Config`. Mirrors `CreateRemoteStoreFromConfig` in `shares/service.go`.
- **Narrow-interface culture** — S3 driver's prefix-collision check takes `blockStoreLister` (a two-method interface), not the composite `Store`. Matches the interface-composition culture in `pkg/controlplane/store/interface.go`.

### File Created
`.planning/phases/03-destination-drivers-encryption/03-PATTERNS.md`

### Ready for Planning
Pattern mapping complete. Planner can now reference exact line-numbered analogs in PLAN.md files — no open questions on "what does the repo do for X?" remain.
