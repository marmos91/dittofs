# Pitfalls Research — Metadata Backup & Restore (issue #368)

**Domain:** Live multi-protocol (NFSv3/v4.x + SMB3) filesystem server, pluggable metadata stores (memory / BadgerDB / PostgreSQL), per-share block stores (local + remote S3). Adding on-demand + scheduled backup/restore with local-FS / S3 destinations.
**Researched:** 2026-04-15
**Confidence:** HIGH (derived from DittoFS architecture + well-known BadgerDB/Postgres/S3 backup failure modes; several patterns directly observed in DittoFS production — see MEMORY.md on Localstack flakiness and operator credential rotation).

Pitfalls below are ordered by severity. Each pitfall is scoped to *this* milestone — backup/restore added to a live-serving NFS/SMB server, not generic "backups are hard" advice.

---

## Critical Pitfalls

### Pitfall 1: Inconsistent snapshot — backup taken during in-flight WRITEs

**What goes wrong:**
A WRITE compound on a large file is mid-flight (metadata says size=10 MB, block store has 7 MB flushed + 3 MB in local cache not yet uploaded). Backup reads the metadata store and captures `size=10MB, PayloadID=X`. On restore into a fresh cluster, file is advertised as 10 MB but only 7 MB of blocks exist in S3 → reads return ESTALE / zero-fill / short reads at offset 7 MB+.

**Why it happens:**
Metadata store and per-share BlockStore are separate systems. "Backing up metadata" means one of them only. Block store is not in the backup scope for v0.13.0, but restored metadata *references* block-store state. Easy to ship a "metadata backup" without auditing the implicit coupling.

**How to avoid:**
- Document explicitly in CLI help + manifest: "metadata backup assumes block store is preserved independently." Manifest records `block_store_refs` (config IDs, bucket+prefix, epoch).
- Snapshot the metadata store *atomically*. BadgerDB: `DB.Backup(writer, since)` (MVCC-safe under writes). Postgres: single `REPEATABLE READ` / `SERIALIZABLE DEFERRABLE` transaction, or logical dump via store interface in a single snapshot txn. Memory: hold read lock during serialization pass.
- **Never** iterate the store with multiple small read transactions. That is a non-atomic snapshot and captures torn state across shares.
- Refuse restore (or loudly warn) if the manifest's referenced remote block store is missing / different.

**Warning signs:**
- E2E test absent: concurrent fio-style write + backup + restore + byte-compare.
- Restore succeeds but first `READ` of a recently-written file returns wrong bytes or NoSuchKey at a mid-file offset.

**Phase to address:** Phase 01 (manifest schema) + Phase 02 (per-store atomic snapshot). Verified in Phase 06 E2E.

**Severity:** CRITICAL — silent data corruption on restore.

---

### Pitfall 2: Restore while shares are mounted → stale handles + client cache poisoning

**What goes wrong:**
Admin runs `dfsctl store metadata my-store restore`. Store is swapped under live shares. NFS/SMB clients hold file handles embedding stable handle IDs and (for SMB) durable-handle/lease state. After restore: some handles still resolve (same ID space) but to different inodes → cross-file data corruption visible to clients. NFSv4 `change` attribute inconsistent → client cache coherence breaks. SMB lease break never fires because server forgot the lease state existed.

**Why it happens:**
Restore is naively implemented as "load snapshot into store". But the Runtime has ephemeral state (mount tracking, NFSv4 state IDs, SMB lease registry, Unified Lock Manager state) built on top of the *previous* metadata generation. Clients don't know the server rewound.

**How to avoid:**
- **Refuse restore on a store with any actively mounted share.** Check `runtime.mounts.Service` before proceeding. Error lists active mounts.
- `--force` flag with `--drain` semantics: quiesce shares using this store, issue NFSv4 `CB_RECALL` on outstanding delegations, break all SMB leases, wait grace period, then restore.
- After restore, all client handles become NFS3ERR_STALE / NFS4ERR_STALE_FH / SMB STATUS_FILE_CLOSED. This is *correct* — do not try to "rebuild" them. Document: clients must remount.
- For NFSv4, increment server boot verifier on restore so clients see a fresh server instance (reclaim grace triggers; reclaim will correctly fail).
- For SMB, clear durable-handle registry for the restored store; break all leases (LEASE_BREAK_NOTIFICATION with break-to-None).
- Integrate with Unified Lock Manager: flush lock state that was captured in the backup; the post-restore state comes from the snapshot, not current memory.

**Warning signs:**
- Restore command completes with shares still in `mounts` table.
- Post-restore E2E: client cached read returns pre-restore data (stale client not invalidated).
- Client logs show handle errors hours later (cache-driven lazy discovery).

**Phase to address:** Phase 04 (restore orchestration + mount gating + boot-verifier bump).

**Severity:** CRITICAL — silent cross-file data corruption.

---

### Pitfall 3: Block store divergence — restored metadata references GC'd blocks

**What goes wrong:**
Day 1: backup. Day 2: delete files. Day 3: block GC (`pkg/blockstore/gc/`) reclaims orphan blocks from S3. Day 4: restore Day-1 backup. Metadata references blocks that were deleted in S3 → every READ of "old" files returns S3 NoSuchKey surfaced as NFS3ERR_IO.

**Why it happens:**
Block GC uses reference counting against *live* metadata. Restoring metadata resurrects references that were lawfully collected.

**How to avoid:**
- **Block GC must be pausable for retained backups.** Add a "backup hold" — GC consults the PayloadID set of each retained backup manifest before deleting.
- Manifest records the PayloadID set (or a bloom filter / merkle summary for compactness).
- Enforce invariant at repo-creation time: "metadata backup retention ≤ block-store retention/GC window". Reject incompatible configs.
- Simpler v0.13.0 alternative: restore logs a warning listing PayloadIDs missing from the remote block store. Admin explicitly accepts partial data loss. Defer GC-hold to later milestone but ship the manifest field now so it's forward-compatible.

**Warning signs:**
- Restore "succeeds" but `READ` of old files returns `NFS3ERR_IO`; server logs show S3 404s.
- No integration between backup retention and block-store GC policy.

**Phase to address:** Phase 01 (manifest records PayloadID set) + Phase 05 (GC integration).

**Severity:** CRITICAL — silent data loss masked as I/O errors.

---

### Pitfall 4: Cross-store contamination — backup from store A restored into store B

**What goes wrong:**
User backs up `fast-meta`, restores into `persistent-meta`. File handles encode share IDs that don't exist in the target runtime. Or: same store name, different schema version (BadgerDB v3 backup → v4 code) → GORM auto-migrates silently, partial data appears.

**Why it happens:**
A backup file is just bytes. Without a manifest tying it to a specific store identity + schema version, nothing prevents mixing.

**How to avoid:**
Every backup writes `manifest.json`:
- `store_id` (UUID, generated on first init and persisted in the store itself)
- `store_type` (memory / badger / postgres)
- `schema_version` (integer, bumped on migration)
- `dittofs_version` (git sha + semver)
- `shares_referenced` (list of share IDs + names at backup time)
- `block_store_refs` (see Pitfall 1)
- `created_at` (UTC)
- `backup_id` (ULID — sortable, time-ordered)
- `payload_checksum` (SHA-256 of data payload)
- `manifest_checksum` or HMAC over the manifest itself

Restore refuses if `store_type` ≠ target or `store_id` ≠ target's store_id (override with `--force-cross-store` + loud warning). If `schema_version > current`: refuse (can't downgrade). If `<`: run forward migrations.

**Warning signs:**
- No manifest file. Backup is a raw dump.
- No `store_id` persisted in the store itself.
- Restore path has no version check.

**Phase to address:** Phase 01 (manifest + store_id introduction, forward-compatible migration) + Phase 04 (restore validation).

**Severity:** CRITICAL — data corruption on heterogeneous deployments.

---

### Pitfall 5: BadgerDB — wrong backup API corrupts snapshot

**What goes wrong:**
Developers reach for "iterate all keys and write them" which races with value-log GC. Worse: `cp -r` or `tar` of the data directory while DB is open — captures inconsistent SST/WAL, ~5% of restores fail with "CORRUPT: checksum mismatch".

**Why it happens:**
BadgerDB has an LSM + value log; on-disk state during writes is not a consistent snapshot. The correct API is `DB.Backup(w io.Writer, since uint64)` which uses a read snapshot and is safe under concurrent writes.

**How to avoid:**
- Use `badger.DB.Backup(w, since)` for backup, `badger.DB.Load(r, maxPendingWrites)` on a fresh directory for restore.
- Forbid file-level backup of BadgerDB dirs in code (comment + lint) and docs.
- Optional: run vlog GC before backup to shrink size.
- Incremental: persist last-backup version cursor per store in the repo manifest; pass to `Backup(w, since)`.

**Warning signs:**
- Backup code opens files under BadgerDB data dir directly.
- Restore occasionally fails with checksum errors — intermittent = racing GC.

**Phase to address:** Phase 02 (BadgerDB driver).

**Severity:** HIGH — silently unrestorable backups.

---

### Pitfall 6: Postgres — long-txn backup bloats indexes and stalls vacuum

**What goes wrong:**
A naive long `REPEATABLE READ` to get a consistent read holds back vacuum — indexes bloat, query performance degrades during the backup window. For multi-GB metadata stores, this is a production incident.

**Why it happens:**
Postgres MVCC keeps old row versions alive as long as any transaction could see them. Multi-hour `pg_dump` on a write-heavy store stalls autovacuum.

**How to avoid:**
- Prefer logical serializer via the store interface (iterate entities in one serializable txn) — reuses the same serialization format as memory/BadgerDB drivers; consistent abstraction.
- If `pg_dump` is used: `--jobs=N` parallelism to shorten wall-clock; set `statement_timeout` reasonably; monitor `pg_stat_activity`.
- `dfsctl` warns when backup wall-clock exceeds half of `statement_timeout` or a configured budget.
- Document: schedule Postgres backups during low-write windows.

**Warning signs:**
- `pg_stat_activity` shows `backup_worker` running >30 min under normal load.
- Table bloat grows during each backup window (`pg_stat_user_tables.n_dead_tup` climbs).

**Phase to address:** Phase 02 (Postgres driver).

**Severity:** MEDIUM — degraded production performance, not data loss.

---

### Pitfall 7: Scheduler — overlapping runs, missed runs, thundering herd, DST

**What goes wrong:**
- Cron fires `@daily`. Backup takes 90 min. Next day's run starts while previous is still uploading to the same destination → manifest collision / partial overwrite.
- Server down 02:00–03:00. On startup 03:05: fire missed run? If yes → fresh server hammered. If no → silent missed backup.
- All 50 shares scheduled `@daily` → all fire at 00:00:00 → S3 rate-limited, metadata read contention.
- DST fall-back: 02:30 cron runs twice. Spring-forward: skipped.

**Why it happens:**
Cron libraries (including robfig/cron/v3) do not handle any of these by default. Must be designed in.

**How to avoid:**
- **Per-repository mutex**: scheduler checks "is a backup running for this store?". If yes: skip + log + metric `backup_overlap_skipped_total`.
- **Missed-run policy**: explicit config `missed_run: skip | fire_once | fire_all`. Default `fire_once` (single catch-up, not N).
- **Jitter**: randomized 0–10 min offset per scheduled run. Or serialize via global `backup_concurrency` limit (default 2).
- **Timezone**: store cron expressions with explicit `CRON_TZ=UTC 0 2 * * *` prefix. Store server TZ in config. robfig/cron/v3 supports this.
- **Watchdog**: separate goroutine alerts if no successful run in `2 * interval`.

**Warning signs:**
- Two concurrent S3 PUTs of `manifest.json` for the same store.
- No "last successful backup" metric.
- `cron.EntryID` lifecycle not tested across server restart.

**Phase to address:** Phase 03 (scheduler).

**Severity:** HIGH — silent missed backups are the #1 way backup systems fail in production.

---

### Pitfall 8: S3 destination — partial uploads, retention race, credentials, bucket-sharing

**What goes wrong:**
- Network drop mid-backup → abandoned multipart upload. S3 bill grows forever (no default expiry for incomplete multiparts).
- Retention pruner "delete oldest N". Upload fails after manifest written but before final completion marker. Pruner treats partial as "latest", deletes actual last-good → **data-loss-by-pruner**.
- IAM credentials rotate mid-backup. Multipart parts authed with old creds, commit with new → fails. Retry from scratch.
- Backup bucket == block-store bucket. Prefix collision, block GC may delete backup objects, billing is muddy.

**How to avoid:**
- **Bucket lifecycle rule**: `AbortIncompleteMultipartUpload` after 1 day. `dfsctl store metadata validate-repo` checks and warns if missing.
- **Two-phase commit**: write to `pending/<backup-id>/...`, only publish `backups/<backup-id>/manifest.json` when all chunks uploaded + verified. Retention pruner only considers entries with valid `manifest.json`.
- **Retention is a separate pass**, runs only after upload confirmed. Always keeps `max(N, all < 24h old)`. Never delete a backup that has a completed manifest unless replaced by a newer completed manifest.
- **Refuse configuration** where backup and block-store buckets/prefixes overlap; validated at repo-create time.
- **Credential refresh**: use AWS SDK v2 credential provider chain (auto-refresh). Do not cache for upload duration.
- **Versioning/object-lock**: if bucket has versioning, `DELETE` creates a tombstone — pruner must issue permanent delete with `VersionId`, or backups accumulate forever. Document.
- **S3 LIST consistency**: AWS S3 is strongly consistent since Dec 2020, but non-AWS S3 (MinIO, Ceph, Scaleway) varies. Don't rely on LIST ordering for "find latest" — read a canonical `LATEST` pointer file, or rely on ULID-sortable filenames fetched with explicit lexicographic scan.

**Warning signs:**
- S3 bill growing faster than `du` of backup prefix.
- Pruner deletes files the scheduler just created (log race).
- No `validate-repo` command.

**Phase to address:** Phase 03 (S3 destination) + Phase 05 (retention).

**Severity:** HIGH — retention-race data loss is silent and catastrophic.

---

### Pitfall 9: Encryption, key management, blast radius

**What goes wrong:**
Metadata contains full namespace: filenames, UIDs/GIDs, ACLs, extended attributes, lock owners, Kerberos principals, SID mappings. Plaintext backup to S3 leaks the whole filesystem to anyone with `s3:GetObject`. Encrypting with a key stored on the same server means compromising the server yields both ciphertext and key → encryption is theater.

**Why it happens:**
Backup is treated as ops, not security. "SSE-S3 is on, we're fine" — but SSE-S3 uses AWS-managed keys; any IAM role with `GetObject` reads plaintext.

**How to avoid:**
- Encrypt at rest with a key **not on the same host**:
  - **SSE-KMS** (AWS KMS) — key in KMS, server only has `kms:Decrypt`. Explicit audit trail, native rotation. Strongest for AWS-native.
  - **Customer-supplied key** loaded from env var or file at backup/restore time, never persisted on the server disk. Restore prompts for key.
- Manifest records encryption mode + key ID (KMS ARN or key fingerprint). Restore validates.
- **Plaintext deny-by-default**. Require explicit `--allow-plaintext` with loud warning.
- Local-FS destination: same rules; warn if destination is on same disk as source store (defeats DR).
- Document: lost key = lost backup. Recommend KMS with key rotation + old-version retention.
- Explicitly out of scope: DittoFS does **not** ship its own KDC/HSM for this milestone — reuse AWS KMS or customer-provided.

**Warning signs:**
- Backup file is `jq`-readable or `badger dump`-readable.
- No encryption field in manifest.
- No test for "rotate KMS key, old backup still restorable" (should work via KMS key versioning).

**Phase to address:** Phase 03 (destinations) + Phase 06 (security review).

**Severity:** HIGH — enterprise data exfiltration vector.

---

### Pitfall 10: Silent failures — cron fired, backup failed, no one knows

**What goes wrong:**
Six months later, admin tries to restore. Last success: 5 months ago. Scheduler ran every night; every run failed after the first week (credentials expired). No one noticed. This is the industry's most common backup failure mode.

**Why it happens:**
Backup success is invisible. Failure is invisible (logged but not alerted). Admins learn the truth at restore time — precisely when they cannot tolerate bad news.

**How to avoid:**
- **Heartbeat metric**: `dittofs_backup_last_success_timestamp_seconds{store="..."}`. Prometheus-scrapeable. Alertable: `time() - last_success > 48h`.
- **Structured log events**: `event=backup_completed status=success|failure store=... duration_s=... bytes=...`.
- `dfsctl store metadata <name> backup list` shows last N attempts including failures with error reason + duration.
- `dfs status` surfaces backup health per store.
- Control-plane REST `/healthz` includes backup freshness. K8s operator propagates to DittoServer CR status (ties into existing operator integration; cf. MEMORY.md).

**Warning signs:**
- Backup log entries only, no metrics.
- `backup list` hides failures.
- Admin learns about failures from a user's restore request.

**Phase to address:** Phase 05 (observability). Operator integration later.

**Severity:** CRITICAL — force-multiplier on every other pitfall.

---

### Pitfall 11: CLI UX — synchronous block, no restore confirm, unsortable IDs

**What goes wrong:**
- `dfsctl store metadata backup` blocks 20 min on a large Postgres store. Ctrl-C. Server cancels? Ghost goroutine writing to S3?
- `dfsctl store metadata restore --from latest` — meant staging, ran prod. No confirmation. Irrecoverable.
- Backup IDs `backup-<uuidv4>`. Lexical sort ≠ chronological sort. Admin can't tell newest from `aws s3 ls`.

**How to avoid:**
- **Async-by-default** for long ops: `backup` returns job ID immediately; `backup status <id>`, `backup wait <id>` (blocks with progress), `backup cancel <id>`. Ctrl-C on `wait` detaches client without canceling server job.
- Server tracks jobs in a small table (in-memory for v0.13.0, persistent later). Orphan jobs time out after max-duration.
- **Restore requires confirmation** — re-type store name, or `--yes-i-really-mean-it`, or interactive TTY prompt. Model after `dfsctl user delete` pattern already in the codebase.
- **ULIDs** for backup IDs (26 chars, time-ordered, lex-sortable, URL-safe). Manifest filename sorts chronologically in S3 listing.
- `--dry-run` on restore: shows what would change (share count, user delta, schema version diff), without applying.

**Warning signs:**
- Ctrl-C during backup leaves orphan multipart upload in S3.
- Restore command has no confirmation prompt.
- Backup IDs are UUIDv4 or otherwise unsortable.

**Phase to address:** Phase 04 (CLI/REST design).

**Severity:** MEDIUM — operator error becomes data loss.

---

### Pitfall 12: Missing tests — corrupt, partial, cross-version, mock-only

**What goes wrong:**
All tests use a fresh backup restored on the same version. Production scenarios — truncated S3 object, bit-flip, v0.13→v0.14 after migration — are never tested. First real bug report is a customer DR drill.

**How to avoid:**
Test matrix in `test/integration/backup/` and `test/e2e/backup/`:
- Happy path: memory/badger/postgres × localfs/S3 destinations.
- Truncated backup → fails cleanly, no panic.
- Bit-flip → checksum catches it.
- Wrong `store_id` → refused, clear error.
- Older `schema_version` → migrate and succeed.
- Newer `schema_version` → refuse, clear upgrade instructions.
- Missing manifest → refuse.
- Manifest present, data files missing → refuse with list of missing files.
- Concurrent backup + fio write load → byte-compare after restore.
- Restore-while-mounted → refused without `--force`.
- Kill server mid-backup + mid-restore (chaos) → no corruption, resumable or cleanly aborted.
- Cross-version matrix in CI: keep last N release binaries, test backup-from-N restore-to-current.

**Mock S3 is not sufficient** — use Localstack (already in `test/e2e/run-e2e.sh --s3`). Multipart semantics, lifecycle rules, and listing quirks only show on real S3 API. Be aware of DittoFS-known Localstack flakiness (`TestCollectGarbage_S3`, MEMORY.md) — use shared container helper, not per-test container.

**Warning signs:**
- Only mock S3 in tests.
- No `_corrupt_` or `_partial_` test cases.
- Test count suspiciously low (6 = one per store × dest is not enough).

**Phase to address:** Phase 06 (test plan).

**Severity:** CRITICAL enabler — absence guarantees production bugs.

---

## Technical Debt Patterns

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| Backup = `cp -r` of BadgerDB dir | 1-day implementation | Unrestorable backups (race with LSM compaction) | **Never** |
| Skip manifest, just write raw dump | Simpler format | No version check → silent cross-version corruption | Never for shipped feature; OK for 1-day spike only |
| Plaintext backups "for now" | No key management complexity | Data exfiltration vector; hard to add encryption retroactively | Only behind `--allow-plaintext` with loud warning |
| Synchronous CLI blocking | Simpler code path | Ctrl-C leaves ghost jobs; no progress | Only for small stores (<100 MB) with timeout fallback |
| Retention pruner inside upload txn | "Atomic" feel | Single failure path deletes good backups | **Never** — always separate pass, only after upload confirmed |
| Reuse block-store S3 config for backup | Less configuration | Prefix collision, billing mess, GC may delete backups | **Never** — force separate bucket or explicit-opt-in prefix |
| Encryption key in control-plane DB | Simple deploy | Key + ciphertext co-located → theater | Never for prod; dev/demo with warning only |
| Cron with no overlap check | Simpler scheduler | Concurrent uploads corrupt manifest | **Never** |
| No `store_id` persistence | Skip one migration | Can't ever detect cross-store restore | Never — add `store_id` in first migration even if unused initially |
| Restore into live-mounted share | Less orchestration to build | Silent cross-file data corruption | **Never** — refuse by default, require `--force` + drain |
| Rely on S3 LIST to "find latest" | One fewer file | Eventually-consistent compatibles (MinIO/Ceph) return stale lists | Never — use sortable ULID names or `LATEST` pointer |

## Integration Gotchas

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| BadgerDB | Tar the data directory | `DB.Backup(writer, since)` / `DB.Load(reader, N)` exclusively |
| PostgreSQL | `pg_dump` without timeout | Logical serializer via store interface, or `pg_dump --jobs --serializable-deferrable` with monitored `statement_timeout` |
| S3 | No lifecycle rule for multipart cleanup | `AbortIncompleteMultipartUpload` after 1d; `validate-repo` checks |
| S3 non-AWS (MinIO/Ceph/Scaleway) | Assume strong LIST consistency | Rely on sortable ULID object names, not LIST ordering; maintain `LATEST` pointer or scan lexicographically |
| S3 | Same bucket/prefix as block store | Refuse at config time; require separation |
| AWS SDK v2 | Cache resolved credentials | Use credential provider chain with auto-refresh |
| robfig/cron/v3 | Assume server TZ, no overlap guard, no missed-run policy | Always `CRON_TZ=` prefix; wrap scheduler with per-store mutex, missed-run policy, jitter |
| Block store GC | Unaware of backup retention | GC consults retained backup manifests' PayloadID sets before delete |
| NFSv4 clients | No boot verifier bump on restore | Increment boot verifier → clients see server reboot, reclaim grace triggers (and correctly fails for restored state) |
| SMB durable handles | Persist across restore silently | Clear durable-handle registry on restore; break all leases |
| Unified Lock Manager | Lock state not part of backup scope | Document: locks are ephemeral across restore; restored state has no active locks (matches post-crash semantics) |
| K8s operator | Restore via CR annotation with no quiesce | Operator patches DittoServer `paused: true` → runtime drains → restore → unpause |
| Control-plane backup (`dfs backup controlplane`) | Confuse with metadata backup | Keep separate (different scope, different retention). Document which contains what |

## Performance Traps

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Read-all-keys in single long txn | Postgres bloat, Badger memory spike | Stream-and-checkpoint, or native `DB.Backup()` | >10M file entries |
| Single-threaded S3 upload | Backup takes hours | Multipart with 16 concurrent parts (matches existing DittoFS S3 tuning auto-scale) | >1 GB backup size |
| Full backup nightly, no incremental | S3 bill + bandwidth linear in data × days | Incremental: Badger `since` cursor; Postgres LSN or logical replication | >10 GB metadata store |
| Retention LISTs entire bucket | Slow + costly S3 LIST bill | Scan `backups/` prefix only; read manifest sizes from existing manifest, not per-object HEAD | >1000 backups retained |
| Full decompress in memory on restore | OOM | Streaming decode (natural with BadgerDB `Load`) | >1 GB backup |
| All shares back up at 00:00 UTC | S3 rate-limit, metadata contention | Jitter + global `backup_concurrency` limit | >20 shares |
| SHA-256 entire backup after upload | Long post-upload delay | Streaming checksum (SHA-256 incremental during upload) | >5 GB backup |
| Ctrl-C never cancels server upload | Resource leak, ghost S3 parts | Job context cancellation; server-side cleanup on abort/timeout | Any scale |

## Security Mistakes

| Mistake | Risk | Prevention |
|---------|------|------------|
| Plaintext backup to S3 | Full namespace leak (filenames, UIDs, ACLs, Kerberos principals) | Default deny; require KMS or customer-supplied key |
| Encryption key stored with ciphertext | Theater — same blast radius | KMS-managed key, or env-var-only at backup/restore time |
| IAM `s3:*` on backup bucket | Privilege escalation via bucket policy tampering | Least-privilege: `PutObject, GetObject, ListBucket, AbortMultipartUpload, DeleteObject` |
| No integrity check on restore | Tampered backup installs attacker ACLs / SID mappings | SHA-256 in manifest + HMAC signed by backup key; verify before apply |
| Backup includes bcrypt hashes from `Users` | Password-cracking target if leaked | Metadata backup is *not* control-plane backup — keep separate; document split |
| Restore API unauthenticated / broad authz | Remote takeover via attacker-controlled metadata | Admin role + re-auth prompt; audit log before/after digest |
| S3 bucket logging disabled | Can't audit who restored when | `validate-repo` warns if bucket logging off |
| Backups leak content via PayloadIDs | Known-plaintext confirms file presence | Either encrypt payload-id hashes or document residual risk in threat model |
| No rotation plan for backup encryption key | Key compromise invalidates all history | KMS with automatic rotation + retain old versions for restorability |

## UX Pitfalls

| Pitfall | User Impact | Better Approach |
|---------|-------------|-----------------|
| Restore has no `--dry-run` | Can't preview | `--dry-run` shows share/user/schema delta |
| No confirmation prompt on restore | Single typo = data loss | Re-type store name, or `--yes-i-really-mean-it`, or interactive TTY prompt |
| `backup list` shows only successes | Failures invisible | Show all attempts with status/duration/size/error |
| Cron validated only at runtime | Typo silently never fires | Validate on `backup-repo create --schedule "..."` with clear error |
| Size shown as bytes | Unreadable | Human-readable in text mode; raw bytes in `-o json` |
| No ETA / progress | Users Ctrl-C thinking it's stuck | Progress bar in `backup wait`; server exposes bytes-transferred and est-total |
| Error "backup failed" | No recourse | Structured error with failure stage (snapshot / serialize / upload / verify) + remediation hint |
| Retention auto-deletes important manual backup | Unrecoverable | `backup pin <id>` — retention skips pinned |
| `backup list` output not sorted chronologically | Admin picks wrong one | Always sort newest-first by default; `--sort=oldest` flag |

## "Looks Done But Isn't" Checklist

- [ ] **Backup implementation:** Atomic snapshot — verified with concurrent-write + byte-compare E2E
- [ ] **Backup format:** Manifest with `store_id` + `schema_version` + `block_store_refs` — verified with cross-store restore refused test
- [ ] **BadgerDB driver:** Uses `DB.Backup()` not directory copy — verified by race-test with concurrent vlog GC
- [ ] **Postgres driver:** Has `statement_timeout` and monitoring — verified with 10M-row load test
- [ ] **S3 destination:** `AbortIncompleteMultipartUpload` lifecycle rule required — `validate-repo` catches absence
- [ ] **S3 destination:** Two-phase commit — kill mid-upload, no ghost "latest"
- [ ] **S3 destination:** ULID-sortable object names — `aws s3 ls` sorted chronologically
- [ ] **Restore:** Active-mount check — refuses without `--force`
- [ ] **Restore:** Boot verifier bump on NFSv4; durable-handle registry cleared on SMB — verified clients see fresh-server
- [ ] **Scheduler:** Overlap guard — long backup + next cron tick = skipped with metric
- [ ] **Scheduler:** Jitter — 20-share config has no N-way concurrent start
- [ ] **Scheduler:** Missed-run policy explicit in config
- [ ] **Retention:** Separate pass, after upload confirmed — verified no race
- [ ] **Retention:** Versioned-delete on versioned buckets — verified backups actually disappear
- [ ] **Retention:** Pinned backups not pruned
- [ ] **Encryption:** Plaintext requires explicit flag
- [ ] **Encryption:** Key separation from host — KMS integration or documented policy
- [ ] **Block store integration:** GC-hold for retained backups — verified no reference dangling
- [ ] **Observability:** `dittofs_backup_last_success_timestamp_seconds` metric exists
- [ ] **Alerts:** Default alert rule "no success in 2× interval" documented
- [ ] **CLI:** Async by default — Ctrl-C on `wait` doesn't cancel server job
- [ ] **CLI:** Restore confirmation required
- [ ] **Backup IDs:** ULIDs (sortable = chronological)
- [ ] **Tests:** Localstack path in CI
- [ ] **Tests:** Corruption cases (truncated, bit-flip, missing manifest, stale data-files)
- [ ] **Tests:** Cross-version restore
- [ ] **Docs:** `docs/BACKUP_RESTORE.md` with explicit scope (metadata only, not block data)
- [ ] **Docs:** Mapping between `dfs backup controlplane` and `dfsctl store metadata backup` — what each covers

## Recovery Strategies

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| Torn-snapshot backup discovered at restore | HIGH | Fall back to next-older good backup if exists. Otherwise: restore, checksum-verify against block store, quarantine mismatched files, require manual recovery from client copies |
| Restore into active mount corrupted clients | HIGH | Force-unmount all clients, bump NFSv4 boot verifier, restart SMB adapter, restore from a known-good backup, clients remount fresh |
| Block GC deleted blocks referenced by backup | VERY HIGH | No server recovery — blocks gone. Add GC-hold going forward. Communicate per-file data loss |
| Cross-store restore contaminated target | MEDIUM if caught early | Stop shares on target, restore target's last clean backup, resume |
| Schema-version mismatch restored partial data | HIGH | Stop, restore target's previous known-good, upgrade forward via migration tool, retry |
| Ghost multipart uploads on S3 | LOW | `aws s3api list-multipart-uploads` + `abort-multipart-upload`; install lifecycle rule to prevent recurrence |
| Pruner deleted last good backup | VERY HIGH | If versioning enabled: restore from version. Otherwise: fall back to older backup if any |
| Lost encryption key | TOTAL LOSS | No recovery. Prevention is the only option — KMS with rotation + old-version retention, or split-key escrow |
| Missed-run chain (months no success) | Depends | Immediate: on-demand backup, verify restore on staging. Long-term: add heartbeat alert |
| Ctrl-C left ghost server job | LOW | Server timeout cleans up; `backup status <id>` to verify. Design: max-job-duration timeout enforced |

## Pitfall-to-Phase Mapping

| # | Pitfall | Prevention Phase | Verification |
|---|---------|------------------|--------------|
| 1 | Inconsistent snapshot | Phase 02 (per-store driver) | E2E concurrent-write + byte-compare |
| 2 | Restore while mounted | Phase 04 (restore orchestration) | E2E mounted-restore must refuse without `--force` |
| 3 | Block store divergence | Phase 01 (manifest PayloadIDs) + Phase 05 (GC integration) | E2E create→backup→delete→GC→restore→read |
| 4 | Cross-store contamination | Phase 01 (manifest + store_id) | Integration test: wrong `store_id` refused |
| 5 | BadgerDB wrong API | Phase 02 (BadgerDB driver) | Review + race-test with concurrent GC |
| 6 | Postgres long-txn bloat | Phase 02 (Postgres driver) | Integration: monitor `pg_stat_activity` during backup load |
| 7 | Scheduler edge cases | Phase 03 (scheduler) | Unit: overlap, DST, jitter, missed-run policy |
| 8 | S3 partial + retention race | Phase 03 (S3 destination) + Phase 05 (retention) | Localstack chaos: kill mid-upload; retention never deletes newest good |
| 9 | Encryption / key mgmt | Phase 03 (destinations) + Phase 06 (security review) | Threat model doc; KMS round-trip; rotate-and-restore test |
| 10 | Silent failures | Phase 05 (observability) | Prometheus metric present; alert rule documented |
| 11 | CLI UX | Phase 04 (CLI/REST) | UX: Ctrl-C leaks no S3 parts; restore prompts |
| 12 | Missing corrupt/cross-version tests | Phase 06 (test plan) | CI matrix: truncated / bit-flipped / cross-version |

### Suggested phase structure (for roadmap author)

1. **Phase 01 — Backup repository schema, manifest, `store_id`**
   Foundation. Adds `store_id` migration to all metadata stores. Defines manifest format with schema_version, block_store_refs, payload_id_set, encryption descriptor. Prevents #4; scaffolds #1, #3.

2. **Phase 02 — Per-store backup/restore drivers (memory, BadgerDB, Postgres)**
   Correct atomic-snapshot APIs per backend. Prevents #1, #5, #6.

3. **Phase 03 — Destination drivers (local FS, S3) + scheduler + encryption hooks**
   Two-phase commit, lifecycle rule, overlap mutex, jitter, CRON_TZ, SSE-KMS / customer key. Prevents #7, partial #8, #9.

4. **Phase 04 — Restore orchestration + CLI/REST API**
   Active-mount gate, boot-verifier bump, durable-handle clear, async job API, ULID IDs, confirmation prompts, `--dry-run`. Prevents #2, #11.

5. **Phase 05 — Retention + observability + block-store GC integration**
   Retention as separate post-upload pass, `backup pin`, Prometheus metrics + heartbeat, GC-hold consulting manifests. Prevents #3 (full), #8 (retention race), #10.

6. **Phase 06 — Test matrix + security review + docs**
   Localstack E2E, corruption/partial/cross-version, chaos tests, threat model, `docs/BACKUP_RESTORE.md`, operator integration notes. Validates #9, prevents #12.

Phases 01–03 can largely proceed in parallel once manifest schema is fixed. Phase 04 depends on 01+02. Phase 05 depends on 03+04. Phase 06 is cross-cutting and should start in parallel with 04.

## Sources

- DittoFS codebase: `pkg/metadata/store/{memory,badger,postgres}`, `pkg/blockstore/{engine,gc,remote/s3}`, `pkg/controlplane/{store,runtime}`, existing `dfs backup controlplane` (reference precedent for a config-only backup/restore pattern).
- BadgerDB: `DB.Backup(io.Writer, uint64) (uint64, error)` / `DB.Load(io.Reader, int)` API — the only safe concurrent-write snapshot method (documented on dgraph.io/docs/badger).
- PostgreSQL: MVCC + autovacuum interaction with long transactions, `postgresql.org/docs/current/routine-vacuuming.html`; `pg_dump --serializable-deferrable` semantics.
- AWS S3 strong consistency announcement (Dec 2020); non-AWS S3-compatibles (MinIO, Ceph, Scaleway) may still have LIST-after-PUT lag.
- robfig/cron/v3 — no overlap guard, no missed-run handling, no default TZ; supports `CRON_TZ=` prefix but must be invoked explicitly.
- NFSv4 change attribute and boot verifier: RFC 7530 §3.3.5 and §3.3.1.
- SMB3 durable-handle reconnect across server restart: MS-SMB2 §3.3.5.9.7.
- DittoFS MEMORY.md — Localstack flakiness on shared containers (`TestCollectGarbage_S3`), operator credential-rotation loop (StatefulSet restart + identity loss), S3 random-write optimization lessons (auto-scale uploads, cache hygiene between runs). Directly informs Pitfall 8 (credential rotation) and Pitfall 12 (Localstack test hygiene).
- DittoFS project conventions: `CLAUDE.md` — metadata vs control-plane backup scope separation; existing `dfs backup controlplane` is config-only, confirming the gap this milestone fills.
- Industry post-mortem patterns (retention race, ghost multiparts, encryption-key co-location, silent-cron-failure): synthesized from common DBaaS vendor incident reports (GitLab 2017, various AWS S3 backup studies) — pattern-level, not citation-level.

---
*Pitfalls research for: metadata backup/restore in DittoFS (issue #368)*
*Researched: 2026-04-15*
