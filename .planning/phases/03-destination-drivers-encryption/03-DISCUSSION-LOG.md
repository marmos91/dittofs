# Phase 3: Destination Drivers + Encryption - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-16
**Phase:** 03-destination-drivers-encryption
**Areas discussed:** Archive layout & atomic publish; Encryption scope & crypto ordering; Key reference resolution & key bytes format; Driver interface & config schema; Error taxonomy & retry boundary; Orphan / pending cleanup; Bucket/prefix collision enforcement; Local FS driver perms, ownership, path auto-create

---

## Area Selection

| Option | Description | Selected |
|--------|-------------|----------|
| Archive layout & atomic publish | How each backup is laid out at the destination; atomic-commit mechanics per driver | ✓ |
| Encryption scope & crypto ordering | What gets encrypted; SHA-256 vs encryption ordering | ✓ |
| Key reference resolution & key bytes format | EncryptionKeyRef format; key material encoding | ✓ |
| Driver interface & config schema | High-level Destination vs low-level BlobStore; per-driver Config schema | ✓ |

User then requested "Explore more gray areas"; selected all four follow-ups:

| Option | Description | Selected |
|--------|-------------|----------|
| Error taxonomy & retry boundary | Sentinels + who retries transient failures | ✓ |
| Orphan / pending cleanup ownership | Who sweeps payload.bin without manifest.yaml | ✓ |
| Bucket/prefix collision enforcement | ValidateConfig strictness against block-store overlap | ✓ |
| Local FS driver permissions, ownership, path auto-create | File/dir modes and mkdir semantics | ✓ |

---

## Area: Archive layout & atomic publish

### Q1 — Layout at destination

| Option | Description | Selected |
|--------|-------------|----------|
| Two-file: manifest.yaml + payload.bin | Each backup = directory/prefix with two siblings; restore pre-flight reads manifest only | ✓ |
| Single tarball (payload+manifest in one .tar) | Each backup = one .tar archive; rename-atomic on FS | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Two-file layout. Restore pre-flight reads only manifest.yaml; manifest-last publish is natural; List decides completeness by manifest presence.

### Q2 — S3 atomic-publish mechanic

| Option | Description | Selected |
|--------|-------------|----------|
| Upload in place; manifest-last is the only marker | No pending/, direct upload to final keys, manifest.yaml is publish marker | ✓ |
| Stage under pending/<id>/ then COPY to final | Server-side copy from pending to final after both upload | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Upload in place. Simpler, matches restic/kopia, one enforcement point for manifest-last. Orphan cleanup via driver init sweep + bucket lifecycle rule.

---

## Mid-discussion aside: "How do we save previous versions?"

User asked how successive backups coexist. Claude clarified:

- Each backup is a distinct `<backup-id>/` under the repo prefix, keyed by ULID (time-ordered)
- No overwrites, no in-place versioning, no block-level dedup across backups
- Retention (Phase 4) prunes old backups via keep_count / keep_age_days
- Schema versioning: `manifest_version: 1` field for forward-compat
- `backup_records` table is source of truth; destination List is DR fallback

### Follow-up — "Ok, do we have a way to list old backups chronologically?"

Claude answered at three layers:
1. Control-plane DB (primary, fastest) — `backup_records.CreatedAt` (indexed), ULID ID sortable
2. Destination key layout — `<prefix>/<ULID>/` gives chronological order via plain `ls`
3. Manifest payload — `created_at` (RFC3339) + `backup_id` (ULID) survive control-plane wipe

Phase 6 delivers `dfsctl store metadata <store> backup list` (API-03) with `-o table|json|yaml`.

Phase 3 obligations: (a) write `manifest.yaml.created_at` authoritatively; (b) `List` driver method returns `(id, created_at, size, has_manifest)` per discovered backup.

---

## Area: Encryption scope & crypto ordering

### Q1 — Encryption envelope

| Option | Description | Selected |
|--------|-------------|----------|
| Encrypt payload only; SHA-256 over ciphertext | Manifest stays plaintext; hash catches storage tamper without key | ✓ |
| Encrypt payload; SHA-256 over plaintext | Verification requires decrypt first | |
| Encrypt both payload AND manifest | Highest confidentiality; defeats manifest-last marker | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Payload-only encryption; SHA-256 over ciphertext. Manifest is always plaintext YAML so restore pre-flight validates store_kind/store_id/schema_version without the key. Matches restic/borg/kopia.

### Q2 — AES-256-GCM streaming framing

| Option | Description | Selected |
|--------|-------------|----------|
| Chunked frames: 4 MiB frames, per-frame nonce+tag | Bounded memory, truncation-resistant, reorder-resistant | ✓ |
| Single GCM with random nonce, no framing | Breaks past 64 GB, unsafe streaming | |
| Single-frame with AEAD chunking lib (age) | Well-reviewed but new dep | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Chunked 4 MiB frames with per-frame nonce+tag; frame counter in AAD; final frame tagged 'final' in AAD for truncation resistance. Wire format documented in D-05.

---

## Area: Key reference resolution & key bytes format

### Q1 — EncryptionKeyRef format

| Option | Description | Selected |
|--------|-------------|----------|
| Explicit scheme prefix: env:NAME / file:/path | Unambiguous, future-extensible to kms:/vault: | ✓ |
| Heuristic: starts with '/' = file path, else env | Shorter input; fragile on Windows, relative paths | |
| Separate kind column + ref column | Stronger DB typing; requires migration for new kinds | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Explicit scheme prefix. `env:NAME` matching `[A-Z_][A-Z0-9_]*`, `file:/absolute/path` must exist at each operation. Unknown schemes rejected.

### Q2 — Key material encoding

| Option | Description | Selected |
|--------|-------------|----------|
| Raw 32 bytes in file; hex (64 chars) in env var | Clear, testable, no ambiguity | ✓ |
| Base64-encoded in both | Copy-paste friendly; variant ambiguity risk | |
| Passphrase + scrypt/argon2 KDF | User-friendly; KDF params need versioning; user passphrases weak | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Raw 32 bytes in file (strict size check), 64-char lowercase hex in env var (TrimSpace then hex decode). Decoded key zeroed after GCM cipher construction. No KDF.

### Q3 — Key rotation semantics

| Option | Description | Selected |
|--------|-------------|----------|
| New key for new backups; old backups keep own key_ref in manifest | Manifest records key_ref used; restore resolves from manifest | ✓ |
| Reject rotation — repo encryption is immutable | Clean invariant; punishes rotation workflow | |
| Rotation triggers auto re-encrypt | Massive I/O cost | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Manifest-embedded key_ref. Operator keeps old keys resolvable while old backups retained; documented responsibility. No re-encryption, no multi-key envelope in Phase 3.

---

## Area: Driver interface & config schema

### Q1 — Driver contract shape

| Option | Description | Selected |
|--------|-------------|----------|
| High-level BackupSink: owns envelope, crypto, two-file layout | Driver is the single enforcement point; minimizes forgotten invariants | ✓ |
| Low-level BlobStore: Put/Get/List/Delete on opaque keys | More flexible; orchestrator must remember manifest-last + encrypt order | |
| Let Claude decide | Defer to researcher | |

**User's choice:** High-level `Destination` interface. Methods: PutBackup, GetBackup, List, Stat, Delete, ValidateConfig, Close. Full signature in CONTEXT.md D-11.

### Q2 — Driver Config schema

| Option | Description | Selected |
|--------|-------------|----------|
| Minimal, mirrors existing BlockStoreConfig fields | Local: {path}; S3: same shape as pkg/blockstore/remote/s3.Config | ✓ |
| Richer S3 config: include SSE-S3/SSE-KMS pass-through | Adds KMS fields; widens scope beyond v0.13.0 | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Minimal, mirrors blockstore S3 Config for operator ergonomics. SSE-KMS deferred (KMS-01 out of scope).

---

## Area: Error taxonomy & retry boundary

### Q1 — Who retries on transient failure?

| Option | Description | Selected |
|--------|-------------|----------|
| Orchestrator owns retry; driver is fail-fast with typed sentinels | AWS SDK handles transient 5xx/429 only; no driver retry loop | ✓ |
| Driver owns retry with exponential backoff | Masks transient failures from observability | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Fail-fast driver, typed sentinels. Transient: ErrDestinationUnavailable, ErrDestinationThrottled. Permanent: ErrIncompatibleConfig, ErrPermissionDenied, ErrDuplicateBackupID, ErrSHA256Mismatch, ErrManifestMissing, ErrEncryptionKeyMissing, ErrInvalidKeyMaterial, ErrDecryptFailed, ErrIncompleteBackup. Every attempt visible in backup_jobs.

---

## Area: Orphan / pending cleanup ownership

### Q1 — Who sweeps orphans?

| Option | Description | Selected |
|--------|-------------|----------|
| Driver init sweep + bucket lifecycle rule | Belt + suspenders; driver New() sweep + AWS lifecycle rule + ValidateConfig warning | ✓ |
| Phase 4 retention owns all cleanup | Single pass, but retention-disabled repos leak forever | |
| Rely entirely on bucket lifecycle rule + docs | Cheapest; S3-compatibles vary in lifecycle support | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Driver `New()` sweep with 24h grace window (configurable), plus warning from ValidateConfig when bucket lifecycle rule is missing, plus bucket lifecycle rule documented for operators. Phase 4 retention NEVER touches orphans.

---

## Area: Bucket/prefix collision enforcement

### Q1 — ValidateConfig strictness

| Option | Description | Selected |
|--------|-------------|----------|
| Hard reject same bucket + overlapping prefix with any registered block store | Prevents block-GC from deleting backup objects | ✓ |
| Warn-only: log a warning but allow the config | Warnings get ignored; silent catastrophic failure | |
| No validation — document operator responsibility | Cheapest; ignored by PITFALLS.md | |
| Let Claude decide | Defer to researcher | |

**User's choice:** Hard-reject with ErrIncompatibleConfig. Query control-plane for all BlockStoreConfig(kind=remote,type=s3). Overlap = same bucket AND (a is prefix of b OR b is prefix of a). Cross-bucket always OK.

---

## Area: Local FS driver permissions, ownership, path auto-create

### Q1 — File/dir modes and mkdir semantics

| Option | Description | Selected |
|--------|-------------|----------|
| 0600 files, 0700 dirs, no chown, auto-mkdir leaf only | Security default matches /etc/shadow, restic, etcd; operator pre-creates repo root | ✓ |
| 0644 files, 0755 dirs, auto-mkdir repo root | World-readable exposes filesystem namespace | |
| Configurable per repo (umask, file_mode, dir_mode in Config) | Widens API surface; defer until requested | |
| Let Claude decide | Defer to researcher | |

**User's choice:** 0600 files, 0700 dirs, no explicit Chown, auto-mkdir only the per-backup leaf. Operator pre-creates repo root. ValidateConfig stats mount type and warns (not rejects) if NFS/SMB/FUSE.

---

## Claude's Discretion

Items left explicitly to planner/execute time:

- Internal envelope struct (one io.Writer per frame vs buffered per-frame), as long as D-05 wire format is honored
- Whether Destination is two concrete types sharing a helper vs one shared type with driver delegation
- S3 multipart chunk size (default 5 MiB SDK default, or tune to 8 MiB); parallelism within PutBackup (default 5 goroutines)
- GetBackup verify-while-streaming vs verify-then-stream semantics (interface implies former; planner confirms)
- Engine metadata pass-through to manifest.engine_metadata (Phase 2 D-09 already requires it)

---

## Deferred Ideas

See CONTEXT.md `<deferred>` section for the complete list, including:
- SSE-S3 / SSE-KMS pass-through
- Re-encryption on key rotation
- KEK/DEK envelope (multi-key)
- Passphrase + KDF
- Configurable FS permissions
- GCS / Azure Blob / SFTP drivers
- Backup-time compression
- Resumable uploads
- Automatic test-restore command
- Per-repo Prometheus metrics wiring (deferred to Phase 5 observability)
