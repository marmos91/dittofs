# Project Research Summary

**Project:** DittoFS v0.13.0 — Metadata Backup & Restore (issue #368)
**Domain:** Disaster-recovery subsystem for pluggable metadata stores in a live multi-protocol (NFSv3/v4.x + SMB3) filesystem server
**Researched:** 2026-04-15
**Confidence:** HIGH

## Executive Summary

This milestone closes the DR gap left by `dfs backup controlplane` (config-only) by adding first-class, per-metadata-store backup/restore with local-FS and S3 destinations, on-demand + scheduled triggers, retention policies, and a REST/CLI/UI surface. Research across the ecosystem (JuiceFS, etcd/k3s/RKE2, restic/kopia) shows a converged design: **per-store repository config → atomic consistent snapshot → tar+manifest envelope → versioned destination → async job with polling → separate retention pass**. DittoFS already ships every heavy dependency needed (BadgerDB, pgx, AWS S3 SDK v2, GORM, Cobra, OpenTelemetry, Localstack test plumbing); **only one new direct dependency is justified — `robfig/cron/v3`** for cron-expression scheduling.

The recommended approach is a new `runtime/backups` sub-service (parallel to existing `adapters/`, `stores/`, `shares/`, `mounts/`, `lifecycle/`, `identity/`) owning a scheduler, driver registry, and orchestrator. Metadata stores opt into a new optional `Backupable` capability interface: BadgerDB uses native `DB.Backup/Load` (SSI snapshot), PostgreSQL uses `pgx` `COPY TO STDOUT (FORMAT binary)` inside a single `REPEATABLE READ` txn (avoids shelling out to `pg_dump`), memory store exports GORM-JSON. Backup destinations are a **new** `pkg/backup/destination/` package — deliberately **not** reused from `pkg/blockstore/remote/` since the semantics (immutable archive objects with retention) differ fundamentally from block-addressable chunks. Repository config is a sibling GORM entity (1:N to `MetadataStoreConfig`), matching issue #368's "persisted alongside store config so triggers and scheduler are stateless consumers" requirement verbatim.

The top risks are all silent-corruption / silent-failure modes that plague production backup systems: **(1)** restoring into a live-mounted share poisons client caches (file handles outlive the restore); **(2)** the restored metadata references block-store state that may have been GC'd between backup and restore; **(3)** scheduled backups that fail every night for months before anyone notices at restore time. Mitigations are baked into the architecture: quiesce-swap-resume with NFSv4 boot-verifier bump + SMB durable-handle clear, manifest-recorded `PayloadID` sets with block-GC hold integration, Prometheus `backup_last_success_timestamp_seconds` heartbeat with documented alert rule, ULID backup IDs, two-phase commit on S3, retention as a separate post-upload pass that always retains ≥1 successful backup.

## Key Findings

### Recommended Stack

DittoFS's existing go.mod already covers 95% of the surface. The only new direct dependency is `robfig/cron/v3` (de-facto Go cron standard, zero transitive deps, supports `CRON_TZ=` per-schedule timezones). Everything else maps to existing deps or stdlib: Badger's `DB.Backup`/`DB.Load`, `pgx.Conn.CopyTo` for PostgreSQL, `aws-sdk-go-v2/service/s3` + `manager.Uploader` for S3 (**reuse `pkg/blockstore/remote/s3`'s constructor**, don't fork), `crypto/aes`+`crypto/cipher` for optional client-side AES-256-GCM encryption, `archive/tar` + `gopkg.in/yaml.v3` + `crypto/sha256` for the manifest envelope, `golang-migrate/migrate/v4` for the new `backup_repositories`/`backup_runs`/`backup_jobs` tables.

**Core technologies:**
- `robfig/cron/v3` (NEW) — in-process cron-expression parser + runner; matches PROJECT.md per-store schedule model
- `dgraph-io/badger/v4` `DB.Backup/Load` (reuse) — SSI-snapshot, safe under concurrent writes, supports incremental via `sinceTs`
- `jackc/pgx/v5` `CopyTo` (reuse) — streaming binary `COPY` from `REPEATABLE READ` txn; avoids shipping `postgresql-client` in container
- `aws-sdk-go-v2/service/s3` + `manager` (reuse) — multipart upload, same client construction as blockstore
- `crypto/cipher` AES-256-GCM (stdlib) — default client-side encryption; rejected `filippo.io/age` as unnecessary dep
- `archive/tar` + `yaml.v3` manifest envelope (stdlib + existing) — `{manifest.yaml, payload/*, checksums.sha256}`; mirrors existing `dfs backup controlplane` pattern

### Expected Features

Feature landscape converges across JuiceFS, etcd, k3s/RKE2, restic, kopia. MVP must match JuiceFS dump/load + auto-backup baseline, adopt etcd/k3s cron + retention, and borrow restic/kopia's repository-as-first-class-object + integrity verification. Defer CAS dedup/incremental to v1.x — metadata stores are small enough that full backup + good scheduling covers DR for v0.13.0.

**Must have (table stakes):**
- On-demand + scheduled (cron) backup per store
- Restore latest by default, restore by ID via `--from`
- List backups with stable time-ordered IDs (ULIDs), `-o table|json|yaml`
- Local FS + S3 destinations (S3-compatible covers MinIO/B2/Wasabi/R2)
- Count + age retention; never delete the only successful backup
- Consistent snapshot per engine (Badger native, Postgres MVCC, Memory RWMutex)
- Restore in-place with store drain + maintenance mode; `409 Conflict` if any active mount
- Async REST API (202 + polling) with CLI `--wait` default / `--async`
- Integrity: SHA-256 on write + verify on restore
- Atomic completion: manifest-last on S3, tmp+rename on local FS
- Prometheus metrics (`backup_last_success_timestamp_seconds` is non-negotiable)
- Structured logs + self-describing manifest v1 with version field

**Should have (competitive differentiators):**
- Restore to a **new** metadata store (staging restore, forensic workflows)
- Cross-engine restore (JSON engine-neutral IR — JuiceFS-style killer feature)
- Test-restore / verify command (`restic check` analog)
- Client-side encryption at rest (AES-256-GCM, operator-supplied or KMS key)
- GFS retention (hourly/daily/weekly/monthly/yearly)
- OpenTelemetry spans matching existing telemetry pattern
- Backup repository as first-class object (one store → multiple repos for 3-2-1)

**Defer (v0.14+ / v1.x):**
- Incremental backups (requires manifest v2 — but version field in v1 now for forward compat)
- Resumable uploads (requires CAS chunking)
- External KMS integration (Vault, AWS KMS beyond SSE-KMS pass-through)
- Webhooks, pre/post hooks
- Additional destination drivers (GCS, Azure Blob, SFTP)
- Multi-node / HA-aware backup

**Anti-features (explicitly rejected):**
- Continuous PITR / WAL streaming (scope explosion; cron + incremental covers RPO)
- Backup of block-store data (delegated to S3 versioning/lifecycle)
- Backup via NFS/SMB mount to self (reentrant risk)
- Silent deletion of the last remaining backup
- Automatic restore on corruption detection

### Architecture Approach

New `pkg/controlplane/runtime/backups/` sub-service composes: `service.go` (orchestrator), `scheduler.go` (cron, tied to `Runtime.Serve` lifecycle), `jobs.go` (state-machine tracking), `quiesce.go` (share pause/resume), `registry.go` (driver factory). A new `Backupable` optional interface on `MetadataStore` keeps per-engine snapshot logic next to each backend. Destination drivers live in a new public `pkg/backup/destination/{fs,s3}` package — distinct from `pkg/blockstore/remote/` (different key-space, different lifecycle, different IAM surface), but internally sharing AWS client construction via a factored `internal/awsclient` helper. Repository config is a sibling GORM entity with a new `BackupRepoStore` sub-interface embedded in the composite `Store` (matches existing 9-sub-interface pattern).

**Major components:**
1. **Models** — `BackupRepoConfig`, `BackupRecord`, `BackupJob` (new GORM entities + migration)
2. **`Backupable` interface** — optional capability on `MetadataStore`; memory/badger/postgres implementations
3. **Destination drivers** — `pkg/backup/destination/{fs,s3}`, separate from blockstore remote
4. **`runtime/backups` sub-service** — orchestrator + scheduler + job tracker + driver registry
5. **Quiesce hooks** — new `shares.Service.DisableShare/EnableShare`, `stores.Service.CloseStore/ReopenStore`, `MetadataService.PauseShare/ResumeShare`
6. **REST + CLI + apiclient** — `POST /stores/metadata/{name}/backups` (202 async), `GET /backup-jobs/{id}`, `backup list/show/delete`, `backup repo add/edit`, `restore`; matching Cobra subtree under `dfsctl store metadata <name>/backup/` and sibling `restore`

### Critical Pitfalls

1. **Restore while shares are mounted → client cache poisoning + stale-handle data corruption.** Handles outlive restore; NFSv4 `change` attr goes inconsistent; SMB leases forgotten. **Mitigation:** default-deny on active mounts (`409 Conflict` with list), require `--force --drain`, bump NFSv4 boot verifier, clear SMB durable-handle registry + break all leases, document that clients must remount.
2. **Silent scheduled-backup failures** (industry's #1 backup failure mode). Cron fires, creds rotate, backup fails nightly for 5 months, admin discovers at restore time. **Mitigation:** `dittofs_backup_last_success_timestamp_seconds{store}` Prom metric + documented alert rule (`time() - last_success > 2 * interval`), structured `event=backup_completed status=...` logs, `backup list` shows failures, `dfs status` + `/healthz` surface backup freshness, K8s operator propagates to CR status.
3. **Block-store divergence** — backup taken Day 1, blocks GC'd Day 3 (reference-counted against live metadata), restore Day 4 → `NFS3ERR_IO` on every read of old files (S3 NoSuchKey). **Mitigation:** manifest records `PayloadID` set; block GC consults retained-backup manifests before deleting; for v0.13.0, minimum viable is to log warnings listing missing PayloadIDs and require explicit operator ack — but ship the manifest field from day one for forward compat.

Also critical but rank-adjacent: **inconsistent-snapshot under concurrent WRITEs** (use atomic engine APIs — `DB.Backup` not `cp -r`, `REPEATABLE READ` txn not key iteration); **cross-store contamination** (manifest with `store_id` UUID + `schema_version` + `dittofs_version`, restore refuses mismatch without `--force-cross-store`); **S3 retention-race data loss** (two-phase commit via `pending/` → `manifests/`, retention is a separate post-upload pass that never deletes a completed manifest unless replaced by a newer one).

## Implications for Roadmap

Based on dependency analysis across all four research outputs, a 6-phase structure emerges. Phase 01 is the foundation all others depend on; Phases 02–03 can run largely in parallel; Phase 04 depends on 01+02; Phase 05 depends on 03+04; Phase 06 is cross-cutting validation.

### Phase 01: Foundations — Models, Manifest, `store_id`, `Backupable` interface
**Rationale:** Schema and manifest are the contract every other phase depends on. Adding `store_id` now (persisted inside each metadata store) prevents the cross-store-contamination pitfall even before restore ships. Defining the manifest with version field + `block_store_refs` + `payload_id_set` up front is cheap insurance; bolting on later is painful.
**Delivers:** `BackupRepoConfig` / `BackupRecord` / `BackupJob` GORM models + migrations; new `BackupRepoStore` sub-interface embedded in composite `Store`; `pkg/metadata/backup.go` defining `Backupable` interface; `store_id` migration on all metadata store backends; manifest v1 format spec.
**Addresses:** Repository schema; manifest; store identity — foundation for every feature downstream.
**Avoids:** Pitfall #4 (cross-store contamination), partial #1 (manifest records block refs), partial #3 (manifest carries PayloadID set).

### Phase 02: Per-Store `Backupable` Drivers — Memory, BadgerDB, PostgreSQL
**Rationale:** The engine-specific atomic-snapshot APIs are the highest-risk correctness work. Isolating them before destination drivers means we can test round-trips in-process with no S3/local-FS coupling. BadgerDB and Postgres are the production backends; memory is for tests + cross-engine IR.
**Delivers:** BadgerDB driver using `DB.Backup(w, since)` / `DB.Load`; PostgreSQL driver using `pgx.Conn.CopyTo` with `COPY TO STDOUT (FORMAT binary)` inside `REPEATABLE READ` txn (keep shell-out to `pg_dump` as optional `--format native-cli`); memory store GORM-JSON export; storetest conformance suite extensions.
**Uses:** `dgraph-io/badger/v4` (existing), `jackc/pgx/v5` (existing).
**Avoids:** Pitfall #1 (inconsistent snapshot), #5 (wrong Badger API), #6 (Postgres long-txn bloat).

### Phase 03: Destination Drivers + Scheduler + Encryption Hooks
**Rationale:** Destinations, scheduling, and encryption can land in parallel with Phase 02 once manifest + repo schema are fixed. Two-phase commit on S3, lifecycle-rule validation, and the overlap mutex in the scheduler must all land together — they jointly prevent the retention-race and thundering-herd classes of failure.
**Delivers:** `pkg/backup/destination/{fs,s3}/` with Put/Get/List/Delete/Stat; tar+YAML envelope + SHA-256 streaming; two-phase commit (`pending/` → `manifests/`) on S3; `AbortIncompleteMultipartUpload` lifecycle validation via `dfsctl validate-repo`; `robfig/cron/v3` scheduler wired to `lifecycle.Service` Serve/Shutdown; per-repo mutex + jitter + missed-run policy + `CRON_TZ=` support; AES-256-GCM encryption hook + SSE-KMS pass-through; factored `internal/awsclient` helper shared with `pkg/blockstore/remote/s3`.
**Uses:** `robfig/cron/v3` (NEW), `aws-sdk-go-v2/service/s3/manager` (existing), `crypto/cipher` (stdlib).
**Avoids:** Pitfall #7 (scheduler overlap/DST/thundering-herd), #8 partial (S3 partial uploads + retention race), #9 (encryption + key management).

### Phase 04: Restore Orchestration + CLI/REST API
**Rationale:** The dangerous path. Depends on 01 (manifest for validation) + 02 (per-store `Restore()`) + 03 (destination `Get`). Quiesce-swap-resume must land with NFSv4 boot-verifier bump + SMB durable-handle clear as a single commit — partial implementations silently corrupt clients.
**Delivers:** `runtime/backups.Service.CreateBackup/CreateRestore`; new `shares.Service.Disable/EnableShare`, `stores.Service.Close/ReopenStore`, `MetadataService.Pause/ResumeShare`; temp-path + atomic rename swap; NFSv4 boot-verifier bump; SMB durable-handle registry clear + lease break-to-None; async `POST` → 202 + `GET /backup-jobs/{id}` polling; `pkg/apiclient/backups.go` typed client; Cobra subtree `dfsctl store metadata <name>/backup/{on-demand, list, show, delete, repo}` + sibling `restore`; ULID backup IDs; `--wait` default / `--async` / `--dry-run` / confirmation prompt on restore.
**Addresses:** On-demand backup, list, restore (latest + by-id), restore-in-place.
**Avoids:** Pitfall #2 (restore while mounted), #11 (CLI UX / Ctrl-C ghost jobs / unsortable IDs).

### Phase 05: Retention + Observability + Block-Store GC Integration
**Rationale:** Retention must be a separate pass that runs only after upload confirmed — collapsing it into the upload path creates the data-loss-by-pruner failure mode. Observability (heartbeat metric + alert rule) is what turns "silent failure for 5 months" into "PagerDuty at hour 49". Block-GC integration closes the divergence pitfall #3.
**Delivers:** Count + age retention evaluator (separate post-upload pass, never deletes only successful backup, respects `backup pin`); Prometheus `dittofs_backup_*` metrics suite (last-success-timestamp, duration histogram, size, jobs-inflight, retention-pruned counter); structured `backup.*` / `restore.*` log events; OpenTelemetry spans at orchestrator boundary; GC-hold in `pkg/blockstore/gc/` consulting retained-backup PayloadID sets.
**Avoids:** Pitfall #3 full (GC divergence), #8 full (retention race), #10 (silent failures).

### Phase 06: Test Matrix + Security Review + Documentation
**Rationale:** Cross-cutting validation. Should start in parallel with Phase 04 — test fixtures take time to build right, and the corrupt/partial/cross-version cases will surface bugs in earlier phases. Localstack E2E hygiene matters (shared container, not per-test — see MEMORY.md on `TestCollectGarbage_S3` flakiness).
**Delivers:** Integration + E2E matrix in `test/integration/backup/` and `test/e2e/backup/` covering happy path × 3 engines × 2 destinations + truncated / bit-flip / wrong `store_id` / schema-up / schema-down / missing-manifest / missing-payload / concurrent-write+backup+restore byte-compare / restore-while-mounted-refused / chaos (kill mid-backup & mid-restore); cross-version matrix (N-1 binary → current); threat model doc; `docs/BACKUP_RESTORE.md` (scope explicitly metadata only, not block data); mapping between `dfs backup controlplane` and `dfsctl store metadata backup`; update `CLAUDE.md`.
**Avoids:** Pitfall #12 (missing corruption/cross-version tests).

### Phase Ordering Rationale

- **Dependency chain:** Models/manifest (P1) gate everything. `Backupable` drivers (P2) and destinations+scheduler (P3) are parallelizable once P1 lands. Restore (P4) needs both. Retention+observability+GC (P5) needs upload paths from P3 and the job model from P4. Tests (P6) cross-cut and should start alongside P4.
- **Risk ordering:** The correctness-critical pieces (atomic snapshot APIs, restore orchestration, quiesce) land in P2 + P4. The silent-failure preventers (heartbeat metric, retention-as-separate-pass, GC-hold) land in P5. Security posture (encryption default-deny-plaintext) is woven through P3 + P6.
- **Issue #368 alignment:** Requirement that "triggers and scheduler are stateless consumers" of persisted state is met in P1 (schema) + P3 (scheduler reads DB only). CLI/REST/UI surface lands in P4. Per-store repository model lands in P1, matching "persisted alongside the store config" (sibling FK, not columns on `MetadataStoreConfig`).
- **Pitfall avoidance timing:** 7 of the 12 critical pitfalls (cross-store, wrong Badger API, Postgres bloat, scheduler edge cases, encryption, CLI UX, retention-race) are structurally prevented by choosing the right API/layout in P1–P3 — not by adding guards later. The remaining 5 (inconsistent snapshot, restore-while-mounted, block-GC divergence, silent failures, missing tests) have dedicated phase mappings above.

### Research Flags

Phases likely needing deeper research during planning:
- **Phase 02 (PostgreSQL driver):** `pgx.Conn.CopyTo` with `COPY (FORMAT binary)` round-trip semantics under heavy concurrent write load; confirm `REPEATABLE READ` vs `SERIALIZABLE DEFERRABLE` tradeoff for backup-worker txn.
- **Phase 04 (restore orchestration):** Exact NFSv4 boot-verifier semantics (RFC 7530 §3.3.1) — does bumping force reclaim-grace path correctly on every Linux kernel client in our matrix? Also SMB durable-handle + lease-break sequencing (MS-SMB2 §3.3.5.9.7).
- **Phase 05 (block-GC hold):** Integration surface with existing `pkg/blockstore/gc/` — API shape, persistence of hold set, interaction with per-share BlockStore ref counting.

Phases with standard patterns (skip research-phase):
- **Phase 01:** Standard GORM migration + sub-interface composition; existing 9-sub-interface pattern in `pkg/controlplane/store/interface.go` is the template.
- **Phase 03 scheduler subset:** `robfig/cron/v3` + overlap mutex + jitter is a well-documented pattern; no deeper research needed beyond confirming `CRON_TZ=` behavior under DST.
- **Phase 06:** Test-matrix construction; Localstack hygiene lessons already captured in MEMORY.md.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | All reused deps verified present at current versions in go.mod; `robfig/cron/v3` is de-facto standard with zero transitive deps; rejected alternatives (`gocron`, `filippo.io/age`, `pg_dump` shell-out, `minio-go`) have clear documented reasons. |
| Features | HIGH | Convergent feature set across JuiceFS, etcd/k3s/RKE2, restic/kopia — all well-documented. Competitive positioning is unambiguous. MVP/fast-follow/future split is grounded in priority matrix. |
| Architecture | HIGH | Grounded in direct source analysis of DittoFS runtime composition, store interface decomposition, and existing `dfs backup controlplane` precedent. Sub-service pattern matches 6 existing siblings. |
| Pitfalls | HIGH | DittoFS-specific pitfalls (block-GC divergence, boot-verifier, SMB durable-handles, cross-protocol mount quiesce) are derived from actual codebase architecture, not generic advice. Industry patterns (retention race, ghost multiparts, silent cron failure) are pattern-level synthesis of well-documented incident classes. |

**Overall confidence:** HIGH

### Gaps to Address

- **Exact wire-compatibility of `pgx` binary COPY round-trip:** needs a spike test early in Phase 02 — confirm all Postgres types in the metadata schema (enums, arrays, jsonb, bytea, timestamptz) round-trip losslessly via `COPY (FORMAT binary)`. Fallback path (per-model GORM JSON serializer) must be designed in P1's manifest format even if not implemented until later.
- **Cross-engine restore format (differentiator, P2-scope if pulled forward):** JuiceFS uses JSON for engine-neutral transport. Whether DittoFS ships a JSON IR in v0.13.0 or defers to v0.14 needs a product call during roadmap finalization. Recommend: ship engine-neutral-capable manifest format now, defer the cross-engine restore command to v0.14.
- **K8s operator integration for backup/restore triggers:** out of scope for v0.13.0 but architecture should not preclude it. Operator would patch `DittoServer.spec.paused=true`, wait for drain, then call `POST /restore`. Leave a note in `docs/BACKUP_RESTORE.md` pointing to this as future work.
- **Unified Lock Manager state in backup scope:** current stance is "locks are ephemeral, excluded from backup; restored state has no active locks (matches post-crash semantics)." This should be validated with the v1.0 lock-manager design authors before Phase 04 lands.

## Sources

### Primary (HIGH confidence)
- `pkg/controlplane/runtime/runtime.go`, `pkg/controlplane/store/interface.go`, `pkg/metadata/store.go`, `pkg/blockstore/remote/remote.go`, `pkg/controlplane/models/stores.go` — direct codebase analysis
- `cmd/dfs/commands/backup/controlplane.go` — existing backup pattern precedent (VACUUM INTO, `pg_dump`, GORM-JSON)
- `go.mod` — verified all reused dep versions
- BadgerDB `DB.Backup`/`DB.Load` docs — SSI snapshot, 4MB Stream framework batching
- PostgreSQL `COPY` docs and `pg_dump` serializable-deferrable semantics
- robfig/cron/v3 — timezone support, zero transitive deps, feature-frozen since 2019
- JuiceFS Metadata Backup & Recovery docs + v1.0 auto-backup release notes
- restic forget/retention, k3s etcd-snapshot CLI, RKE2 backup & restore
- kopia / restic / borg comparison
- RFC 7530 §3.3.1 (NFSv4 boot verifier), MS-SMB2 §3.3.5.9.7 (SMB durable-handle reconnect)

### Secondary (MEDIUM confidence)
- AWS S3 strong-LIST consistency announcement (Dec 2020); non-AWS S3-compatibles behavior variance — pattern-level
- Bareos full-vs-incremental-vs-differential backup taxonomy
- resticprofile retention reference for GFS algorithm

### Tertiary (LOW confidence — pattern-level)
- Industry post-mortem classes (retention race, ghost multipart uploads, encryption-key co-location, silent-cron-failure): synthesized from common DBaaS vendor incident reports
- DittoFS MEMORY.md — Localstack shared-container flakiness, operator credential-rotation loop; directly informs Pitfall #8 (credential refresh) and Pitfall #12 (test hygiene)

---
*Research completed: 2026-04-15*
*Ready for roadmap: yes*
*Detailed research: STACK.md, FEATURES.md, ARCHITECTURE.md, PITFALLS.md*
