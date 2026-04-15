# Feature Research — Metadata Backup & Restore (v0.13.0)

**Domain:** Metadata-store backup/restore for a distributed/virtual filesystem NAS (DittoFS)
**Researched:** 2026-04-15
**Confidence:** HIGH (ecosystem well-documented: restic, kopia, borg, JuiceFS, etcd, MinIO, k3s, RKE2)

## Scope Clarification

This research covers ONLY the NEW milestone feature: backup/restore of metadata store **contents** (file tree, attributes, permissions, ACLs, lock/lease state, xattrs). The existing `dfs backup/restore controlplane` (users/groups/shares/permissions/store configs) is out of scope except insofar as the UX conventions it established should be preserved.

Target surface per PROJECT.md:
- `dfsctl store metadata <name> backup [--wait]`
- `dfsctl store metadata <name> restore [--from <id>]`
- `dfsctl store metadata <name> backup list`
- Per-store backup **repository** config: destination (local FS | S3), schedule (cron), retention policy
- In-process scheduler
- REST API mirror (drives `dittofs-pro` UI)

## Feature Landscape

### Table Stakes (Operators Will Expect These)

Missing any of these makes the feature feel half-finished relative to JuiceFS, etcd, restic, kopia.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| On-demand backup command | Fundamental — every backup tool has a manual trigger (`restic backup`, `juicefs dump`, `etcdctl snapshot save`, `k3s etcd-snapshot save`) | LOW | Partially modeled by `dfs backup controlplane`. New surface: `dfsctl store metadata <name> backup`. |
| Scheduled backup (cron expression) | JuiceFS has it built-in (hourly default since v1.0). etcd-snapshot, RKE2, k3s all support `--schedule-cron`. Operators won't hand-roll cron + API calls. | MEDIUM | In-process scheduler (`github.com/robfig/cron/v3`) keyed per-metadata-store. Schedule persisted in control-plane DB, survives restart. |
| List backups with stable IDs | Universal: `restic snapshots`, `kopia snapshot list`, `juicefs load` expects named dumps, `etcd-snapshot ls`. Required for restore UX. | LOW | Columns: ID, timestamp (RFC3339), size (human + bytes), status (complete/in-progress/failed), destination, mode (full/incr if applicable). Support `-o json/yaml/table`. |
| Restore latest by default | Kopia/restic default to latest snapshot if `--from` omitted. "Panic restore" must be one command. | LOW | `dfsctl store metadata foo restore` == "restore newest successful backup". |
| Restore from specific backup ID | Fundamental DR: rollback from corruption that occurred between N and N-1. | LOW | `--from <id>` flag. ID format short-prefix-matchable like git (restic/kopia style). |
| Local FS destination | Default/simplest driver. Testing, air-gapped, NFS-mounted target. | LOW | Reuse existing `local/fs` abstractions. Atomic write (tmp + rename). |
| S3 destination | Production standard. JuiceFS defaults to the same object store as data. MinIO KMS, etcd tools all support S3 / S3-compatible. | MEDIUM | Reuse existing `remote/s3` store. Versioned keys + manifest object. Must work against LocalStack (E2E). |
| Retention policy (count-based) | "Keep last N" — simplest and most used restic option (`--keep-last N`). Prevents unbounded growth. | LOW | Apply after successful backup. Never delete the only successful backup. |
| Retention policy (age-based) | `--keep-within 30d`, `--keep-daily/--keep-weekly/--keep-monthly` are ubiquitous. GFS is differentiator; simple TTL is table stakes. | MEDIUM | Support `max_age` as companion to count. |
| Consistent snapshot (no-quiesce where possible) | etcd uses MVCC; BadgerDB supports `DB.Backup(w, since)` stream at a consistent sequence; Postgres has `pg_dump` snapshot isolation. Users expect backups while server serves I/O. | MEDIUM-HIGH | Badger: stream backup. Postgres: `pg_dump` or `BEGIN ISOLATION LEVEL REPEATABLE READ`. Memory: freeze via RWMutex Rlock during dump. |
| Backup progress / status | etcdctl prints progress; restic shows `files: X, bytes: Y`. For async REST jobs, status endpoint is mandatory. | MEDIUM | Report: state, bytes written, items processed, ETA optional, error detail on failure. |
| Restore to the same store (in place) | Primary DR scenario. Must fail-safe if destination store is non-empty (force flag). | MEDIUM | Lock store (stop accepting ops via maintenance mode), drain shares, reset, stream in, unlock. |
| Async REST job with polling | Long-running (multi-GB metadata) backup blocks HTTP. Industry-standard pattern (AWS Backup, GCS transfers, K8s Jobs). | MEDIUM | `POST /api/v1/stores/metadata/{name}/backups` → `202 Accepted` + `{job_id, poll_url}`. `GET /api/v1/backup-jobs/{id}` → status. CLI hides this behind `--wait` (default) vs `--async`. |
| Failure detection + observability | Failed backup must not be silently counted as a retention slot; must be alertable. | LOW | Distinct failed state in listing. Structured log events. Prometheus counter. |
| Checksum / integrity on write | `restic check`, etcd snapshot SHA256 in status. Detects silent storage corruption. | LOW | SHA-256 of backup payload + manifest. Stored in manifest, verified on restore (skippable via `--skip-verify`). |
| Atomic backup completion | Partial backup must never be listed as "complete." | LOW | Write to `<id>.inprogress`, commit via rename / manifest-last. On S3: manifest is the commit record — written after payload objects. |
| Dry-run / validate | Operators validate before wiring cron. `pg_restore --list`, `restic check --read-data-subset`. | LOW | `--dry-run` on backup: estimate size, validate destination reachability. On restore: print what would be replaced. |

### Differentiators (Competitive Advantage)

These push DittoFS ahead of JuiceFS (JSON dump + hourly auto-backup) and commodity NAS.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Restore to a **new** metadata store | Branch/clone workflows: test migration, forensic inspection, recover a single share without touching production. JuiceFS `load` requires empty target DB, not a new store. | MEDIUM | `--to <new-store-name>` creates fresh store config + restores into it. Enables "staging restore" before cut-over. |
| Cross-engine restore (memory → badger → postgres) | JuiceFS advertises this as a killer feature: JSON backup portable across all metadata engines. Lets operators migrate backends without downtime rebuild. | HIGH | Requires engine-neutral serialization (JSON or protobuf IR). Dovetails with existing `metadata/storetest` conformance suite — any store that passes conformance can round-trip the IR. |
| Test-restore / verify | `restic check`, `kopia verify`. Meaningful backup = verified backup. Most NAS skip this. | MEDIUM | `dfsctl store metadata foo backup verify <id>` — restore into ephemeral memory store, walk tree, compare checksums. |
| Incremental backups | Restic/kopia/borg all dedupe via CAS. For 10M-file metadata stores, full backup every hour is wasteful. | HIGH | Requires manifest format + per-backup delta tracking. Badger `Backup(since)` provides a natural incremental seam. **Depends on manifest v2 — design v1 to forward-compat.** |
| Encryption at rest (client-side) | Operators pushing to S3 want backup encrypted before leaving the node, independent of S3 SSE. Restic/kopia/borg default to this. | MEDIUM | AES-256-GCM with key from control-plane secrets. Key per repository. Public-key write-only-node option = future. |
| Backup repository as first-class object | Named entity (destination + schedule + retention + encryption) lets one store push to multiple repos (local + offsite S3). Restic/kopia model. | MEDIUM | Schema: `BackupRepository { id, store_ref, driver, config, schedule, retention, encryption_key_ref }`. One-to-many store → repositories. |
| GFS retention (grandfather/father/son) | `--keep-hourly 24 --keep-daily 7 --keep-weekly 4 --keep-monthly 12 --keep-yearly 5`. Enterprise compliance shops expect this. | MEDIUM | Restic has the canonical algorithm — port it. |
| Prometheus metrics | `dfs_backup_last_success_timestamp_seconds`, `dfs_backup_bytes`, `dfs_backup_duration_seconds{store,result}`. K8s operators ALERT on these. | LOW | Zero overhead when disabled — matches existing metrics pattern. |
| OpenTelemetry tracing | Already present for other ops. Helpful for diagnosing slow backups (particularly S3 upload phase). | LOW | Span per stage (snapshot/serialize/upload/retention). |
| Webhook / event notifications | Slack/PagerDuty integration without scraping logs. | LOW | POST JSON to configured webhook on success/failure. Not MVP. |
| Resume interrupted backup | 5 GB upload failing at 80% should resume. restic/kopia do via CAS chunking. | HIGH | Requires chunking — **defer past v0.13.0**. |
| Self-describing manifest | Engine type, engine version, schema version, feature flags, lock-manager state. Future-proofs restores. | LOW | Part of MVP manifest format. Cheap insurance. |
| Pre/post-backup hooks | Run shell command or HTTP call. Operators integrating with existing DR stacks. | LOW | Optional. Not MVP. |

### Anti-Features (Commonly Requested, Usually Problematic)

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| Continuous / per-second point-in-time restore | "Log-shipping feels modern" | Requires WAL streaming, replay infra, monotonic clock coordination. Scope explosion. Metadata stores are small enough that hourly + incrementals cover RPO. | Tight cron + incremental. Document RPO tradeoff honestly. |
| Cross-node / HA backup coordination | "Back up the cluster" | DittoFS is single-node (PROJECT.md constraint). Designing for multi-node now is premature. | Per-instance backup. Future: external coordinator if/when multi-node lands. |
| Backup of block-store data | "Back up everything" | Block store is often the object store (S3) — already replicated/versioned. Doubling storage to back it up is wasteful. Block data is content-addressed. | Scope strictly to metadata. Document block-store durability is delegated to remote backend (S3 versioning, lifecycle rules). |
| Backup via NFS/SMB mount to self | "Just mount and dump" | Reentrant — backing up into the FS you're backing up risks locks, recursive growth. | Local FS path outside any DittoFS share, or S3. Validate destination is not a DittoFS-served path. |
| Many backup drivers at launch | "Let me plug in GCS/Azure/B2" | 3 drivers × 3 metadata backends × edge cases = explosion. | Launch with Local FS + S3 (S3-compatible covers B2, Wasabi, R2, MinIO). Add drivers post-validation. |
| External KMS at launch | "We use Vault/AWS KMS" | KMS integration is its own feature (rotation, caching, failure modes, audit). | Launch with local symmetric key from control-plane secret store. KMS = future differentiator. |
| Concurrent backups of same store to same repo | "Parallel backups" | Consistency surface expands; contention; race on retention pruning. | Serialize per-(store, repo). Different repos can fan out sequentially. |
| Deleting the only remaining backup via retention | "Strict retention" | Silent data loss trap. | Always retain ≥1 successful backup. Require explicit `--force-purge` to delete all. |
| Automatic restore on detected corruption | "Self-healing" | Wrong decision in 1% of cases = data destruction during panic event. | Alert + manual operator action. |
| Backing up ephemeral lock/lease state | "Capture everything" | Locks/leases are session-bound. Restoring after crash is actively incorrect. | Back up *persistent* lock ownership records (v1.0 grace-period design needs them) but mark lease TTLs stale; restart triggers grace period. |

## Feature Dependencies

```
BackupRepository config ──┬──> On-demand backup
                          ├──> Scheduled backup ────> In-process cron scheduler
                          ├──> Retention policy
                          └──> REST API + CLI

Manifest v1 ─────────────┬──> List backups
                         ├──> Restore (latest + by-id)
                         ├──> Checksum verify
                         └──> Manifest v2 ─────────> Incremental backup

S3 driver ──────────────> Atomic commit (manifest-last)
Local FS driver ────────> Atomic commit (tmp + rename)

Consistent snapshot source ──┬──> BadgerDB DB.Backup(since)
                             ├──> Postgres pg_dump / logical replication slot
                             └──> Memory store RWMutex freeze

Restore engine ──┬──> Restore in-place (requires maintenance mode)
                 ├──> Restore to new store (differentiator)
                 └──> Cross-engine restore (requires engine-neutral IR)

Async REST job ──┬──> CLI --wait / --async
                 ├──> Progress endpoint
                 └──> Webhook notifications (future)

Encryption at rest ──> Control-plane secret key mgmt ──> External KMS (future)

Prometheus metrics ─> Existing metrics subsystem
OTel tracing ──────> Existing telemetry subsystem
```

### Dependency Notes

- **Incremental requires manifest v2:** MVP ships manifest v1 **with a version field**. Adding incrementals later without breaking v1 backups requires forethought now, not later.
- **Cross-engine restore requires engine-neutral format:** JSON/protobuf IR is mandatory — binary Badger dumps cannot restore into Postgres. JuiceFS solved this with a **dual format**: JSON (universal, slow) + binary (same-engine, fast). Recommend mirroring.
- **Scheduled backups require repo persistence:** Schedule lives in control-plane DB (already GORM-backed), not CLI args. Scheduler reads on startup, reacts to config changes (mirror the existing `SettingsWatcher` 10s polling pattern).
- **Restore-in-place requires store drain:** Conflicts with accepting mounts/ops during restore. Design must include per-store "maintenance mode" (reject ops or queue them, surface via REST + CLI status).
- **Async REST + CLI `--wait` default:** CLI polls transparently and streams progress. `--async` returns immediately with job ID. Matches `gh run watch`, `aws s3 cp --progress`, `kubectl rollout status`.
- **Retention evaluation must be atomic with listing:** Concurrent list + prune can show disappearing backups. Evaluate retention under a repository-level lock.
- **Manifest-last commit on S3:** An S3 backup is "complete" iff the manifest object exists; listing ignores payload objects without a manifest. This gives atomicity without transactions.

## MVP Definition

### Launch With (v0.13.0)

Minimum to credibly close "no metadata DR path exists today" gap.

- [ ] **BackupRepository model** — per-metadata-store config in control-plane DB: driver (local|s3), destination config, optional cron schedule, retention (count + age), optional encryption key ref
- [ ] **Manifest format v1 (versioned, self-describing)** — engine type/version, schema version, timestamp, SHA-256, size, item counts, encryption metadata
- [ ] **Consistent snapshot per engine** — BadgerDB stream backup, Postgres `pg_dump` or snapshot-isolation export, Memory RWMutex-locked dump
- [ ] **Full backup only** — no incremental in MVP (simplifies manifest, defers dedup)
- [ ] **Local FS driver** — atomic tmp+rename, documented directory layout
- [ ] **S3 driver** — versioned keys, manifest-last commit, LocalStack E2E coverage
- [ ] **On-demand backup CLI** — `dfsctl store metadata <name> backup [--repo <id>] [--wait/--async] [--dry-run]`
- [ ] **List backups CLI** — `dfsctl store metadata <name> backup list [--repo <id>] [-o table|json|yaml]`, columns: ID, timestamp, size, status, repo, checksum-ok
- [ ] **Restore CLI** — `dfsctl store metadata <name> restore [--from <id>] [--repo <id>] [--force]`, default = latest successful
- [ ] **Count + age retention** — applied post-backup, never deletes the only successful backup
- [ ] **In-process scheduler** — robfig/cron/v3, per-repository cron, survives restart via DB
- [ ] **Async REST API** — `POST /repositories/{id}/backups` → 202 + job URL, `GET /backup-jobs/{id}`, `GET /repositories/{id}/backups`, `POST /repositories/{id}/restores`
- [ ] **Restore in-place** with store drain + maintenance mode
- [ ] **Integrity verify on restore** — reject mismatched SHA-256 unless `--skip-verify`
- [ ] **Prometheus metrics** — last-success-timestamp, duration histogram, bytes, failure counter
- [ ] **Structured logging** — `backup.started`, `backup.completed`, `backup.failed`, `restore.*`, `retention.pruned`
- [ ] **Unit + E2E tests** — conformance suite per engine, round-trip backup→restore→diff, retention scenarios, scheduler trigger, S3 via LocalStack

### Add After Validation (v0.13.x / v0.14)

- [ ] **Restore to new store** — staging restore, forensic workflows
- [ ] **Cross-engine restore** — JSON engine-neutral path, migration story
- [ ] **Test-restore / verify command** — restore-to-ephemeral + tree diff
- [ ] **Client-side encryption at rest** — symmetric key from control-plane secrets
- [ ] **GFS retention** — daily/weekly/monthly/yearly keeps
- [ ] **OpenTelemetry spans** — matches existing telemetry pattern
- [ ] **Progress reporting detail** — bytes written, items processed, ETA

### Future Consideration (v1.x+)

- [ ] **Incremental backups** — manifest v2, content-addressed chunking
- [ ] **Resumable uploads** — requires chunking
- [ ] **External KMS integration** — AWS KMS, Vault, AD
- [ ] **Webhook notifications** — Slack/PagerDuty on backup outcomes
- [ ] **Pre/post hooks** — shell/HTTP callbacks
- [ ] **Additional drivers** — GCS, Azure Blob, SFTP
- [ ] **Multi-node / HA-aware backup** — contingent on DittoFS multi-node support landing

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost | Priority |
|---------|------------|---------------------|----------|
| BackupRepository model + schema | HIGH | LOW | P1 |
| Manifest v1 | HIGH | LOW | P1 |
| On-demand backup (CLI + REST) | HIGH | MEDIUM | P1 |
| List backups | HIGH | LOW | P1 |
| Restore latest | HIGH | MEDIUM | P1 |
| Restore by ID | HIGH | LOW | P1 |
| Local FS driver | HIGH | LOW | P1 |
| S3 driver | HIGH | MEDIUM | P1 |
| Count + age retention | HIGH | LOW | P1 |
| Scheduled backup (cron) | HIGH | MEDIUM | P1 |
| Async REST + polling | HIGH | MEDIUM | P1 |
| Consistent snapshot | HIGH | MEDIUM-HIGH | P1 |
| Integrity checksum | MEDIUM | LOW | P1 |
| Prometheus metrics | MEDIUM | LOW | P1 |
| Restore in-place with drain | HIGH | MEDIUM | P1 |
| Dry-run | MEDIUM | LOW | P1 |
| Restore to new store | MEDIUM | MEDIUM | P2 |
| Cross-engine restore | MEDIUM | HIGH | P2 |
| Test-restore / verify | MEDIUM | MEDIUM | P2 |
| Encryption at rest | MEDIUM | MEDIUM | P2 |
| GFS retention | MEDIUM | MEDIUM | P2 |
| OTel tracing | LOW | LOW | P2 |
| Incremental backup | MEDIUM | HIGH | P3 |
| Resume uploads | LOW | HIGH | P3 |
| External KMS | LOW | MEDIUM | P3 |
| Webhooks | LOW | LOW | P3 |
| Pre/post hooks | LOW | LOW | P3 |

**Priority key:** P1 must ship in v0.13.0. P2 fast-follow. P3 future.

## Competitor Feature Analysis

| Feature | JuiceFS | etcd / k3s / RKE2 | restic / kopia | Proposed DittoFS v0.13.0 |
|---------|---------|-------------------|----------------|--------------------------|
| On-demand backup | `juicefs dump` → JSON/binary | `etcdctl snapshot save`, `k3s etcd-snapshot save` | `restic backup` | `dfsctl store metadata <name> backup` |
| Scheduled | Auto every 1h (default), tunable | `--etcd-snapshot-schedule-cron` | External (cron/systemd) | In-process cron per-repository |
| Restore | `juicefs load` into empty DB | `etcdctl snapshot restore`; k3s `--cluster-reset-restore-path` | `restic restore <id>:<path>` | `dfsctl store metadata <name> restore [--from <id>]` |
| List | Repo dir listing | `k3s etcd-snapshot ls`; `ETCDSnapshotFile` CRDs | `restic snapshots`, `kopia snapshot list` | `backup list` table + `-o json/yaml` |
| Retention | Ring buffer (keeps last N) | `--etcd-snapshot-retention` (count) | `forget --keep-last/daily/weekly/monthly/yearly` + prune | Count + age at MVP; GFS fast-follow |
| Destination | Same object store as data | Local, S3 | Local, S3, SFTP, REST, B2, Azure, GCS | Local FS + S3 at MVP |
| Consistency | Offline preferred; online best-effort | MVCC snapshot (no-quiesce) | Live — chunking | Engine-dependent: Badger stream, PG txn, Memory RWMutex |
| Encryption at rest | Repo-level (RSA for sensitive fields) | None (relies on disk-level) | Built-in AES-256 | Fast-follow (not MVP) |
| Integrity verify | JSON schema validate on load | SHA256 in snapshot status | `restic check` | SHA-256 manifest verify on restore (MVP); verify-restore cmd fast-follow |
| Cross-engine | Yes — JSON portable across Redis/MySQL/Postgres | N/A (etcd-only) | N/A | Fast-follow; engine-neutral JSON IR |
| Async job API | CLI blocks | CLI blocks | CLI blocks | REST 202 + polling; CLI `--wait` (default) / `--async` |
| Incremental | No (full dump) | No (full snapshot) | Yes (chunked CAS) | Future (manifest v2) |
| Metrics | Yes (Prometheus) | Yes | External wrappers | Yes (MVP) |

**Positioning statement:** match JuiceFS's dump/load + auto-backup baseline, add etcd/k3s-style cron + retention, borrow restic/kopia's repository-as-first-class-object model and integrity verification. Defer restic/kopia's chunked dedup (incremental, resume, CAS) to v1.x — metadata stores are small enough that full backup + good scheduling covers the DR story for v0.13.0.

## Expected Operator "Good" Behavior

From PROJECT.md's enterprise Kubernetes-first positioning:

1. **Configure once via CRD / `dfsctl`, forget forever.** Cron + retention + destination = declarative state.
2. **`kubectl logs` or Prom scrape tells me backup health without opening a shell.** Metrics + structured logs with stable event names.
3. **Panic restore is a single command.** `dfsctl store metadata X restore` — no flags — restores newest successful, rejects on checksum mismatch.
4. **Staging restore is possible.** Restore-to-new-store (fast-follow) enables "test before cut-over."
5. **Backups never silently fail.** Failed = metric bump + alertable event + non-retained slot.
6. **S3-compatible works out of the box.** Backblaze B2, Wasabi, MinIO, Cloudflare R2 all supported via the same S3 driver.
7. **No surprises under load.** Backup does not pause serving I/O (consistent snapshot, not stop-the-world dump).
8. **UI parity.** dittofs-pro can list, trigger, restore — REST designed around this, not retrofitted.

## Sources

- [JuiceFS Metadata Backup & Recovery](https://juicefs.com/docs/community/metadata_dump_load/)
- [JuiceFS v1.0 RC1 automatic metadata backup](https://juicefs.com/en/blog/release-notes/juicefs-release-v1-rc1)
- [restic forget / retention policy](https://restic.readthedocs.io/en/stable/060_forget.html)
- [restic backup docs](https://restic.readthedocs.io/en/stable/040_backup.html)
- [kopia snapshot list UX / Restic vs Kopia comparison](https://computingforgeeks.com/borg-restic-kopia-comparison/)
- [k3s etcd-snapshot CLI (manual + scheduled)](https://docs.k3s.io/cli/etcd-snapshot)
- [RKE2 backup & restore](https://docs.rke2.io/datastore/backup_restore)
- [Kubernetes configure / upgrade etcd](https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/)
- [MinIO KMS backup & recovery](https://docs.min.io/enterprise/minio-kms/operations/backup-and-recovery/)
- [Full vs incremental vs differential — Bareos](https://www.bareos.com/backup-strategy-full-incremental-differential/)
- [AWS — incremental vs differential backup](https://aws.amazon.com/compare/the-difference-between-incremental-differential-and-other-backups/)
- [resticprofile retention reference](https://creativeprojects.github.io/resticprofile/reference/profile/retention/index.html)

---
*Feature research for: metadata backup/restore in DittoFS v0.13.0*
*Researched: 2026-04-15*
