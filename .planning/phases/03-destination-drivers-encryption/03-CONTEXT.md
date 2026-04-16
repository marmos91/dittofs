# Phase 3: Destination Drivers + Encryption - Context

**Gathered:** 2026-04-16
**Status:** Ready for planning
**Requirements covered:** DRV-01, DRV-02, DRV-03, DRV-04

<domain>
## Phase Boundary

Take the cleartext payload stream + `PayloadIDSet` that Phase 2 engines produce,
publish it (together with a manifest v1) to a local-filesystem or S3 destination
with:

- **DRV-01** atomic completion on local FS (tmp+rename)
- **DRV-02** two-phase commit on S3 (payload first, manifest last) reusing AWS
  client plumbing from `pkg/blockstore/remote/s3`
- **DRV-03** optional operator-supplied AES-256-GCM encryption of the payload
- **DRV-04** SHA-256 over the written payload, recorded in the manifest

**Out of scope for this phase:**
- Per-engine backup drivers (Phase 2 — complete)
- Manifest v1 codec (Phase 1 — `pkg/backup/manifest/manifest.go`, complete)
- Scheduler, per-repo cron, jitter, overlap guard (Phase 4)
- Retention (count / age / pin) — Phase 4 is the single caller of `Delete`
- Restore orchestration, quiesce/swap/resume, share-disable (Phase 5)
- CLI + REST + apiclient (Phase 6)
- Block-store GC hold (Phase 5 consumes the manifest's `payload_id_set`)
- External KMS, SSE-KMS pass-through, passphrase+KDF (deferred milestone)
- Incremental backups, cross-engine restore (deferred — INCR-01, XENG-01)

</domain>

<decisions>
## Implementation Decisions

### D-01 — Destination layout: two-file per backup (manifest.yaml + payload.bin)

Each backup is a directory/prefix under the repo containing two sibling objects:

```
<repo-root>/<backup-id>/
  ├─ payload.bin     (written first, may be encrypted ciphertext)
  └─ manifest.yaml   (written last — publish marker, always plaintext YAML)
```

- `<backup-id>` is ULID (already locked: `BackupRecord.ID`), giving
  lexicographic = chronological ordering at the destination.
- **Restore pre-flight reads `manifest.yaml` only** — cheap integrity check
  (validate `manifest_version`, `store_id`, `store_kind`, `schema_version`)
  without downloading potentially-huge `payload.bin`.
- Multiple backups in the same repo are separate immutable `<id>/` siblings.
  No overwrites, no merging, no block-level dedup (INCR-01 deferred).
- Listing `<repo-root>/` gives chronological history directly; `manifest.yaml`
  presence is the single "this backup is complete" signal.

### D-02 — S3 atomic publish: upload in place, manifest-last is the publish marker

No `pending/` → `final/` staging copy. The driver uploads directly to final
keys:

1. `multipart_upload(<prefix>/<id>/payload.bin)` — streaming, possibly GB-scale
2. `put_object(<prefix>/<id>/manifest.yaml)` — small, atomic

On crash between (1) and (2): `<id>/payload.bin` exists without
`manifest.yaml`. Listing treats such `<id>/` as **incomplete** and
excludes it from the restore candidate set. Orphan cleanup follows D-06.

Rationale: simpler (one multipart per file, no server-side COPY storage
bandwidth bill), matches restic/kopia precedent, and the manifest-last
invariant is enforced in one place (`Destination.PutBackup`).

### D-03 — Local FS atomic publish: directory rename

```
<repo-root>/<backup-id>.tmp/
  ├─ payload.bin     (mode 0600)
  └─ manifest.yaml   (mode 0600)

os.Rename(<backup-id>.tmp → <backup-id>)   // atomic on same FS
```

Write both files under `<id>.tmp/` (mode 0700), `fsync` each file + the tmp
directory, then `os.Rename` the directory. Same-filesystem guarantee is the
atomicity invariant; ValidateConfig warns if the repo root sits on
NFS/SMB/FUSE where rename semantics differ.

### D-04 — Encryption envelope: payload-only AES-256-GCM, manifest stays plaintext

- `manifest.yaml` is always plaintext YAML (so restore pre-flight can validate
  `store_kind`/`store_id`/`schema_version` without needing the key, and the
  manifest-last publish marker is always readable).
- `payload.bin` is AES-256-GCM ciphertext when `encryption_enabled = true`.
- SHA-256 in the manifest is computed **over the ciphertext bytes** actually
  written to storage — catches storage tamper / bit-rot without needing the
  key. GCM tag (per-frame, see D-05) catches tamper-with-valid-storage on
  decrypt. Defense in depth.
- Write path ordering:

  ```
  plaintext_stream  (from Phase 2 engine)
    └→ AES-256-GCM encrypt (per-frame, D-05)
         └→ SHA-256 hash-writer (tees to destination)
              └→ destination.Put(payload.bin)
  → manifest.yaml { sha256: <over ciphertext>, encryption: {...} }
  → destination.Put(manifest.yaml)   // plaintext, manifest-last
  ```

- Read/restore ordering:

  ```
  destination.Get(manifest.yaml)   // no key needed
  → validate manifest_version, store_kind, store_id
  destination.Get(payload.bin)
  → SHA-256 verify against manifest.sha256   // no key needed
  → AES-256-GCM decrypt   // key required; GCM tag auths plaintext
  → engine.Restore(plaintext_stream)
  ```

Matches restic / borg / kopia convention.

### D-05 — AES-256-GCM streaming format: 4 MiB chunked frames with per-frame nonce+tag

Streaming AES-GCM must be chunked — GCM's 64 GB-per-nonce limit and the lack of
truncation resistance make single-nonce-whole-stream unsafe. Payload wire
format:

```
[magic 'DFS1' | version u8 | frame_size u32]            // header, 9 bytes
[nonce 12B | ct_len u32 | ciphertext | tag 16B]         // frame 0, aad = counter=0, 'data'
[nonce 12B | ct_len u32 | ciphertext | tag 16B]         // frame 1, aad = counter=1, 'data'
...
[nonce 12B | ct_len u32 | ciphertext | tag 16B]         // frame N, aad = counter=N, 'final'
```

- Default `frame_size = 4 MiB` (~0.8% ciphertext overhead).
- Frame counter included in AAD → reorder-resistant.
- Last frame's AAD is tagged `final` → truncation-resistant (streaming reader
  that hits EOF without a `final`-tagged frame returns `ErrDecryptFailed`).
- Streaming decrypt: one frame in memory at a time; suitable for 100+ GB
  backups.
- Unencrypted payloads skip the envelope entirely — `payload.bin` is the raw
  engine stream (no header, no frames). `encryption.enabled` in the manifest
  is the single discriminator; drivers do not guess from bytes.

### D-06 — Orphan / pending cleanup: driver-init sweep + bucket lifecycle rule

Belt-and-suspenders, no single point of failure:

1. **AWS SDK multipart lifecycle** — completes or aborts on `PutBackup` return.
   Still leaks on `kill -9` mid-upload.
2. **Driver `New()` sweep** — one-shot at startup:
   - S3: `ListObjectsV2` under `<prefix>/`; for each `<id>/`, delete when
     `payload.bin` exists, `manifest.yaml` is absent, and
     `LastModified < now - grace_window`.
     Also `ListMultipartUploads` → `AbortMultipartUpload` for parts older than
     the grace window.
   - Local FS: `readdir(<repo_root>)` → delete `<id>.tmp/` dirs older than
     grace window.
3. **S3 bucket lifecycle rule** (operator-owned, not DittoFS-owned):
   `AbortIncompleteMultipartUpload after 1 day`. `ValidateConfig` emits a
   **warning** (not an error) if this rule is missing from the bucket — some
   S3-compatibles don't support bucket lifecycle.
4. **Phase 4 retention does NOT touch orphans** — it only prunes published
   (manifest-present) backups by count/age. Orphans are the driver's problem.

- Default `grace_window = 24h`, configurable per driver Config JSON.
- Log every orphan deletion at WARN level with the backup id and size.

### D-07 — Error taxonomy: fail-fast + typed sentinels, orchestrator owns retry

New sentinels in `pkg/backup/destination/errors.go` (distinct from Phase 2's
`pkg/metadata/backup.go` sentinels — different layer):

```go
// Transient / retryable (caller may retry, best-effort classification)
ErrDestinationUnavailable  // network, 5xx, DNS
ErrDestinationThrottled    // 429, 503 SlowDown

// Permanent / do-not-retry
ErrIncompatibleConfig      // bucket missing, path not writable, prefix overlap
ErrPermissionDenied        // 403, EACCES
ErrDuplicateBackupID       // ULID collision (vanishingly rare)
ErrSHA256Mismatch          // corruption on read-back
ErrManifestMissing         // restore-time: no manifest.yaml for that id
ErrEncryptionKeyMissing    // env var unset, file missing/unreadable
ErrInvalidKeyMaterial      // key not exactly 32 bytes (raw) / 64 hex chars
ErrDecryptFailed           // wrong key, tampered, truncated (GCM tag mismatch)
ErrIncompleteBackup        // backup has payload.bin but no manifest.yaml
```

- Driver does **no internal retry loop** beyond the AWS SDK's own
  `MaxRetries: 5` on transient 5xx/429. Returns immediately on error.
- Retry semantics live in the orchestrator (Phase 4 scheduler, Phase 5
  restore): transient errors mark the `BackupJob` failed; next scheduled tick
  retries for backup, operator re-triggers for restore.
- Every attempt is visible in `backup_jobs` — no hidden internal retries
  masking observability.

### D-08 — Key reference format: scheme prefix (`env:NAME` / `file:/abs/path`)

`backup_repos.encryption_key_ref` stores a scheme-prefixed string:

```
env:DITTOFS_BACKUP_KEY_PROD
file:/etc/dittofs/keys/repo-primary.key
file:/run/secrets/backup_key              # K8s mounted secret
```

Validation at repo-create and on every backup/restore:

- Bare strings (no scheme) → reject with `ErrIncompatibleConfig`.
- `file:` path must be absolute; path must exist, be regular file,
  owner-readable. Validated at each operation (not cached).
- `env:` var name must match `[A-Z_][A-Z0-9_]*`; var must be set and non-empty
  at the moment of operation.
- Unknown schemes (e.g. `http://`, `vault:`, `kms:`) → reject.
  Forward-compatible: a future milestone may register `kms:` / `vault:` without
  schema migration.

### D-09 — Key bytes encoding: raw 32 bytes in file, 64-char hex in env var

- **File mode** (`file:/etc/dittofs/backup.key`): file contents are **exactly
  32 raw bytes**. `stat` size must be 32. No encoding, no newline. Reject
  all other lengths with `ErrInvalidKeyMaterial`.

  Operator generation: `openssl rand -out /etc/dittofs/backup.key 32 && chmod 0400 /etc/dittofs/backup.key`

- **Env mode** (`env:DITTOFS_BACKUP_KEY`): env var contents are a 64-character
  lowercase hex string (after `strings.TrimSpace`). Decoded 32 bytes → key.
  Reject malformed / wrong-length with `ErrInvalidKeyMaterial`.

  Operator generation: `export DITTOFS_BACKUP_KEY=$(openssl rand -hex 32)`

- Decoded key is held in a `[]byte`, passed to `cipher.NewGCM`, and
  **zeroed** (`crypto/subtle.ConstantTimeCopy` or manual `for i := range k { k[i] = 0 }`)
  immediately after the GCM cipher is constructed. Defense in depth —
  minimizes time-in-memory.

- **No KDF, no passphrase, no key wrapping.** Operator supplies the raw AES-256
  key material. This matches the research SUMMARY explicit rejection of
  `filippo.io/age` / passphrase+scrypt as unnecessary for v0.13.0.

### D-10 — Key rotation: new backups use new key, old backups carry their own key_ref

- Manifest v1 records the `encryption.key_ref` used at backup time
  (already defined in `pkg/backup/manifest/manifest.go`).
- Operator can `UPDATE backup_repos SET encryption_key_ref = ?` at any time;
  new backups written after the update carry the new `key_ref` in their
  manifest; old backups' manifests still point at their original `key_ref`.
- **Restore resolves the key_ref from the backup's manifest**, not from the
  current repo config. Old backups decrypt as long as their referenced key
  still resolves (env var set, file present).
- **Operator responsibility:** while any Q1 backups are retained, the Q1
  key must remain resolvable. Documented, not enforced.
- **Not implemented in Phase 3:** re-encrypting old backups on rotation
  (requires rewriting each `payload.bin` — huge I/O, deferred), multi-key
  envelope / KEK+DEK pattern (deferred to external KMS milestone).

### D-11 — Driver contract: high-level `Destination` interface

Drivers own envelope, crypto, SHA-256 tee, and atomic publish. Callers hand
off cleartext + populated manifest metadata; driver fills in
`SHA256`/`SizeBytes` on the manifest before writing it.

```go
// pkg/backup/destination/destination.go

type Destination interface {
    // PutBackup publishes a new backup. payload is cleartext; driver handles
    // SHA-256 tee, optional AES-256-GCM encryption (per m.Encryption), and
    // atomic publish. Driver populates m.SHA256 and m.SizeBytes before writing
    // manifest.yaml. Returns after manifest-last upload completes.
    PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error

    // GetBackup returns (manifest, payload-reader). Caller is Phase 5's
    // restore orchestrator. Reader yields plaintext (post-decrypt) when
    // m.Encryption.Enabled == true. Reader verifies SHA-256 as it streams
    // and returns ErrSHA256Mismatch on close if the hash differs.
    GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error)

    // List returns chronologically-ordered descriptors of PUBLISHED backups
    // (manifest.yaml present). Source of truth when DB is inconsistent.
    List(ctx context.Context) ([]BackupDescriptor, error)

    // Stat returns metadata for one backup without fetching the payload.
    Stat(ctx context.Context, id string) (*BackupDescriptor, error)

    // Delete removes a published backup atomically (manifest first, then
    // payload — inverse of publish — so a crash mid-delete leaves the backup
    // discoverable, not half-gone). Used by Phase 4 retention only.
    Delete(ctx context.Context, id string) error

    // ValidateConfig is called at repo-create (Phase 6). Pings destination,
    // checks permissions, checks bucket lifecycle rule on S3, rejects
    // bucket/prefix overlap with registered block stores (D-12).
    ValidateConfig(ctx context.Context) error

    Close() error
}

type BackupDescriptor struct {
    ID          string    // ULID
    CreatedAt   time.Time // from manifest if readable, else object LastModified
    SizeBytes   int64     // payload.bin size
    HasManifest bool      // false = orphan/incomplete — excluded from restore
    SHA256      string    // from manifest.yaml (empty if manifest unreadable)
}

type Factory func(ctx context.Context, cfg Config) (Destination, error)

// Registry maps backup_repos.kind → factory.
var Registry = map[string]Factory{
    "local": fs.New,
    "s3":    s3.New,
}
```

- `PutBackup` is the single enforcement point for manifest-last, SHA-256-tee,
  and AES-GCM framing. Calling code (Phase 4/5 orchestrators) cannot forget
  to encrypt or forget to write the manifest last.
- `Delete` inverts the publish order (manifest removed first) so retention
  crashes leave published-but-not-fully-deleted backups, never the reverse.

### D-12 — Driver Config schema: minimal, mirrors `BlockStoreConfig` fields

`backup_repos.config` is the same JSON-blob convention as
`metadata_store_configs.config` / `block_store_configs.config`.

**Local (`kind='local'`):**

```json
{ "path": "/var/lib/dittofs/backups/store-prod" }
```

Required: `path` (absolute directory, pre-created by operator, owner-writable).

**S3 (`kind='s3'`):**

```json
{
  "bucket":           "dittofs-backups",
  "region":           "eu-west-1",
  "endpoint":         "",                    // blank = real AWS
  "access_key":       "",                    // blank = SDK default chain
  "secret_key":       "",                    // blank = SDK default chain
  "prefix":           "metadata/prod-store/",
  "force_path_style": false,
  "max_retries":      5
}
```

Field names and types mirror `pkg/blockstore/remote/s3.Config` so operators
copy-paste between block-store and backup-repo configs. Credentials optional —
blank falls back to AWS SDK default chain (IRSA / IMDS / env / `~/.aws/credentials`),
which is the K8s-standard path.

SSE-S3 / SSE-KMS pass-through and storage-class tuning deferred (KMS-01 is
out of scope per REQUIREMENTS.md).

### D-13 — Bucket/prefix collision: hard reject against registered block stores

`ValidateConfig` on S3 drivers queries the control-plane store for all
`BlockStoreConfig` rows with `kind='remote'` and `type='s3'`. For any row where
`bucket` matches the backup repo's bucket, check whether one prefix is a
prefix of the other (or they're equal). If so, reject with
`ErrIncompatibleConfig`.

```
bucket=X, block_prefix='blocks/',   backup_prefix='metadata/'   → OK
bucket=X, block_prefix='',          backup_prefix='metadata/'   → REJECT
bucket=X, block_prefix='data/',     backup_prefix='data/meta/'  → REJECT
bucket=X, block_prefix='data/meta', backup_prefix='data/'       → REJECT
bucket=Y, any                                                   → OK
```

**Why hard-reject, not warn:** block-store GC (`pkg/blockstore/gc/`) iterates
its configured prefix to find orphaned blocks. If prefixes overlap, GC could
`DeleteObject` backup payloads, silently destroying DR capability. This is
exactly PITFALL #8 (research/PITFALLS.md). Warn-only is an operator-error
footgun for a catastrophic failure mode.

Cross-bucket (different `Bucket` field) is always allowed — operators using
the same AWS account for block and backup buckets are fine as long as the
buckets themselves differ.

### D-14 — Local FS driver: 0600 files, 0700 dirs, no chown, require pre-created repo root

- `payload.bin` and `manifest.yaml` created mode `0600`.
- `<id>.tmp/` and `<id>/` directories created mode `0700`.
- No explicit `Chown` — process runs as the DittoFS service user; inherited
  ownership is what ops already configured.
- Explicit `Chmod` after `os.Create` (do not rely on `umask`).
- Auto-mkdir the per-backup `<id>.tmp/` leaf dir only. Do **not** auto-create
  the repo root directory — operator must pre-create it with correct
  owner/perms so they make a deliberate choice about parent dir, mount point,
  and reentrancy.
- `ValidateConfig`:
  - `stat(path)` → must exist, be a directory, be owner-writable, and be
    readable. Reject with `ErrIncompatibleConfig` otherwise.
  - Check the mount type at `path` (`/proc/mounts` / `getmntent` on Linux).
    If the parent filesystem is `nfs`/`nfs4`/`cifs`/`smb`/`fuse.*`, **warn
    loudly** (logged at WARN, returned as a diagnostic alongside the success
    return) — this is the reentrancy trap from research/PITFALLS.md. Do
    not hard-reject — advanced operators may legitimately want local-disk
    mounted via iSCSI/NFS for capacity reasons.
  - On macOS/Windows, skip the mount-type check (best-effort).

Security rationale:
- When encryption is **disabled**, `manifest.yaml` exposes the full filesystem
  namespace (filenames, UIDs/GIDs, ACLs, Kerberos principals via
  `payload_id_set` and engine_metadata). 0600 denies world-read.
- When encryption is **enabled**, the ciphertext `payload.bin` is
  cryptographically protected, but 0600 still denies casual access and matches
  `/etc/shadow` / restic / etcd convention.

### Claude's Discretion

- Exact internal structure of the GCM envelope (one `io.Writer` per frame vs
  buffer+encrypt per-frame) as long as the wire format in D-05 is honored.
- Whether `Destination` is implemented as two concrete types (`fs.Store`,
  `s3.Store`) sharing a private `envelope` helper, or one shared `common`
  type delegating blob ops — left to planner.
- S3 multipart chunk size (default 5 MiB SDK default, or tune to 8 MiB to
  match block.Size). Planner may default to SDK and revisit under benchmark.
- Parallelism within a single `PutBackup` (goroutines in `manager.Uploader`)
  — default 5, tune later if benchmarks justify.
- Whether `GetBackup` returns a verify-while-streaming reader or a
  verify-then-stream reader (former is safer for huge payloads; latter needs
  temp spooling). Planner picks; verify-while-streaming implied by D-11
  interface comment.
- Engine metadata round-trip via the manifest's `engine_metadata` field (Phase
  2 D-09) — drivers just pass through; nothing Phase 3 decides.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner) MUST read these before planning or implementing.**

### Phase 1 + Phase 2 lock-ins (binding contracts)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — Phase 1 context (models, manifest schema, Backupable interface)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-02-SUMMARY.md` — BackupStore sub-interface (CRUD used by Phase 6 wiring)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-03-SUMMARY.md` — manifest v1 + `Backupable` + `PayloadIDSet`
- `.planning/phases/02-per-engine-backup-drivers/02-CONTEXT.md` — Phase 2 context (per-engine cleartext stream producers, error sentinels)
- `pkg/backup/manifest/manifest.go` — manifest v1 struct; SHA-256, Encryption{Enabled, Algorithm, KeyRef}, PayloadIDSet, EngineMetadata, `CurrentVersion = 1`, `MaxManifestBytes = 1 MiB`
- `pkg/metadata/backup.go` — `Backupable` interface, `PayloadIDSet` type, Phase 2 error sentinels (`ErrBackupAborted` etc.)
- `pkg/controlplane/models/backup.go` — `BackupRepo.EncryptionEnabled`, `BackupRepo.EncryptionKeyRef`, `BackupRecord.SHA256`/`ManifestPath`/`SizeBytes`, `BackupJob`

### Project-level
- `.planning/REQUIREMENTS.md` §DRV — DRV-01..04 (this phase's requirements)
- `.planning/REQUIREMENTS.md` §Out of Scope — KMS, bundled key management deferred
- `.planning/research/SUMMARY.md` §"Phase 03: Destination Drivers + Scheduler + Encryption Hooks" — rationale for destination package separation from block-store remote, two-phase commit, encryption hook layer
- `.planning/research/PITFALLS.md` #8 — S3 partial uploads, retention-race, credential rotation, bucket-sharing (drove D-02, D-06, D-13)
- `.planning/research/PITFALLS.md` #9 — encryption key management, blast radius (drove D-04, D-08, D-09, D-10)
- `.planning/research/STACK.md` — existing deps; `crypto/aes` + `crypto/cipher` (stdlib), `aws-sdk-go-v2/service/s3/manager` (reuse), `archive/tar` (unused in D-01 two-file layout; kept for engine-internal Postgres tar in Phase 2)
- `.planning/PROJECT.md` "Key Decisions" — single-instance, no clustering constraints (relevant to orphan-sweep on startup: only one server touches the destination)

### Reuse targets (read to match existing patterns)
- `pkg/blockstore/remote/s3/store.go` — **reuse** AWS client construction (`NewFromConfig`, HTTP transport tuning, credential chain fallback); mirror `Config` field names for operator ergonomics
- `pkg/blockstore/remote/s3/store.go` §`Config` struct — the shape D-12 mirrors field-for-field
- `pkg/blockstore/gc/` — the consumer whose prefix iteration D-13 protects; read to understand why collision is catastrophic

### Control-plane integration
- `pkg/controlplane/store/backup.go` — existing `BackupStore` sub-interface (CRUD for repos/records/jobs), source for D-13's block-store-config lookup via composite `Store`
- `pkg/controlplane/runtime/` — future Phase 4/5 orchestrator lives here (do not add orchestrator code in Phase 3; drivers are called from here later)

### External (read at plan/execute time)
- Go `crypto/cipher` AEAD docs — https://pkg.go.dev/crypto/cipher#AEAD (GCM nonce size, Seal/Open semantics)
- AES-GCM nonce reuse cautions — https://crypto.stackexchange.com/q/26790 (each frame in D-05 uses a random 12-byte nonce; 2^32 frames = nonce-collision risk of 2^-64, safe in practice)
- AWS SDK Go v2 `manager.Uploader` — https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/s3/manager#Uploader (multipart, default part size 5 MiB, default concurrency 5)
- S3 `AbortIncompleteMultipartUpload` lifecycle rule — https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html
- AWS SDK Go v2 credential chain — https://aws.github.io/aws-sdk-go-v2/docs/configuring-sdk/#specifying-credentials

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets

- **`pkg/blockstore/remote/s3/store.go`:** AWS client construction, HTTP transport pool tuning, credential-chain fallback pattern, `Config` field shape. D-12 mirrors the `Config` struct field-for-field so operators can copy-paste between block and backup configs. AWS client construction should be factored into a shared helper (`internal/awsclient` per research SUMMARY, or directly imported from the `s3` package if clean). Planner decides.
- **`pkg/backup/manifest/manifest.go`:** `Manifest` struct is complete (Phase 1). Phase 3 populates `SHA256`, `SizeBytes`, `Encryption`, `PayloadIDSet`, `EngineMetadata` at `PutBackup` call sites; no new fields needed.
- **`pkg/controlplane/store/backup.go`:** `BackupStore.ListAllBlockStoreConfigs` equivalent (`ListBlockStoreConfigs`) available via composite `Store` for D-13's prefix-collision check. Driver `ValidateConfig` takes a `Store` reference (or narrower interface) to query.
- **`pkg/controlplane/models/backup.go`:** `BackupRepo.ParsedConfig map[string]any` + `SetConfig/GetConfig` helpers — existing pattern for parsing `Config` JSON into a typed driver config. Drivers define their own `Config` struct and unmarshal from the map (or from raw JSON).
- **Phase 2 error-wrapping idiom** (`fmt.Errorf("...: %w", err)` with `errors.Is`/`errors.As`): Phase 3 sentinels in D-07 must work the same way.

### Established Patterns

- **Sub-interface composition** — control-plane `Store` composes 10 sub-interfaces. D-13's block-store lookup takes a narrow interface (`BlockStoreConfigStore`), not the composite.
- **`//go:build integration` for Localstack** — S3 driver tests that need real S3 semantics use the shared-container Localstack helper (per MEMORY.md: do NOT per-test containers). Phase 3's S3 round-trip tests follow the pattern from `pkg/blockstore/remote/s3/store_test.go`.
- **Error sentinels are `errors.New`, not `fmt.Errorf`** — fixed-identity sentinels wrap with `%w`, check with `errors.Is`.
- **Factory + registry pattern** — mirrored from `pkg/blockstore/remote/` (`remote.Registry`). D-11 uses `map[string]Factory`, keyed by `backup_repos.kind`.
- **`ValidateConfig` at repo-create** — precedent: `pkg/blockstore/remote/s3.Store.HealthCheck` is a read-path ping, not a pre-flight. Phase 3 introduces a formal `ValidateConfig` — planner may choose to retrofit the pattern onto block-store too (out of this phase's scope).

### Integration Points

- **Phase 4 scheduler calls** `Destination.PutBackup(ctx, manifest, stream)` per repo tick. Scheduler owns overlap mutex, jitter, retries on transient errors (D-07).
- **Phase 5 restore calls** `Destination.GetBackup(ctx, id)` → streams plaintext into `Backupable.Restore` on a fresh engine instance. Phase 5 handles share-disable + swap + resume; drivers are oblivious.
- **Phase 5 block-GC hold reads** `manifest.PayloadIDSet` from the manifest Phase 3 wrote. Driver must pass through the `PayloadIDSet` the engine produced (Phase 2) without modification.
- **Phase 4 retention calls** `Destination.List(ctx)` and `Destination.Delete(ctx, id)`. Retention never touches orphans (D-06).
- **Phase 6 CLI `repo add`** calls `Destination.ValidateConfig(ctx)` before persisting the `backup_repos` row. Validation errors map to REST 400 / CLI exit-1 with human message.

</code_context>

<specifics>
## Specific Ideas

- **User emphasized safety-first defaults, consistent with Phase 2 philosophy.** Every gray-area choice defaulted to the more conservative option: hash-of-ciphertext over hash-of-plaintext (pre-flight integrity without key), hard-reject bucket collisions over warn-only, 0600 file perms over 0644, driver init sweep PLUS bucket lifecycle PLUS documentation.
- **"Belt and suspenders" orphan cleanup** (D-06): three independent layers each catching the same failure mode. Accepts redundancy as the cost of avoiding a silent-data-loss footgun.
- **Key rotation explicitly preserves old backups' decryptability** (D-10): the manifest records `key_ref`, restore resolves from manifest not from current config. Operator responsibility to keep old keys resolvable while old backups are retained.
- **ULID as the backup ID is the chronology backbone** (locked since Phase 1): sortable, meaningful, survives control-plane wipe. D-01 leans on this — lexicographic `ls` is the disaster-recovery fallback for listing.
- **Reentrancy trap warning** (D-14): explicitly warn but do not reject when local FS repo root is on NFS/SMB/FUSE. Some operators legitimately want local-disk-via-iSCSI/NFS for capacity; silent failure is worse than loud warning.

</specifics>

<deferred>
## Deferred Ideas

- **SSE-S3 / SSE-KMS pass-through on S3 driver** — mentioned as option; rejected from Phase 3. Defer to BlockStore Security milestone or a later hardening pass. Client-side AES-256-GCM (D-04, D-05) is sufficient for v0.13.0 confidentiality.
- **Re-encrypting existing backups on key rotation** — deferred (D-10). Requires rewriting every retained `payload.bin`; huge I/O cost; operator can achieve the same outcome by triggering fresh backups with the new key and letting retention age out old-key backups.
- **KEK/DEK envelope (multi-key encryption)** — deferred to external KMS milestone. Current model: one key per repo, stored by reference.
- **Passphrase + KDF (scrypt/argon2)** — rejected by research/SUMMARY and by D-09. Raw 32-byte key material only.
- **Configurable file/dir permissions on local FS driver** (`file_mode`, `dir_mode`, `umask` fields in Config) — deferred until an operator asks. Hardcoded 0600/0700 covers the security posture for now.
- **GCS / Azure Blob / SFTP destination drivers** — not required for v0.13.0 (research SUMMARY lists as "defer to v1.x"). Local FS + S3 covers the 99% case; S3-compatibles (MinIO, Scaleway, Wasabi, B2, R2) covered via endpoint config.
- **Backup-time compression** — manifest v1 has no compression field. Defer to future version bump. Backups of binary Postgres dumps would benefit; backups of gob-encoded memory stores less so.
- **Resumable uploads** — research SUMMARY lists as v0.14+ (requires CAS chunking). Failed multipart uploads are aborted and retried from scratch in v0.13.0.
- **Automatic test-restore / verify command** (`restic check` analog, AUTO-01 in REQUIREMENTS.md future list) — deferred. Phase 7 (testing) covers happy-path verification via integration tests.
- **Bucket-level KMS config validation** — currently not checked. Future enhancement.
- **Per-repo Prometheus metrics wiring** — deferred to Phase 5 (observability). Phase 3 exposes hooks (e.g., a `metrics.Collector` argument or no-op default) only if planner finds a clean insertion point; otherwise Phase 5 retrofits.

### Reviewed Todos (not folded)

None — no pending todos matched this phase (none surfaced by `todo match-phase 3`).

</deferred>

---

*Phase: 03-destination-drivers-encryption*
*Context gathered: 2026-04-16*
