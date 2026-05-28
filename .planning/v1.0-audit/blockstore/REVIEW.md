# Area 1 — Blockstore + CAS + Engine — PR-A Audit (REVIEW.md)

**Status**: SCAFFOLD — awaiting agent fill-in.
**Branch**: `v1.0/area1-blockstore-audit` @ `origin/develop@22f0afd0`.
**Scope**: `pkg/blockstore/` (171 files, ~19.3k src + ~20.7k test). GC bundled (`engine/gc*.go`). Conformance suite (`blockstoretest/`) audited as subject, not authority.
**Excludes**: Syncer (area #2 — files referenced but not deep-audited). Backup/Snapshot lifecycle (area #9 — HoldProvider integration touched here).

---

## 1. Current State (filled by `code-explorer`)

_Architecture map: WRITE path, READ path, GC mark-sweep, per-share lifecycle, restart-survival surfaces._

## 2. Structural Findings (filled by `code-architect`)

_Module boundaries, interface leaks, layering violations, public-API surface, naming pass._

### 2a. Collapse Candidates
_One-impl interfaces, single-consumer Options, type aliases, util grab-bags, shims._

### 2b. Naming Pass
_Variables / types / functions / files / packages._

## 3. Bug Findings (filled by `code-reviewer`)

_Bugs, security, quality, convention adherence. Includes root-cause confirmation for pre-existing trackers #668 (rollup wedge), #669 (dedup refcount), #670 (NFS COMMIT — engine-side contribution only)._

## 4. Tests Findings — Conformance Suite Audit-as-Subject

`pkg/blockstore/blockstoretest/` audited **assuming rewrite-from-scratch**. Anchors: external CAS literature, S3 API spec, BLAKE3 spec, POSIX file semantics where they bleed in. NOT anchored on current impl.

Outcomes to surface:
- Assertions that codify implementation choice instead of spec behavior.
- Round-trip tests that pass on any in-memory map.
- Missing edge cases — partial writes, torn reads, concurrent dedup donor + GC, restart-mid-flush, hash collision, range read past EOF, zero-byte put, multi-GB put.
- Backend coverage asymmetry — local-fs-shaped assertions that accidentally pass on s3-memory.
- **Recommendation table**: per assertion → KEEP / REWRITE / DELETE / MISSING.

## 5. Bottlenecks (filled by perf pass)

Top-5 hotspots per workload as `file:func cum% flat% notes`.

### Workloads (seeded — replayable via `--seed N`)
- **(a)** small-files concurrent writes — N workers × M files × 4-64 KiB
- **(b)** small-files concurrent reads — same fileset, randomized reader assignment
- **(c)** big-files concurrent writes — N workers × M files × 64-256 MiB
- **(d)** big-files concurrent reads — random offsets, mixed seq + rand
- **(e)** mixed-ops storm — thousands of seeded random WRITE/READ/LIST/DELETE; replay-from-seed for regression triage

Captured artifacts under `_profiles/`:
- `cpu.{a,b,c,d,e}.pprof`
- `heap.{a,b,c,d,e}.pprof`
- `block.{a,b,c,d,e}.pprof` (mutex + block — once #671 wires runtime exposure; until then mark MISSING)
- `goroutine.{a,b,c,d,e}.pprof`
- `seed.{a,b,c,d,e}.txt` — exact seed + workload params for replay

### Macro reuse
`.planning/v1.0-audit/_baseline/` Wave 0 pprofs diffed where applicable. No fresh macro on bench infra unless a finding requires it.

## 6. Pre-existing Tracker Mapping

| Issue | Title | Audit treatment |
|---|---|---|
| #668 | Rollup wedges on tree/logIndex divergence + ObjectIDPersister conflict | Root-cause confirm in §3, fix slot in PR-B |
| #669 | File-level dedup refcount on missing FileBlock | Root-cause confirm in §3, fix slot in PR-B |
| #670 | NFS COMMIT D-state hang | Engine-side stall surface only; full fix lives in NFS area #4 |

## 7. HIGH / MED / LOW Triage

| ID | Severity | Confidence | File | Summary | PR-B slot |
|---|---|---|---|---|---|

## 8. PR-B Sequencing Proposal

_Ordered fix list: independent fixes first → coupled fixes → simplifier collapse sweep → reviewer re-review → verify → ultrareview → merge._

## 9. Decisions Carried Forward

- GC bundled into area #1; drop separate area #8 from PLAN.md ✅
- Conformance suite — audit treats rewrite-from-scratch as viable outcome.
- `pkg/` ↔ `internal/` split intent (a/b/c) — feeds runtime area #7, but this audit logs any `pkg/blockstore` → `internal/` import as a data point.

---

_Filled by parallel `feature-dev:{code-explorer,code-architect,code-reviewer}` agents + serial bottleneck pass._
