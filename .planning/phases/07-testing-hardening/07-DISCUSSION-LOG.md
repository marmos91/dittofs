# Phase 7: Testing & Hardening - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-17
**Phase:** 07-testing-hardening
**Areas discussed:** Test placement, Chaos test mechanism, Cross-version test scope, Corruption test vectors

---

## Test Placement

| Option | Description | Selected |
|--------|-------------|----------|
| Split across layers | Corruption + concurrent-write as pkg/backup integration tests; E2E matrix + chaos in test/e2e/ | ✓ |
| All in pkg/backup integration | Everything as //go:build integration, no server process, CLI/REST untested | |
| All in test/e2e/ | Everything as //go:build e2e with real server processes, heavy CI cost for micro-tests | |

**User's choice:** Split across layers
**Notes:** Mirrors how Phase 3 destination tests are already split from E2E. Corruption micro-tests don't need a full server.

---

## Chaos Test Mechanism

| Option | Description | Selected |
|--------|-------------|----------|
| Process kill in test/e2e/ | helpers.ForceKill() mid-run on real dfs process (SIGKILL) | ✓ |
| Context cancel in integration tests | Cancel executor context mid-run, faster but misses DB orphan recovery | |
| Both layers | Context-cancel in pkg/backup/ + process kill in test/e2e/ | |

**User's choice:** Process kill in test/e2e/
**Notes:** Ensures SAFETY-02 orphan recovery (running→interrupted on restart) and S3 ghost MPU cleanup are exercised with real DB state.

---

## Cross-Version Test Scope

| Option | Description | Selected |
|--------|-------------|----------|
| Manifest version gating only | Synthetic manifest_version:2 test → ErrManifestVersionUnsupported | ✓ |
| Old binary restore test | Build older dfs binary, backup with it, restore with current binary | |
| Just the 3×2 matrix | Treat "cross-version" as cross-engine × cross-destination only | |

**User's choice:** Manifest version gating only
**Notes:** Covers SAFETY-03 forward-compat without old-binary CI complexity.

---

## Corruption Test Vectors

| Option | Description | Selected |
|--------|-------------|----------|
| Table-driven suite in pkg/backup/destination/ | Single table iterating all 5 vectors, integration build tag | ✓ |
| Extend backup_conformance.go | Add vectors to the existing storetest conformance suite | |

**User's choice:** Table-driven suite in pkg/backup/destination/
**Notes:** Corruption is a destination-driver concern; adding it to backup_conformance.go would require engine implementations to stub destination logic.

---

## Claude's Discretion

- Exact test file names (backup_matrix_test.go, backup_chaos_test.go, backup_corruption_integration_test.go)
- Payload size for mid-kill determinism (researcher picks size reliably > 500ms on CI)
- Whether manifest-version-gating test lives in pkg/backup/manifest/ or pkg/backup/destination/
- Ghost MPU assertion approach (direct ListMultipartUploads vs. indirect re-run)
- Number of concurrent writes in byte-compare test

## Deferred Ideas

- Old-binary restore test — high CI complexity
- Context-cancel chaos layer in integration tests — process kill sufficient
- Prometheus metrics for backup operations (OBS-01)
- Automatic test-restore job (AUTO-01)
