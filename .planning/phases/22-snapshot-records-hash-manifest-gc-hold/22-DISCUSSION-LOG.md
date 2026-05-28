# Phase 22: Snapshot Records + Hash Manifest + GC Hold - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-28
**Phase:** 22-Snapshot Records + Hash Manifest + GC Hold
**Areas discussed:** GC hold injection strategy, Snapshot state machine, Manifest on-disk format, Control plane store scope, Code structure and design

---

## GC hold injection strategy

### Q1: How should snapshot hashes enter the GC live set?

| Option | Description | Selected |
|--------|-------------|----------|
| Extend markPhase | New HoldProvider interface, markPhase calls it after FileBlock enumeration | |
| Options field + callback | HoldProvider on engine.Options struct, threaded through CollectGarbage | |
| You decide | Claude picks best approach | ✓ |

**User's choice:** You decide
**Notes:** Claude chose the Options-field approach (D-01) — wires through existing Options struct, matches Reconciler pattern, single live set.

### Q2: Disk manifest vs DB column as ground truth for held hashes?

| Option | Description | Selected |
|--------|-------------|----------|
| Read manifests from disk | HoldProvider streams manifest.hashes files | |
| Store hash list in DB | Duplicate hashes in DB for query efficiency | |
| You decide | Claude picks | ✓ |

**User's choice:** You decide
**Notes:** Claude chose disk-file ground truth (D-04) — minimal surface, avoids dual-write divergence.

### Q3: Per-remote scoped hold or global?

| Option | Description | Selected |
|--------|-------------|----------|
| Per-remote scoped | Hold filtered to remote being GC'd; matches RunBlockGC per-remote loop | ✓ |
| Global (simpler) | All active snapshot hashes regardless of remote | |
| You decide | Claude picks | |

**User's choice:** Per-remote scoped
**Notes:** Matches existing RunBlockGC architecture (D-03).

---

## Snapshot state machine

### Q1: 'deleting' state or immediate hard-delete?

| Option | Description | Selected |
|--------|-------------|----------|
| Hard delete (immediate) | rm row + manifest atomically, no deleting state | |
| Soft delete with 'deleting' state | Transition state first, then cleanup | |
| You decide | Claude picks | ✓ |

**User's choice:** You decide
**Notes:** Claude chose hard-delete (D-07) — simpler; failed cleanup leaves harmless orphans; hold released first (fail-safe direction).

### Q2: Can a 'failed' snapshot be retried?

| Option | Description | Selected |
|--------|-------------|----------|
| No retry — create new | Terminal failed state | |
| Retry allowed | failed → creating transition; needs idempotent orchestration | ✓ |
| You decide | Claude picks | |

**User's choice:** Retry allowed
**Notes:** Phase 23 orchestration must implement idempotent create (D-06).

### Q3: Should GC hold protect 'creating' snapshots too?

| Option | Description | Selected |
|--------|-------------|----------|
| Only 'ready' holds | No protection during create; grace period covers window | ✓ |
| 'creating' + 'ready' hold | Partial manifest also protected | |
| You decide | Claude picks | |

**User's choice:** Only 'ready' holds
**Notes:** If a block is collected mid-create, snapshot fails → retry. Avoids partial-manifest read races (D-05).

### Q4: Track MetadataEngine on the model?

| Option | Description | Selected |
|--------|-------------|----------|
| Yes — MetadataEngine field | Engine tag stored, restore validates match | ✓ |
| No — infer from share | Look up share config at restore | |

**User's choice:** Yes — MetadataEngine field
**Notes:** Protects against share-config changes between snapshot and restore (D-10).

### Q5: ID format — UUID or ULID?

| Option | Description | Selected |
|--------|-------------|----------|
| UUID (consistent) | Matches every existing model | ✓ |
| ULID (sortable) | Lexically sortable by creation time, new dependency | |

**User's choice:** UUID (consistent)
**Notes:** Overrides SNAP-01's ULID wording for project-wide consistency (D-09). Sort by CreatedAt when needed.

---

## Manifest on-disk format

### Q1: Plain text hex lines or binary format?

| Option | Description | Selected |
|--------|-------------|----------|
| Plain text hex lines | Human-readable, ~6.5 MB for 100k hashes | ✓ |
| Binary (length-prefixed) | ~3.2 MB, not greppable | |

**User's choice:** Plain text hex lines
**Notes:** Matches SNAP-02 spec verbatim (D-16).

### Q2: Streaming vs batch manifest writing?

| Option | Description | Selected |
|--------|-------------|----------|
| Batch from HashSet.Sorted() | Sort in-memory, then write | |
| Streaming during backup | Write as hashes arrive | ✓ |

**User's choice:** Streaming during backup
**Notes:** Required clarification — Phase 20 D-03 keeps HashSet in-memory. User confirmed the intent was "stream lines from Sorted()" (Q2-clarify below), not bypass HashSet.

### Q2-clarify: What kind of streaming?

| Option | Description | Selected |
|--------|-------------|----------|
| Stream lines from Sorted() | bufio.Writer line-by-line write; HashSet kept | ✓ |
| Bypass HashSet entirely | Break D-03; on-disk sort needed | |

**User's choice:** Stream lines from Sorted()
**Notes:** D-03 preserved. Manifest writer iterates Sorted() and writes via bufio.NewWriter (D-17).

### Q3: ReadManifest return type — HashSet or callback?

| Option | Description | Selected |
|--------|-------------|----------|
| Return HashSet | Simple, ~3.2 MB per snapshot for 100k | ✓ |
| Stream via callback | Lower peak RAM, more complex | |
| Both — callback + wrapper | Best of both | |

**User's choice:** Return HashSet
**Notes:** GC hold provider and Phase 23 sync gate both consume HashSet (D-18).

---

## Control plane store scope

### Q1: New SnapshotStore interface or extend ShareStore?

| Option | Description | Selected |
|--------|-------------|----------|
| New SnapshotStore interface | Dedicated sub-interface | ✓ |
| Extend ShareStore | Keep share-related ops together | |
| You decide | Claude picks | |

**User's choice:** New SnapshotStore interface
**Notes:** Matches existing sub-interface composition pattern (D-13).

### Q2: What happens to snapshots when share is deleted?

| Option | Description | Selected |
|--------|-------------|----------|
| Cascade delete (FK constraint) | Auto-delete rows; need filesystem hook | ✓ |
| Block share deletion if snapshots exist | User must delete snapshots first | |
| You decide | Claude picks | |

**User's choice:** Cascade delete (FK constraint)
**Notes:** Runtime.RemoveShare hook must rm snapshot dirs before cascade fires (D-15).

### Q3: ListSnapshots filter behavior?

| Option | Description | Selected |
|--------|-------------|----------|
| Always return all | Caller filters by state | ✓ |
| Optional state filter | SQL-level filtering | |
| You decide | Claude picks | |

**User's choice:** Always return all
**Notes:** Simpler API; HoldProvider filters itself (D-14).

### Q4: Path helpers — on model, caller, or pkg/snapshot?

| Option | Description | Selected |
|--------|-------------|----------|
| Model has helpers | Methods on *Snapshot for paths | ✓ |
| Caller computes path | Each call site builds path | |
| Helpers live in pkg/snapshot | Keep model pure data | |

**User's choice:** Model has helpers
**Notes:** Single source of truth for SNAP-02 layout (D-12).

### Q5: Uniqueness constraint on active snapshots?

| Option | Description | Selected |
|--------|-------------|----------|
| Unique (share, state='creating') | Only one in-flight per share | ✓ |
| No constraint | Phase 23 handles concurrency | |
| You decide | Claude picks | |

**User's choice:** Unique (share, state='creating')
**Notes:** Multiple ready snapshots OK; partial unique index (D-08).

---

## Code structure and design

### Q1: HoldProvider interface location?

| Option | Description | Selected |
|--------|-------------|----------|
| engine package | Alongside MetadataReconciler in gc.go | ✓ |
| pkg/snapshot package | Engine imports snapshot | |

**User's choice:** engine package
**Notes:** Matches existing reconciler pattern (D-01).

### Q2: PR shape?

| Option | Description | Selected |
|--------|-------------|----------|
| Single PR, staged commits | 6 commits each building independently | ✓ |
| Two PRs | Model+store first, then GC integration | |

**User's choice:** Single PR, staged commits
**Notes:** Matches Phase 20 D-21 and Phase 21 D-09 patterns (D-20).

### Q3: Integration test driver coverage?

| Option | Description | Selected |
|--------|-------------|----------|
| Memory store only | Phase 21 conformance covers driver coverage | ✓ |
| All three engines | More confidence; slower CI | |
| You decide | Claude picks | |

**User's choice:** Memory store only
**Notes:** Focus is GC hold semantics, not backup-driver coverage (D-21).

---

## Claude's Discretion

- D-01 / D-02 — GC hold injection mechanism (Options field + markPhase wiring); user chose "you decide"
- D-04 — Manifest file as ground truth vs DB column; user chose "you decide"
- D-07 — Hard-delete semantics; user chose "you decide"
- Manifest scanner buffer sizing
- Whether `ManifestCount` validation method belongs to Phase 22 or Phase 23

## Deferred Ideas

- Hash-list DB index for huge snapshot counts
- Async GC hold release with brief delete-lock (Phase 23 concern)
- `deleting` soft-state for crash-safe cleanup
- Project-wide ULID adoption
