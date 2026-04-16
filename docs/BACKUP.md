# Backup & Restore

DittoFS ships metadata-store backups as immutable archives published to a local filesystem or an S3-compatible object store. Backups are self-describing (manifest v1), integrity-checked with SHA-256, and optionally encrypted at rest with operator-supplied AES-256-GCM keys.

**Status:** available from v0.13.0. Adding a new destination driver requires operator code changes and a server restart; existing repos and schedules persist across upgrades.

**Scope:** metadata backups only. Block data lives in object storage or the local cache and is not copied by this system. See [FAQ.md](FAQ.md) for scope rationale.

## Table of Contents

- [How backups work](#how-backups-work)
- [Destination drivers](#destination-drivers)
- [Local filesystem driver (`kind=local`)](#local-filesystem-driver-kindlocal)
- [S3 driver (`kind=s3`)](#s3-driver-kinds3)
- [Encryption at rest (AES-256-GCM)](#encryption-at-rest-aes-256-gcm)
- [Integrity check](#integrity-check)
- [Orphan cleanup](#orphan-cleanup)
- [Validation at repo-create](#validation-at-repo-create)
- [What this release does NOT include](#what-this-release-does-not-include)
- [See also](#see-also)

## How backups work

Every backup is an immutable two-file archive under a per-backup directory or key prefix:

```
<repo-root>/<backup-id>/
  ├─ payload.bin     (written first, optionally AES-256-GCM ciphertext)
  └─ manifest.yaml   (written last — always plaintext YAML, acts as the publish marker)
```

- `<backup-id>` is a ULID — sortable, time-prefixed, and stable across control-plane wipes. Lexicographic directory listing equals chronological order.
- `manifest.yaml` is intentionally small and plaintext. Restore pre-flight reads it to check `manifest_version`, `store_kind`, and `store_id` without downloading potentially multi-GiB `payload.bin`.
- `payload.bin` is the on-disk archive the backup driver emitted. When encryption is enabled it is the D-05 streaming envelope (AES-256-GCM, 4 MiB frames); when encryption is disabled it is the raw engine stream.
- Publish is two-phase: `payload.bin` first, `manifest.yaml` last. A crash between the two leaves an orphaned `payload.bin` that `List` excludes and the startup sweep removes after the grace window. The manifest-last invariant means there is exactly one observable "backup is published" event per archive.
- Multiple backups in the same repo are separate sibling directories. There is no overwriting, no merging, no incremental dedup in v0.13.0.

**Design notes:** see `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` decisions D-01 (layout) and D-04 (encryption ordering) for rationale.

## Destination drivers

DittoFS ships two built-in drivers:

| Kind   | Driver             | Typical use                                |
| ------ | ------------------ | ------------------------------------------ |
| `local`| Local filesystem   | On-host disk, iSCSI-mounted block device   |
| `s3`   | S3-compatible      | AWS S3, MinIO, Wasabi, Scaleway, B2, R2    |

Drivers are registered explicitly at server startup (no init-time magic). Adding a new driver requires a code change and a server restart; existing backup repos continue to function across driver registrations.

## Local filesystem driver (`kind=local`)

### Configuration

```json
{
  "path": "/var/lib/dittofs/backups/store-prod",
  "grace_window": "24h"
}
```

| Field          | Required | Default | Notes                                                 |
| -------------- | -------- | ------- | ----------------------------------------------------- |
| `path`         | yes      | —       | Absolute directory, pre-created by operator, owner-writable |
| `grace_window` | no       | `24h`   | Age threshold for orphan-sweep of stale `<id>.tmp/`  |

### Filesystem semantics

- Files are created mode `0600`, directories mode `0700`. The driver explicitly `chmod`s after `open` / `mkdir` to defend against a surprising process `umask`.
- The driver does **not** `chown` — the DittoFS service user owns everything it creates.
- The driver does **not** auto-create the repo root. The operator must `mkdir -p` it with the right owner and mode before registering the repo. This is deliberate: it forces an operator decision about parent directory, mount point, and reentrancy (see below).

### Reentrancy warning: never back up onto a DittoFS-served mount

If `path` lives on an NFS, SMB, or FUSE mount — **especially one served by this DittoFS instance** — the driver cannot guarantee atomic rename semantics, and you can create a reentrancy deadlock: the backup writes to the mount, which writes back through DittoFS, which blocks on its own worker pool, which blocks the backup writer, and so on.

The driver detects `nfs`, `nfs4`, `cifs`, `smb`, `smbfs`, and `fuse.*` parent filesystems on Linux and emits a WARN log at `ValidateConfig` time. It does **not** hard-reject, because advanced operators legitimately want iSCSI-mounted storage for capacity. Safe alternatives, in order of preference:

1. Local disk (`/var/lib/...`) on the same host as the DittoFS server.
2. iSCSI- or FC-mounted block device formatted with ext4 / xfs / ZFS.
3. Bind-mount of external storage into the container's filesystem.

Avoid: DittoFS-served NFS/SMB mounts, Samba mounts of the host's own shares, FUSE filesystems that layer over network transports.

## S3 driver (`kind=s3`)

### Configuration

```json
{
  "bucket": "dittofs-backups",
  "region": "eu-west-1",
  "endpoint": "",
  "access_key": "",
  "secret_key": "",
  "prefix": "metadata/prod-store/",
  "force_path_style": false,
  "max_retries": 5,
  "grace_window": "24h"
}
```

| Field              | Required | Default | Notes                                                    |
| ------------------ | -------- | ------- | -------------------------------------------------------- |
| `bucket`           | yes      | —       | S3 bucket name                                           |
| `region`           | no       | SDK     | Falls back to AWS SDK default chain                      |
| `endpoint`         | no       | ""      | Blank = real AWS; set for MinIO / Wasabi / Scaleway / B2 / R2 |
| `access_key`       | no       | ""      | Blank → SDK default credential chain (IRSA / IMDS / env) |
| `secret_key`       | no       | ""      | Must be set together with `access_key` or omit both      |
| `prefix`           | no       | ""      | Object-key prefix under the bucket                       |
| `force_path_style` | no       | false   | Set `true` for most S3-compatibles (MinIO / Scaleway)    |
| `max_retries`      | no       | 5       | AWS SDK retry cap for transient 5xx / 429                |
| `grace_window`     | no       | `24h`   | Age threshold for orphan-sweep and stale multipart abort |

In Kubernetes, leave `access_key` / `secret_key` blank and use IAM-Roles-for-Service-Accounts (IRSA) or an IMDS-provided role. Static credentials in the repo config are the least-preferred option.

### Endpoint examples

```bash
# AWS (real)
"region": "eu-west-1", "endpoint": ""

# MinIO
"endpoint": "http://minio.internal:9000", "force_path_style": true

# Scaleway Object Storage (fr-par)
"endpoint": "https://s3.fr-par.scw.cloud", "region": "fr-par"

# Backblaze B2
"endpoint": "https://s3.us-west-002.backblazeb2.com", "region": "us-west-002"

# Cloudflare R2
"endpoint": "https://<accountid>.r2.cloudflarestorage.com", "region": "auto"
```

### Bucket lifecycle: add `AbortIncompleteMultipartUpload`

When a DittoFS process dies mid-upload, AWS SDK's internal abort may not reach the broker before the process exits. The result is a stale multipart upload that silently accrues storage cost.

DittoFS has two belt-and-suspenders cleanup layers for this (startup orphan sweep + manager abort on error), but the S3 native lifecycle rule is the strongest line of defense. Add:

```xml
<LifecycleConfiguration>
  <Rule>
    <ID>abort-incomplete-mpu</ID>
    <Status>Enabled</Status>
    <AbortIncompleteMultipartUpload>
      <DaysAfterInitiation>1</DaysAfterInitiation>
    </AbortIncompleteMultipartUpload>
  </Rule>
</LifecycleConfiguration>
```

Or via AWS CLI:

```bash
cat > lifecycle.json <<'EOF'
{
  "Rules": [
    {
      "ID": "abort-incomplete-mpu",
      "Status": "Enabled",
      "Filter": {"Prefix": ""},
      "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}
    }
  ]
}
EOF

aws s3api put-bucket-lifecycle-configuration \
  --bucket dittofs-backups \
  --lifecycle-configuration file://lifecycle.json
```

`ValidateConfig` emits a WARN log if the rule is absent. It does not hard-reject — some S3-compatibles do not implement bucket lifecycle at all, and the DittoFS-side orphan sweep is sufficient for correctness.

### Bucket / prefix collision with block stores

If DittoFS is already using this bucket for remote block-store data (`pkg/blockstore/remote/s3`), the backup `prefix` MUST NOT overlap with any configured block-store `prefix` on the same bucket. Overlap is hard-rejected at repo-create with `ErrIncompatibleConfig`.

Rationale: block-store GC scans its configured prefix and issues `DeleteObject` on orphaned block keys. An overlapping prefix means GC could silently destroy your backup payloads.

| Bucket | Block `prefix` | Backup `prefix` | Allowed? |
|--------|----------------|-----------------|----------|
| A      | `blocks/`      | `metadata/`     | yes      |
| A      | `data/`        | `data/meta/`    | no — backup prefix sits under block prefix |
| A      | `data/meta/`   | `data/`         | no — block prefix sits under backup prefix |
| A      | `""` (root)    | `metadata/`     | no — root prefix matches everything        |
| A (block) | `blocks/`   | — (repo uses bucket B) | yes — different bucket                 |

Cross-bucket is always allowed. If you use the same AWS account for both block and backup storage, put them in separate buckets — the per-bucket blast radius outweighs the minor convenience of a single bucket.

## Encryption at rest (AES-256-GCM)

Encryption is opt-in per repo. Set `EncryptionEnabled=true` on the `backup_repos` row and provide `EncryptionKeyRef`.

### Key reference format

Key references use a scheme prefix — the raw key material is never stored in the DittoFS database or the manifest, only a resolvable reference:

| Scheme      | Target                | Value                                              |
| ----------- | --------------------- | -------------------------------------------------- |
| `env:NAME`  | Environment variable  | 64 lowercase hex characters (32 decoded bytes)     |
| `file:PATH` | Absolute file path    | Regular file containing exactly 32 raw bytes       |

Anything else — bare strings, `http://`, `vault:`, `kms:` — is rejected with `ErrIncompatibleConfig`. External KMS, Vault, and AWS KMS key wrapping are explicitly deferred (see "What this release does NOT include" below).

### Key generation

```bash
# Env-var form (64 hex chars, 32 decoded bytes):
export DITTOFS_BACKUP_KEY_PROD=$(openssl rand -hex 32)

# File form (32 raw bytes, owner-readable only):
openssl rand -out /etc/dittofs/keys/backup-prod.key 32
chmod 0400 /etc/dittofs/keys/backup-prod.key
chown dittofs:dittofs /etc/dittofs/keys/backup-prod.key
```

For Kubernetes, mount a Secret as a file at a stable absolute path:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-backup-key
type: Opaque
data:
  key: <base64-of-32-raw-bytes>
---
# On the pod:
volumes:
- name: backup-key
  secret:
    secretName: dittofs-backup-key
    defaultMode: 0400
volumeMounts:
- name: backup-key
  mountPath: /run/secrets/backup
  readOnly: true
# encryption_key_ref: "file:/run/secrets/backup/key"
```

### How the crypto layer is wired

- The manifest (`manifest.yaml`) is **always plaintext YAML** regardless of encryption. Restore pre-flight can validate `manifest_version`, `store_kind`, and `store_id` without the key; the manifest-last publish marker is always readable.
- Only `payload.bin` is encrypted, using AES-256-GCM with per-frame random nonces and counter-in-AAD for reorder-resistance. The final frame carries a distinct `"final"` AAD tag so truncation is detectable — a stream that hits EOF without a final-tagged frame returns `ErrDecryptFailed`.
- SHA-256 is computed over the **ciphertext** bytes actually written to storage. Operators can verify integrity without holding the key.
- The GCM authentication tag catches tamper-with-valid-storage on decrypt (e.g., a legitimate-looking object whose bytes were replaced). SHA-256 and the GCM tag together provide defense in depth.
- DittoFS loads the raw 32 bytes long enough to construct the GCM cipher, then zeroes the buffer. The raw key material never reaches disk or logs.

### Key rotation

Keys are referenced per-backup via the manifest's `encryption.key_ref` field.

1. Operator updates `backup_repos.encryption_key_ref` (via future `dfsctl repo update`, currently via direct DB update).
2. New backups written after the change record the new `key_ref` in their `manifest.yaml`.
3. Old backups' manifests continue to point at their original `key_ref`. They decrypt as long as that reference still resolves — meaning the env var is still exported, or the file still exists and is readable.
4. **Operator responsibility:** while any backup encrypted with the old key is retained, the old key must remain resolvable. DittoFS does not re-encrypt old backups on rotation — that would require rewriting every retained `payload.bin` (huge I/O cost) and is explicitly deferred.

The practical rotation playbook:

```bash
# 1. Generate a new key.
export DITTOFS_BACKUP_KEY_PROD_NEW=$(openssl rand -hex 32)

# 2. Update repo to point at new key.
UPDATE backup_repos SET encryption_key_ref = 'env:DITTOFS_BACKUP_KEY_PROD_NEW' WHERE id = ?;

# 3. Keep the old env var exported until retention has aged out every old-key backup.
# (DittoFS cannot detect this for you; track in your key-management system.)

# 4. Once no old-key backups remain (check: select distinct encryption_key_ref from
# backup_records ... join manifest.yaml), unset the old env var and destroy the
# old key material.
```

### Losing the key

If the key material is permanently lost, encrypted backups are **unrecoverable**. The manifest is still readable so you can identify which `key_ref` they reference, but the payload is cryptographically protected and cannot be recovered without the key. This is the explicit trade-off for encryption-at-rest: confidentiality cost is that losing the key equals losing the data.

## Integrity check

Every backup archive records a SHA-256 over the bytes actually written to storage (ciphertext when encrypted, plaintext when not). The read-back path verifies on `Close()`:

- Clean read: SHA-256 matches → `Close()` returns nil.
- Corrupt read: SHA-256 mismatches → `Close()` returns `ErrSHA256Mismatch`.

Phase 5 restore must always `Close()` the reader to catch corruption — the error surfaces from `Close`, not from `Read`, so truncated consumers miss it.

## Orphan cleanup

"Orphans" are backup archives whose `payload.bin` is present but whose `manifest.yaml` never made it to storage (crash between the two-phase commit steps). DittoFS cleans them up in three independent layers:

1. **AWS SDK multipart abort on error** — `manager.Uploader.Upload` aborts the active multipart when `PutBackup` returns an error, before the call completes. Catches clean crashes.
2. **Driver init sweep (belt)** — on `New()` the driver scans the repo for stale `<id>.tmp/` (local FS) or `<id>/payload.bin`-without-`manifest.yaml` (S3) older than `grace_window` (default 24h). On S3, also aborts stale multipart uploads older than `grace_window`. Non-fatal — logged at WARN, never blocks startup.
3. **Operator-owned S3 bucket lifecycle rule (suspenders)** — the `AbortIncompleteMultipartUpload` rule above. Runs independently of DittoFS process lifetime.

Phase 4 retention (count / age / pin) operates **only** on published backups (manifest-present). It never touches orphans — cleanup is the driver's problem.

## Validation at repo-create

`ValidateConfig` runs before the `backup_repos` row is persisted (wired end-to-end once Phase 6 delivers `dfsctl repo add`). Failure modes:

| Error sentinel               | When                                                                                  | Likely fix                                                    |
| ---------------------------- | ------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| `ErrIncompatibleConfig`      | `path` missing / not a directory / not writable; `bucket` missing; prefix collision   | Re-create path with correct owner+mode, or move backup to a non-overlapping bucket/prefix |
| `ErrPermissionDenied`        | S3 `AccessDenied` / `Forbidden` on `HeadBucket`                                       | Fix IAM policy — backup role needs s3:ListBucket and s3:GetBucketLocation at minimum |
| `ErrDestinationUnavailable`  | Network timeout, DNS lookup failure, S3 5xx                                           | Retry; check connectivity to endpoint                         |
| `ErrInvalidKeyMaterial`      | Env var not 64 hex chars; file not exactly 32 bytes                                   | Re-generate with `openssl rand -hex 32` or `openssl rand -out <path> 32` |
| `ErrSHA256Mismatch`          | Read-back path integrity failure (not a validation error — surfaces at `GetBackup`)   | Bit-rot on storage; restore from an older backup              |
| (warn only)                  | `path` on NFS/SMB/FUSE; S3 bucket has no `AbortIncompleteMultipartUpload` lifecycle   | Consider a local-disk repo; add the lifecycle rule            |

Warnings do not reject. DittoFS emits them at WARN on `ValidateConfig`; operators can decide to override.

## What this release does NOT include

- **External KMS or SSE-S3 / SSE-KMS pass-through** — operator-supplied raw 32-byte keys only. A future "External KMS" milestone will add Vault and AWS KMS key wrapping.
- **Passphrase + KDF (scrypt / argon2)** — raw key material only. Key-ref schemes that look like `pass:` are not accepted.
- **Multi-key envelope (KEK + DEK)** — one key per repo. Every payload written with a given repo's key is protected by that single key; no per-backup key wrapping.
- **Compression** — manifest v1 has no compression field. Defer to a future manifest version bump. Binary Postgres dumps would benefit; in-memory gob dumps much less so.
- **Resumable uploads** — a killed multipart upload is aborted (by AWS SDK or the orphan sweep) and retried from scratch. Requires CAS-chunked content addressing; deferred to v0.14+.
- **Automatic test-restore / verify command** — equivalent of `restic check`. Use integration tests and manual restore drills for now. Planned as a future `AUTO-01` requirement.
- **GCS / Azure Blob / SFTP drivers** — not in v0.13.0. Local FS + S3 (with S3-compatible endpoints) covers the 99% case.
- **Re-encryption of existing backups on key rotation** — old backups keep their original key. Let retention age them out rather than rewriting every `payload.bin`.

## See also

- [ARCHITECTURE.md](ARCHITECTURE.md) — overall system design
- [CONFIGURATION.md](CONFIGURATION.md) — global configuration (logging, telemetry, server, adapters)
- [SECURITY.md](SECURITY.md) — threat model, authentication, network security
- [FAQ.md](FAQ.md) — scope rationale and known limitations
- `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` — full design-decision record (D-01 through D-14) with rationale for layout, atomic publish, encryption envelope, orphan sweep, and bucket collision enforcement
- `pkg/backup/destination/` godoc — driver API reference
- [RFC 5116](https://tools.ietf.org/html/rfc5116) — AEAD framework (AES-256-GCM)
- [AWS S3 `AbortIncompleteMultipartUpload` lifecycle rule](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html)
