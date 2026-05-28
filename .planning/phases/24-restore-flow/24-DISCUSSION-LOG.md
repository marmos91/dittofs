# Phase 24: Restore Flow - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-28
**Phase:** 24-restore-flow
**Areas discussed:** Lifecycle & orchestration shape, Metadata store reset semantics, Atomicity & interrupted-restore recovery, Verification gate & --no-sync-gate handling, Code structure / PR shape

---

## Lifecycle & orchestration shape

### Disable/Enable bracketing

| Option | Description | Selected |
|--------|-------------|----------|
| Operator-driven (CLI brackets it) | Plan-doc model: operator runs `dfsctl share disable` → `share snapshot restore` → `share enable`. Restore refuses if Enabled=true (`ErrShareEnabled`). | ✓ |
| Restore auto-disables, leaves disabled | Restore implicitly calls DisableShare at entry; on success leaves Enabled=false; operator explicitly EnableShare. | |
| Restore auto-disables AND auto-enables on success | Full envelope: disable → verify → swap → verify → enable. Failure leaves disabled. | |

**User's choice:** Operator-driven (CLI brackets it)
**Notes:** Explicit operator intent at every step; pairs naturally with REST-02; consistent with existing `shares/service.go::DisableShare/EnableShare` API surface (ErrShareAlreadyDisabled precedent).

### Sync vs async

| Option | Description | Selected |
|--------|-------------|----------|
| Sync (blocking call returns final state) | CLI/REST blocks until done. No goroutine registry, no WaitForRestore, no startup recovery. | ✓ |
| Async (202+poll, mirror CreateSnapshot) | Mirror Phase 23 async with restore-tracking row + goroutine registry + startup recovery. | |

**User's choice:** Sync
**Notes:** Restore is rare + operator-initiated + disaster-driven; Phase 23's async machinery was justified by routine multi-create patterns that don't apply to restore.

---

## Metadata store reset semantics

| Option | Description | Selected |
|--------|-------------|----------|
| New `Resetable` interface on MetadataStore | `Resetable interface { Reset(ctx) error }`. Per backend: memory re-init maps; badger DropAll; postgres TRUNCATE in tx. Symmetric with Backupable. | ✓ (Claude's choice per user delegation) |
| Recreate store: close → new instance → Restore() → re-register | Plan-doc literal flow. Requires shares.Service unregister/re-register dance + cached constructor args. | |
| Truncate-in-Restore: Backupable.Restore handles empty-dest itself | Push reset INTO each backend's Restore impl. Bends Phase 21's ErrRestoreDestinationNotEmpty contract. | |

**User's choice:** "Choose cleanest design here. The restore should be atomic between metadata and block stores. Use any skill you need to choose best option" (delegated to Claude)
**Notes:** Selected Resetable interface. Reasoning: avoids shares.Service re-register dance, avoids cached constructor args, no race window with nil-registered store, preserves Phase 21 ErrRestoreDestinationNotEmpty semantics (just-shipped), simpler test fixture, mirrors Phase 21 optional-interface pattern. "Atomicity between metadata + block stores" reinterpreted as "reachability invariant at re-enable time" — satisfied by post-verify gate (D-24-07); blocks are CAS-immutable + HoldProvider-pinned so no block-store reset needed.

---

## Atomicity & interrupted-restore recovery

### Recovery model

| Option | Description | Selected |
|--------|-------------|----------|
| Pre-restore safety snapshot of current state | Before Reset, call CreateSnapshot to capture current metadata. On failure: restore-from-safety-snapshot. | ✓ |
| Restore-in-progress marker on share row + manual recovery | Share.RestoreInProgress bool + RestoreSnapshotID. Startup recovery surfaces error; mid-Reset crash loses original. | |
| Combine: marker + retry, no safety snapshot | Cheapest. "Original data intact" interpreted as "no garbage served" not "recoverable original". | |

**User's choice:** Pre-restore safety snapshot of current state
**Notes:** Strongest REST-02 satisfaction — original is literally recoverable. Reuses Phase 21/22/23 surfaces.

### Safety snapshot semantics

| Option | Description | Selected |
|--------|-------------|----------|
| Hidden internal, --no-sync-gate, auto-delete on success | Add Snapshot.Kind enum (user|safety); excluded from list; auto-delete on success. | |
| Visible regular snapshot, sync-gate enabled, manual cleanup | Public CreateSnapshot with auto-name `pre-restore-<timestamp>`. Drains + verifies. Operator deletes after confirming. | ✓ |
| Hidden internal, sync-gate enabled, auto-delete on success | Hybrid: Kind=safety excluded from list, but DO run sync gate. Auto-delete on success. | |

**User's choice:** Visible regular snapshot, sync-gate enabled, manual cleanup
**Notes:** Operator-friendly bulletproof option — safety snapshot fully durable on remote, survives later remote-only failures. Accepts snapshot-list clutter cost.

---

## Verification gate & --no-sync-gate handling

### Pre vs post verify

| Option | Description | Selected |
|--------|-------------|----------|
| Both — pre-verify (fail-fast) + post-verify (REST-03 gate) | Pre-verify aborts before destruction; post-verify gates Enable readiness per REST-03. | ✓ |
| Post-verify only | Skip pre-verify (snapshot was sync-gated at create per Phase 23). Post-verify only after destructive Reset. | |
| Pre-verify only | Run before swap; skip post-verify. Violates REST-03 literal wording. | |

**User's choice:** Both
**Notes:** Pre-verify is cheap on durable snapshots (manifest is sorted hex hashes, Head per hash with bounded concurrency); catches 99% case without destructive cost. Post-verify is the REST-03 contract + catches mid-restore deletions.

### RemoteDurable=false source handling

| Option | Description | Selected |
|--------|-------------|----------|
| Refuse outright (ErrSnapshotNotDurable) | RestoreSnapshot returns error if RemoteDurable=false. Cleanest invariant. | |
| Allow with --force / RestoreOpts.AllowNonDurable | Default refuses; opt-in flag bypasses. Pre-verify still safe-gate. | ✓ (Claude's product rec) |
| Allow silently — verification catches it | Pre+post-verify will fail with missing-hash if blocks really not on remote. | |

**User's choice:** "What is the best choice here product wise?" (delegated to Claude)
**Notes:** Selected --force opt-in. Reasoning: default-refuse keeps obvious-correct-path UX; pre-verify is the real safety gate so --force is still safe-ish (fails fast if blocks actually missing); refusing outright over-restricts legit dev/test scenarios; RemoteDurable=false flag stays meaningful as default-refuse trigger without becoming hard wall.

---

## Code structure / PR shape

| Option | Description | Selected |
|--------|-------------|----------|
| 4 plans / 2 waves, single PR (mirror Phase 22/23 cadence) | Wave 1: Resetable+backends+conformance, sentinels+RestoreOpts. Wave 2: orchestration, E2E test. | ✓ |
| 2 plans / 1 wave, single PR (smaller scope) | P24-01 surface PR (interface+sentinels+conformance); P24-02 orchestration+test. | |
| Planner discretion | Defer to gsd-planner; let it pick wave breakdown. | |

**User's choice:** 4 plans / 2 waves, single PR
**Notes:** Continuity with Phase 22/23 cadence for review-velocity familiarity.

---

## Claude's Discretion

- D-24-03 Postgres TRUNCATE table list — planner audits Phase 20/21/22 migrations
- D-24-04 safety-snapshot name format (`pre-restore-<RFC3339>` suggested) — planner picks collision-avoidance vs readability
- D-24-09 step ordering sub-splits — planner may extract reset-then-restore helper
- D-24-14 HashSetFromMetadataStore placement — `pkg/snapshot/` if no Phase 21 helper to reuse
- `RestoreSnapshotOpts.SkipPreVerify bool` — planner may omit; non-breaking add later
- Concurrent-restore exclusion — share-disabled is the barrier; planner may add invariant comment but no runtime lock
- Metadata store reset interface design (option A/B/C tradeoff) — user delegated
- RemoteDurable=false handling default behavior — user delegated

## Deferred Ideas

- `dfsctl share snapshot restore` CLI command — Phase 25
- REST endpoint `POST /api/v1/shares/{name}/snapshots/{id}/restore` — Phase 25
- `pkg/apiclient/snapshots.go` Restore method — Phase 25
- Auto-disable / auto-enable around Restore — rejected per D-24-01; could revisit in v0.17
- Async Restore orchestration — rejected per D-24-02; could revisit if HTTP-timeout pain emerges in Phase 25
- Auto-cleanup of safety snapshot — operator-driven per D-24-04; non-breaking opt-in addable later
- Cross-share restore (snapshot A → share B)
- Snapshot encryption / per-snapshot keys
- Restore-progress metrics — `slog` only this phase
- Concurrent-restore exclusion lock — share-disabled is the barrier; add atomic if real-world double-restore emerges
