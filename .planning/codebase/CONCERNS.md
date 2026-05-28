# Codebase Concerns

**Analysis Date:** 2026-05-28

> Scope note: this document captures forward-looking concerns for the v1.0 audit. Concrete protocol-conformance gaps tracked in GitHub issues / KNOWN_FAILURES manifests are not duplicated here — those are the source of truth.

## Tech Debt

### Durable Handle Store: single-tenant assumption

**Files:** `pkg/adapter/smb/adapter.go:516`

**Issue:** TODO marks that the SMB adapter resolves a single `DurableHandleStore` for all shares. Multi-store topologies (different metadata backends per share) require per-share resolution.

**Impact:** With heterogeneous metadata backends, durable-handle reconnect could hit the wrong store. The SMB DH V1 implementation (PR #661) is shipped against this assumption.

**Fix approach:**
1. Plumb `Runtime.MetadataServiceForShare(share)` through SMB session/tree manager.
2. Resolve durable-handle store at reconnect time, not at adapter init.
3. Add cross-store DH conformance to E2E (#663 tracks a related schema gap).

**Priority:** Medium

---

### NFSv4.1 session-limit settings not wired

**Files:** `pkg/controlplane/models/adapter_settings.go:72`

**Issue:** TODO documents that NFSv4.1 session-limit fields exist on the adapter-settings model but are not yet read by the StateManager.

**Impact:** Operators can set these knobs via the API but they have no effect.

**Fix approach:**
1. Either wire them into NFSv4.1 state manager and document them, or remove the fields (per memory: "less is more — minimize feature surface").
2. If wired, hook into `SettingsWatcher` for live updates.

**Priority:** Low

---

### NSM (Network Status Monitor) callback dispatch stubbed

**Files:** `internal/adapter/nfs/nsm/handlers/notify.go:34`

**Issue:** Inbound NSM NOTIFY accepted but not dispatched to local NLM clients.

**Impact:** Crash-recovery notifications from peers do not propagate to local NLM lock holders. Tradeoff acceptable while NLM remains lightly used (NFSv3 advisory locks).

**Fix approach:**
1. Track local clients watching each host.
2. On NOTIFY, fan out to matching lock-holders so they can reclaim.

**Priority:** Low

---

### Per-share rate limiting

**Issue:** Adapter-level rate limiting is global (token bucket per protocol). No per-share or per-user buckets.

**Impact:** Noisy-neighbor isolation impossible in multi-tenant deployments.

**Fix approach:**
1. Move rate limiter into per-share adapter context.
2. Read limits from share / adapter settings.
3. SLO metric per share.

**Priority:** Low (single-tenant assumption holds for v1.0).

---

## Known Bugs

### Pre-existing reopen1a watchdog hang

**Files:** SMB v2 handler, durable handle V1 reopen path (PR #661 context).

**Symptom:** `reopen1a` smbtorture test occasionally hangs the watchdog rather than failing fast.

**Status:** Pre-existing; surfaced but not introduced by #661 / #431. Tracked in project memory.

**Workaround:** None at code level. CI run-time guard is the watchdog itself.

**Priority:** Medium

---

### Postgres durable-handle schema gap (#663)

**Issue:** Postgres metadata store missing schema columns for durable-handle V1 fields (`OriginalFileID`, `PositionInfo`, `DELETE_ON_CLOSE-at-disconnect`). Documented from PR #661 follow-up.

**Impact:** DH V1 reconnect partially unsupported against Postgres backend.

**Fix approach:**
1. Add migration adding the missing columns.
2. Run conformance suite against Postgres.

**Priority:** Medium

---

## Security Considerations

### No production security audit

**Risk:** Authentication, authorization, and data-protection pathways have not been audited by external security experts.

**Mitigations in place:**
- AUTH_UNIX credentials extracted from client TCP connection only (no over-the-wire trust).
- User lookup goes through control-plane store.
- JWT REST auth standard implementation.
- SMB signing + encryption implemented (AES-CMAC, AES-GMAC, AES-CCM, AES-GCM) via `internal/adapter/smb/{signing,encryption}/`.
- Kerberos available for NFSv4 (RPCSEC_GSS) and SMB.

**Recommendations:**
1. Deploy behind VPN / encrypted tunnel for untrusted networks.
2. Use network-level firewall for client access.
3. Never expose REST API to untrusted networks without TLS-terminating proxy.
4. Commission third-party audit before production use with sensitive data.

**Priority:** High (for any production deployment).

---

### REST API default bind

**Files:** `pkg/config/defaults.go`, `pkg/controlplane/api/server.go`

**Issue:** API listens on `0.0.0.0:8080` by default; metrics on `0.0.0.0:9090`. No built-in TLS.

**Mitigations:**
- JWT required for authenticated endpoints.
- TLS expected via reverse proxy in production.

**Recommendations:**
1. Document explicitly that API should never be exposed to untrusted networks.
2. Consider defaulting to `127.0.0.1` and requiring opt-in to 0.0.0.0.
3. Add config validation warning when binding 0.0.0.0 in non-container environments.

**Priority:** Medium

---

## Fragile Areas

### Engine syncer + reference-CAS GC

**Files:** `pkg/blockstore/engine/syncer.go`, `gc.go`, `gcstate.go`, `sync_health.go`, `coordinator.go`

**Why fragile:**
- Concurrent dedup, upload, and GC interact through shared hash sets + HoldProvider scoping.
- Reference-CAS snapshots add a "ready hold" lifecycle that GC must honor (project memory: SNAP-01..05 design).
- Idempotent rollup on `local/fs` interacts with dedup-LRU on warm restarts.

**Safe modification:**
1. Add tests that interleave snapshot, GC, and concurrent writes.
2. Race-detector mandatory in `pkg/blockstore/engine`.
3. Document hold-provider invariants where they cross sub-package boundaries.

**Coverage:** Heavy (>20 test files in `engine/`), but post-#522/#527/#546 phases continue to surface edge cases.

**Priority:** Medium

---

### SMB session + channel + signing + encryption state

**Files:** `internal/adapter/smb/session/`, `signing/`, `encryption/`, `crypto_state.go`, `lease/`

**Why fragile:**
- SMB 3.x channel binding (#483) gates non-binding session-setups on the right state machine step (MS-SMB2 §3.3.5.5).
- Cross-channel lease/oplock break fan-out is split across `lease/notifier.go` and the lease manager.
- Sequence-window invariants (`session/sequence_window.go`) tie signing + credits + replay protection.
- Lease cluster rounds 1–7 (PRs #444–#464) added per-test invariants that are easy to regress.

**Safe modification:**
1. Always run smbtorture (`smb2.lease`, `smb2.session`, `smb2.compound`, `smb2.signing`, `smb2.encryption`) in addition to unit tests.
2. Use the canonical Samba pcap diff workflow (see CLAUDE.md "Debugging protocol interop").
3. Keep KNOWN_FAILURES manifest tracked only after CI confirms flips.

**Coverage:** Dense but uneven; cross-channel + multi-channel still incomplete (Phase 2 of #361).

**Priority:** High (security + correctness, plus active milestone).

---

### Metadata store: ACL canonicalization + inheritance

**Files:** `pkg/metadata/acl/`, particularly `inherit.go`, `synthesize.go`, `generic.go`

**Why fragile:**
- Windows DACL canonicalization vs. wire-canonical order (UI convention) is subtle (PR #510, #514, #520, #524).
- GENERIC_* expansion on inherited DACLs (#591) keeps surfacing `0x000e0002 vs 0x001f01ff` family failures.
- Per-share `AclFlagInheritedCanonicalization` toggle introduces a GORM bool zero-value pitfall (#514).

**Safe modification:**
1. Run full smbtorture ACL suite + WPTS BVT after changes.
2. Compare against Samba `calculate_inherited_from_parent` and `desc_expand_generic`.
3. Add fixture-based unit tests for canonicalization toggles.

**Priority:** Medium

---

### Postgres metadata store transactions

**Files:** `pkg/metadata/store/postgres/`

**Why fragile:**
- Link counts in a separate table; silly-rename + cross-dir mkdir/rmdir need GREATEST() protection.
- SGID inheritance + ACL canonicalization gated through this backend's transaction layer.
- Distributed handle encoding must remain stable across restarts.

**Safe modification:**
1. Run `pkg/metadata/storetest` conformance suite against Postgres (testcontainers).
2. pjdfstest pass via E2E.
3. Add concurrent stress tests for link-count operations.

**Priority:** Low (well-tested but high blast radius).

---

## Performance / Scaling

### Single-node scope

**Current state:** One server process. No replication, no HA at the server layer.

**Mitigations:**
- Postgres metadata + S3 remote store give durability.
- Cluster-level HA is operator concern (failover via K8s).

**Path to scale:**
- Phase 22 reference-CAS snapshots (shipped in #665) unlock backup/restore.
- True multi-node serving remains out of scope for v1.0.

**Priority:** Low for v1.0; documented as a known limit.

---

### Random-write workloads at scale

**State:** Phase 19 (#546) and the append-log + idempotent rollup on `local/fs` materially improved random writes. Bench infra (Pulumi `bench/`) tracks ns/op deltas.

**Watchpoint:** D-21 sentinel-zero behavior is observation-only; if it triggers in production, raise alarms before optimizing.

**Priority:** Watch, not act.

---

## Missing Critical Features

### NFSv4.1 session-limit enforcement

See `pkg/controlplane/models/adapter_settings.go:72`. Settings exist; enforcement does not.

**Priority:** Low (delete or wire — see CLAUDE memory "less is more").

---

### Cross-channel broadcast for multi-channel SMB

**Files:** Phase 2 of multi-channel (#361 follow-up).

**Status:** Phase 1 shipped in PR #404 (channel binding, per-channel sign/verify, CAP_MULTI_CHANNEL advertised). Phase 2 needs cross-channel lease + oplock break fan-out, `FSCTL_QUERY_NETWORK_INTERFACE_INFO`, wide-channel coordination. ~10 multichannel smbtorture tests in KNOWN_FAILURES.

**Priority:** Medium (active milestone follow-up).

---

## Test Coverage Gaps

### Concurrent metadata operations under contention

**Risk:** Race conditions in concurrent file create / rename / chmod / hard-link.

**Recommended additions:**
1. Parallel `CreateFile` on same directory across backends.
2. Simultaneous rename + delete on same handle.
3. Parallel chmod stress (race detector + sanitizers if available).

**Priority:** Medium

---

### S3 chaos / failure injection

**Files:** `pkg/blockstore/remote/s3/`, `engine/syncer.go`

**Risk:** Transient S3 errors during multipart uploads — partial fail paths are exercised but not chaos-tested.

**Recommended additions:**
1. Inject 5xx and throttling at random points.
2. Verify orphaned multipart cleanup on server restart.
3. Validate retry/backoff observability.

**Priority:** Medium

---

### Large-file boundary correctness

**Risk:** Off-by-one in chunker/blockstore arithmetic for files >1 TB.

**Recommended additions:**
1. Boundary tests at FastCDC chunk edges + multi-chunk spans.
2. Sparse-file tests with very large holes.

**Priority:** Low (FastCDC + CAS naturally robust, but worth coverage).

---

## Dependencies at Risk

**Cobra / Viper:** Stable. No migration warranted.

**GORM:** Acceptable abstraction; we audit hot queries with EXPLAIN and ship raw SQL when needed.

**AWS SDK v2:** Standard. Monitor advisories; pin versions.

**Kerberos (`gokrb5`):** Critical for SMB + NFSv4 GSS. Limited maintainer count. Monitor for security advisories.

---

## Notable Recent Architectural Bets (project memory, audit context)

- v0.15.0: FastCDC chunker + CAS write path + GC.
- v0.16.0: Unified `BlockStore`, RAM-only cache (mmap WAL deleted), syncer mirror loop, write-path RAM optimizations.
- Phase 22 (PR #665): reference-CAS snapshots replacing the v0.13 backup; verifier 5/5; one HIGH finding (H-2) deferred for follow-up.
- SMB lease cluster (rounds 1–7): all smb2.lease residuals cleared; rename/stat/byte-range lock paths flipped.
- SMB notify wave 1 (PR #658): armed-handle event buffering; cancel-vs-dispatcher race fixed.
- SMB DH V1 (PR #661): foundation for DH V2 (#432); see "Durable Handle Store: single-tenant assumption" above.

These bets are stable but each remains an area where regressions are easy to introduce.

---

*Concerns audit: 2026-05-28*
