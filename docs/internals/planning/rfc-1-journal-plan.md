# RFC 1 — journal v2: implementation plan

**Scope:** [RFC 1 — the journal](../rfcs/rfc-1-journal.md), together with the parts of
[RFC 28 — journal format](../rfcs/rfc-28-journal-format.md) it cannot work without, and the
release-1 parts of [RFC 15 — topology](../rfcs/rfc-15-topology.md). The test foundations
follow [RFC 29 — test harness](../rfcs/rfc-29-test-harness.md).
**Team:** two engineers, called **A** and **B** below.
**Naming:** **v1** is the journal that exists today (`pkg/block/journal`, one journal per
share). **v2** is the journal RFC 1 describes (one journal per disk, shared by every share
placed on that disk).

---

## 1. Introduction

### 1.1 What we are building

v2 is a new local journal. It is not a refactor of v1 and shares no code with it. The two
differ at the root:

| | v1 (`pkg/block/journal`) | v2 (this plan) |
| --- | --- | --- |
| Granularity | one journal per **share** (`<journal.path>/shares/<name>/journal`) | one journal per **device**, shared by every share on it, each share working through its own `Handle` and `ShareTag` |
| What it knows | carving, chunk sizes, eviction, cold seeding (`Hydrate`, `Commit`, `SeedCold`, …) | extents of files only. No chunks, no blocks, no remote tier ([RFC 1 §1.1](../rfcs/rfc-1-journal.md)) |
| Read of absent data | depends on the path | always an exact `missing` list, caller's buffer untouched |
| Eviction | journal-driven | engine-driven: `Offload` → `report` → `Release` → `Repack` |
| On-disk format | v1 segments | RFC 28: `format` file, 48-byte segment header, 96-byte record header, 14 record kinds, catalog and trailer, CRC32C |

The v2 package's dependency set is the Go standard library plus the OS file interface
(RFC 1 §1.1), which admits `golang.org/x/sys` in `internal/platform` only (epic 1, D6). An
import test enforces this, and it also forbids importing v1.

### 1.2 How the plan is organised

There are 12 epics:

- **Epics 1–2** fix the contract: the interfaces, and a red end-to-end suite with every
  test on a known-failure list, plus the small test components everything else needs.
- **Epics 3–9** implement the RFC. They are cut along the RFC's own seams (format, index,
  segments, durability, recovery, data path, reclamation) so that each can be built and
  unit-tested on its own.
- **Epics 10–12** connect v2 to the rest of DittoFS, turn the e2e suite green, and add
  dashboards.

Each epic is sized at about 10 subtasks. The detailed subtask breakdown of an epic is
written when that epic starts, under the rules of §1.5.

### 1.3 What epic 1 must deliver

Epic 1 is **interfaces only**. Its output is code that compiles and does nothing:

- **The directory tree** of the v2 package and its internal sub-packages (§4, epic 1).
- **Every exported symbol from RFC 1 §3–§4**, with its exact signature. That means
  `Init`, `Open`, `Restamp`, `Journal`, `Handle`, `Reservation`, and every method in
  §3.1–§3.11, §4.3 (`Draw`, `SetVersionLimit`), §4.4 (inspection) and §7.2 (`Repack`).
  It also means every value type (`FileID`, `Extent`, `Held`, `Version`, `ShareTag`,
  `SyncPolicy`, `Offer`, `Change`, `CloneSpec`, `RepackScope`, `Stats`, …) and the error
  set (`ErrNoSpace` carrying which limit refused, the stale-fill error, closed, forgotten,
  …).
- **The internal seams between epics 3–9**, as Go interfaces with signatures: the storage
  seam (RFC 1 §1.3), the record and segment codecs, the placement index, the segment
  store and append stream, the sync coordinator, recovery, and the reclaimer. These are
  what make the epics independent. Each epic implements one side of a seam and tests
  against the other side's interface.
- **How dependencies are injected**: clock, storage seam, observability hooks, limits and
  settings. RFC 1 fixes `Open(dir, floor, expect)`, so epic 1 picks a mechanism that keeps
  that signature (for example functional options or an `OpenWith` variant) and states it
  once.
- **The observability contract**: the `Stats` struct with every field of RFC 1 §3.7 and
  §3.9, and a `Hooks` type for spans and events built from standard-library types only
  (`context.Context`, `time.Duration`, function fields). The journal may not import a
  metrics or tracing library, so epic 10 adapts these hooks to Prometheus and OpenTelemetry.
- **Stub bodies**: every function returns `ErrNotImplemented` with zero values, or an empty
  iterator, and never panics (epic 1, D8). This is what lets epic 2's tests compile and
  fail on their assertions.

**Exit criteria:**

- `go build ./...` and `go vet ./...` pass.
- The import test passes, allowing stdlib only (plus `golang.org/x/sys` in
  `internal/platform`, epic 1 D6) and forbidding `pkg/block/journal`.
- Every symbol carries a doc comment stating its contract in terms of the code.
  `CLAUDE.md` keeps RFC section numbers out of source comments, so the map from each
  symbol to its RFC section lives in the epic's planning file instead.
- Both engineers have reviewed and signed off. Changing a frozen signature after that
  needs both to agree, and the change is noted in this file.

### 1.4 What epic 2 must deliver

Epic 2 makes "done" measurable before any implementation exists, following
[RFC 29](../rfcs/rfc-29-test-harness.md). It has three outputs.

**1. Shared test components that block everything else:**

- **Clock and fault implementations.** `pkg/sim` holds the set's one clock and one fault
  interface ([RFC index, test harness item 1](../rfcs/rfc-index.md)). Epic 1 declares
  them (subtask 1.4); epic 2 implements the virtual clock and the seeded fault schedule.
  The journal declares its own `Clock` with standard-library types only, because its
  import rule bars importing the shared package. RFC 8's `Clock` uses a named `Mono` type,
  so the two are not structurally identical, and an adapter in `journalv2test` bridges
  them (epic 1, D3).
- **Crash-simulating storage seam.** It wraps a **real** filesystem and records every
  write, allocation and truncate since each file's last sync, and every create, rename and
  unlink since the last directory sync. A simulated crash discards exactly those
  operations. The seam can also fail, delay, tear or reorder any single call, including
  sync, directory sync and the uncached (direct I/O) read. Killing the process is never
  used as a stand-in.
- **Reference model.** A pure in-memory model of the journal: each file's bytes, each
  extent's offloaded bit, removal markers, and what `Since` yields. The model-based test
  and the e2e assertions compare against it after every step. A failing seed is kept and
  replayed, never retried into green.
- **Known-failure mechanism for Go tests.** Today one exists only for shell suites
  (`test/common/known-failures.sh`). Epic 2 adds a Go helper that reads the same Markdown
  table format, so one list can serve both the Go and shell runners. A listed test that
  fails is reported as expected. **A listed test that passes fails the run**
  ([RFC index, harness requirement 6](../rfcs/rfc-index.md)), so a test that turns green
  forces its entry out of the list.
- **Profiling harness.** A helper wrapped around each test and benchmark that snapshots
  goroutines, open file descriptors and heap before and after. It fails on leaked
  goroutines or descriptors and on heap growth past a threshold, and writes pprof profiles
  as artefacts when it fails. It also provides an allocations-per-operation assertion for
  the RFC 1 §9.7 counted checks. The allocation and heap assertions are skipped under
  `-race`, which changes both, and run in a separate non-race job. The heap threshold is
  calibrated per test with slack, not one global number, so that it does not flake.

**2. The red end-to-end suite, at two levels:**

- **Component e2e (conformance).** These drive the public v2 interface on a real
  filesystem in a temporary directory, through the storage seam, on virtual time. They
  cover every named check of RFC 1 §9.4 (Groups A–D), RFC 1 §9.2's edge cases, and
  RFC 28 §4.1–§4.2's format and recovery checks.
- **System e2e, scaffolding only.** The system tests run a real `dfs` process on v2, with
  kernel NFS and SMB clients, under `test/e2e`. They cover what only the composed system
  shows: one journal per disk serving several shares, restart, device loss, a full disk,
  and the v1→v2 upgrade. No `dfs` can run v2 before epic 10 wires it, and there is no
  configuration key to select it, so a system test written now could only fail at start,
  never on its own assertion. Epic 2 therefore builds the runner, the fixtures and the
  known-failure wiring, and keeps the system scenarios as a catalogue. **Epic 10 writes
  the system tests**, listed as known failures, and epic 11 turns them green.

The scenario catalogue the suite must cover is in §5, epic 2.

**3. A traceability matrix.** It maps every RFC 1 §9.4 row and every RFC 28 §4.2 row to
the test that implements it. A script fails CI when a row has no test, or when a test was
expected to run and did not. A check that did not run is a failure
([RFC index, test tiers](../rfcs/rfc-index.md)).

**Exit criteria:**

- The whole suite compiles against epic 1's stubs and runs in CI.
- Every test fails on its own assertion, not on a build error or a skip.
- Every test is on the known-failure list, so the run is green.
- The matrix has no unmapped rows.
- The seam, clock, model and profiler each have their own unit tests. These are green from
  day one, because they do not depend on v2.

### 1.5 General rules for each epic

> **Instruction for whoever expands an epic into its detailed description and subtasks,
> human or AI:** link this section from that description; do not copy it, so the rules
> keep one source and the copies cannot drift. Every subtask must state how it meets each
> rule that applies to it. A rule that does not apply is named
> with the reason.

1. **Independent unit tests.** Each epic carries its own unit tests and does not rely on
   another epic's implementation to be tested. It may rely on another epic's interfaces
   from epic 1, and on the test components from epic 2.
2. **Fuzz tests where relevant.** Use Go native fuzzing (`testing.F`) on every parser of
   bytes the journal did not just write: records, segment headers, seal markers, catalogs,
   trailers, the `format` file. Also fuzz-drive operation sequences against the reference
   model. The property is that any input is either accepted as valid or rejected: never a
   panic, and never an extent the bytes do not contain.
3. **Metrics, and tracing where relevant.** Every counter and gauge RFC 1 §3.7 and §3.9
   assigns to the epic's area is maintained, and exposed through `Stats` (served from
   maintained counters, never a walk). Spans and events go through epic 1's
   standard-library `Hooks`, never a metrics or tracing library. Operations worth a span:
   open and recovery, sync, offload pass, repack pass, fill, and a refused reservation.
4. **Profiling.** Tests and benchmarks run under epic 2's profiling harness and fail on
   goroutine, descriptor or memory leaks and on anomalous resource growth. Hot paths
   (`WriteAt`, `ReadAt`, `Stats`, `report`, index lookup and insert) carry
   allocations-per-op assertions and the counted checks of RFC 1 §9.7. Allocation and
   heap assertions run outside `-race` only (§1.4).
5. **Public interface and internals.** Tests cover the epic's internal package API and the
   public v2 interface behaviour it enables, including concurrency under `-race`. Per the
   project rules, test at the layer that **consumes** a value, not the one that produces
   it. A regression test is validated by reverting the fix and watching it fail **on its
   assertion**.
6. **Known failures shrink.** Every e2e test an epic turns green is removed from the
   known-failure list in the same change. The runner fails if a listed test passes.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on
   `pkg/block/journal`. The import test enforces it. v1 is touched only in epic 10, and
   only at its callers. The one exception is the `v1-clean` marker, which ships in v1
   ahead of v2 ([#3156](https://github.com/marmos91/dittofs/issues/3156)); v2 reads only
   that file and the presence of v1 directories, never v1's format.
8. **Real storage for storage properties.** No in-memory filesystem stands in for a check
   about durability, torn tails, unlink durability or corruption (RFC 1 §9.5). In-memory
   stand-ins are allowed only for pure logic (index, codecs).
9. **Code comment conventions** (`CLAUDE.md`). No epic, issue or plan IDs in source
   comments. Use `ponytail:` for knowingly-simple implementations and `decision:` for
   deliberate behaviour. Where the RFC already carries a ponytail or decision note, its
   marker goes at the code site that implements it.

---

## 2. Epics at a glance

| # | Epic | Content | Est. subtasks | Owner | Blocks |
| --- | --- | --- | --- | --- | --- |
| 1 | **Interfaces** ⛔ | Directory tree, exported symbols, signatures, internal seams, `Stats`, `Hooks`, errors, stubs | 10 | A + B | **everything** |
| 2 | **E2E suite + test foundations** ⛔ | Clock and fault implementations, crash-simulating seam, reference model, Go known-failure list, profiler, red conformance suite, system e2e scaffolding and catalogue, traceability matrix | 14 | A + B | **all testing** in 3–12 |
| 3 | **On-disk format and platform layer** ⛔ | RFC 28 codecs, golden vectors, CRC32C, `format` file replace, per-OS sync, allocation, direct I/O and CoW checks | 10 | A | 5, 7 |
| 4 | **Placement index and versions** ⛔ | Per-file B-tree, held, retained and marker sets, precedence, sequence and version counters, version limit, floor buckets, extent bound, drop ranges, per-segment bytes | 11 | B | 7, 8, 9 |
| 5 | **Segments, streams and accounting** ⛔ | Segment lifecycle, spare pool, append streams, seal, catalog and free tail, recycle and unlink, descriptor cache, storage guard, footprint accounting | 11 | A | 6, 7, 8 |
| 6 | **Durability and loss** | `Sync`, group commit, `bounded` and `standard` policies and timer, failed-sync windows (re-append or fail), loss records, events and generation | 10 | A | 8 (Sync), 10 |
| 7 | **Recovery and identity** | `Init`, `Open`, `Restamp`, rebuild by catalog and by scan, torn vs corrupt, unproven-tail re-verify, offload-state rebuild, stale rule, spare re-adoption, inspection and verify | 12 | A | 10 |
| 8 | **Data path and capacity** ⛔ | `Journal`, `Share`, `Handle`, `Close`, `WriteAt`, `ReadAt`, `Fill`, removals, `Since` and `Settle`, `Draw`, `Reserve`, limits, headroom, named refusals, lock domains, `Stats` | 13 | B | 9, 10 |
| 9 | **Offload, reclamation, holds and tags** | `Offload`, `OffloadMany`, pins, `MarkOffloaded`, `Unmark`, `Release`, auto-retire, `Repack` and drop test, snapshot holds, `Forget`, `Strand` | 13 | B | 10 |
| 10 | **Integration and v1→v2 upgrade** 🔀 | One journal per disk in Runtime, engine bridge, startup order, upgrade preflight and adoption, config, metrics and span exporters, error mapping, salvage, API and CLI | 14 | B (wiring) + A (config, upgrade, exporters) | 11, 12 |
| 11 | **E2E green** 🔀 | Burn down the known-failure list, soak, benchmarks J1–J5 and J8, CI tier registration, profiling baselines | 8 | A + B | release |
| 12 | **Dashboards** | 4 Grafana dashboards, each with an automated test | 6 | A | — |
| | | | **~132** | | |

⛔ = blocker: other epics cannot finish until this one has delivered.
🔀 = synchronisation point: both engineers converge, and the work that follows depends on
everything before it.

---

## 3. Synchronisation points, blockers and the two-person split

### 3.1 Dependency graph

```text
                 ┌──────────── E1 Interfaces ────────────┐   S1: interfaces frozen
                 ▼                                       ▼
           E2 test foundations + red e2e suite  ──────────────   S2: red suite in CI
        ┌────────┴──────────┐
        ▼                   ▼
   A: E3 format        B: E4 index
        │                   │
        ▼                   │
   A: E5 segments ──────────┤        S3: walking skeleton (E5 minimal stream + E8 WriteAt/ReadAt)
        │   ╲               ▼
        │    ╲─────► B: E8 data path & capacity
        ▼                   │
   A: E6 durability ───────►│ (Sync)
        │                   ▼
   A: E7 recovery      B: E9 offload / reclaim / holds
        └─────────┬─────────┘
                  ▼
            E10 integration + upgrade    S4: core complete → S5: wired
                  │
           ┌──────┴──────┐
           ▼             ▼
        E11 e2e green  E12 dashboards
```

### 3.2 Synchronisation points

- **S1. Interfaces frozen** (end of E1). Both engineers review every signature and seam.
  Nothing in E3–E9 starts before this point.
- **S2. Red suite in CI** (end of E2). The conformance suite compiles, runs, fails, and is
  all listed as known failures. The system e2e scaffolding is in place. The seam, clock, model and profiler are
  green. Implementation epics start here.
- **S3. Walking skeleton** (mid E5 / mid E8). A minimal append stream from E5 (one stream,
  no spare pool, no seal) plus E8's `WriteAt` and `ReadAt` on top of E4's index. The first
  conformance tests turn green and leave the known-failure list. **E8's integration tests
  are blocked on this point.** E5 therefore delivers the minimal stream as its first
  subtasks.
- **S4. Core complete** (E3–E9 done). All component conformance tests are green. Only
  system e2e tests remain on the known-failure list. E10 cannot finish before this point.
- **S5. Wired** (end of E10). v2 replaces v1 in the Runtime, and the system e2e suite,
  written in E10, runs against it. **S5 has an external dependency:** the RFC 16 metadata
  store of [#3120](https://github.com/marmos91/dittofs/pull/3120) must be on
  `feat/storage-v2`, because E10's engine bridge and upgrade both rely on it. E11 and E12 then run in parallel.

### 3.3 Blockers

- **E1 and E2** block everything. Keep them short and do them together.
- **E3** blocks E5 (segments are made of E3's records) and E7 (recovery parses E3's
  bytes).
- **E4** blocks E7 (rebuild fills the index), E8 and E9. It is pure in-memory code with no
  storage dependency, so it can start at S2.
- **E5** blocks E6, E7 and E8's integration tests.
- **E8** blocks E9: offload needs written content, and `report`, `Release` and `Repack`
  need the data path's locking and capacity.
- **E10** blocks E11 and E12's live tests.
- **#3120 (external)**, the RFC 16 metadata store, blocks E10's version-limit raise, its
  store contracts and the upgrade's adoption record.

### 3.4 Splitting the load between two people

The split follows the RFC's natural divide: **A owns the bytes on disk, B owns the state
in memory and the operations on it.** Each track has a run of work with no cross-track
dependency.

| Phase | A — storage track | B — model and operations track | Joint |
| --- | --- | --- | --- |
| 0 | E1: storage-side seams (storage seam, codecs, segment store, sync, recovery) | E1: public API (`Journal`, `Handle`, ops, `Stats`, errors), index seam | review → **S1** |
| 1 | E2: clock and fault implementations, crash-simulating seam, profiler, system e2e scaffolding | E2: known-failure mechanism, reference model, conformance test bodies, traceability matrix | → **S2** |
| 2 | E3, then E5 (minimal stream first) | E4, then E8's storage-independent parts (share registry, capacity counters, version counter) | → **S3** |
| 3 | E6, then E7 | E8 remainder, then E9 | → **S4** |
| 4 | E10: config keys, per-device registry, upgrade preflight and adoption, metrics and span exporters | E10: Runtime and engine wiring, startup order, error mapping, salvage | → **S5** |
| 5 | E12 dashboards; E11 burn-down of recovery, format and durability failures | E11 burn-down of data-path and reclamation failures | release |

Load by subtask count: A ≈ 5 (E1) + 7 (E2) + 10 + 11 + 10 + 12 + 7 (E10) + 4 (E11) + 6
(E12) ≈ 72. B ≈ 5 + 7 + 11 + 13 + 13 + 7 (E10) + 4 (E11) ≈ 60. B's half of E10 (the engine
bridge) is the riskiest piece of integration, which roughly evens the two tracks.
If B finishes E9 early, B takes E7's inspection and verify subtasks (RFC 1 §4.4), which
read only the index and the segments.

---

## 4. Directory layout proposed for epic 1

E1 confirms or amends this layout. It is listed here so that every epic knows where its
code lives.

```text
pkg/block/journalv2/              package journalv2 — public API (RFC 1 §3)
  doc.go  journal.go  handle.go  reservation.go
  types.go  errors.go  stats.go  hooks.go  inspect.go
  clock.go  options.go            narrow stdlib-only Clock / injection points
  imports_test.go                 stdlib only (x/sys in internal/platform), no pkg/block/journal
  internal/format/                RFC 28 §2 codecs, CRC32C, golden vectors       (E3)
  internal/platform/              per-OS sync/alloc/direct I/O/CoW probes        (E3)
  internal/storage/               the storage seam interface (RFC 1 §1.3)        (E1/E2)
  internal/index/                 placement index, versions, drop ranges         (E4)
  internal/segment/               segments, streams, spares, descriptors, guard  (E5)
  internal/durability/            sync, group commit, failed windows, losses     (E6)
  internal/recovery/              open/rebuild/scan/verify                       (E7)
  internal/reclaim/               repack, retirement                             (E9)
  conformance/                    component e2e: RFC 1 §9.4, RFC 28 §4.2         (E2)
  journalv2test/                  crash seam, reference model, profiler, fixtures (E2)
pkg/sim/clock, pkg/sim/fault      shared clock & fault (RFC 8 signatures)   (E1 declares, E2 implements)
test/journalv2/KNOWN_FAILURES.md  one list, Markdown table read by Go and shell  (E2)
test/e2e/journalv2_*_test.go      system e2e                       (E2 scaffolding, E10 tests)
monitoring/grafana/journal/       dashboards                                     (E12)
```

The package is named `journalv2` while v1 exists. Renaming it once v1 is removed is out of
scope. The suffix stays in Go package and import paths only: metric and span names, log
fields and dashboards say `journal`, with the version exposed as a label where it matters
(epic 1, D1).

---

## 5. The epics in detail

Each paragraph below gives the epic's content, its RFC sources, and candidate subtasks. A
detailed breakdown must also link §1.5.

### Epic 1 — Interfaces ⛔ (≈10 subtasks, A + B)

Epic 1 creates the tree of §4 and declares, with doc comments and stub bodies, every
symbol and signature of RFC 1 §3–§4 and §7.2. It also declares the internal seams that let
epics 3–9 proceed in parallel. **No behaviour is implemented.**

Candidate subtasks:

1. Package skeleton and the import test.
2. Value types and the version type: 128-bit, epoch plus counter, epoch held at zero. Its
   byte form is big-endian, epoch then counter, byte for byte the metadata store's
   `entity.JournalVersion`, so that `bytes.Compare` orders as `Compare` does.
3. Error set, with `ErrNoSpace` naming the refusing limit (share, journal, index,
   headroom, version, spare) and the missing amount.
4. `Init`, `Open` and `Restamp`, and the injection mechanism for clock, seam, hooks and
   settings.
5. `Journal`, `Handle` and `Reservation` methods.
6. `Offer`, `Change`, `CloneSpec`, `RepackScope` and the snapshot-hold signatures.
7. `Stats`, its per-share breakdown, the counters, `LossEvent`, and `Hooks`.
8. Inspection and verify API (RFC 1 §4.4).
9. Internal seam interfaces: storage, codecs, index, segment store, sync coordinator,
   recovery, reclaimer.
10. Joint review and freeze.

### Epic 2 — E2E suite and test foundations ⛔ (≈14 subtasks, A + B)

Epic 2 delivers §1.4. The suite must cover **as many use cases as can be named**. The
scenario catalogue below is the minimum. Each family becomes one or more subtasks. Each
test is named after its RFC row where it has one, and every test is listed as a known
failure.

**Foundations:**

- clock and fault implementations;
- the crash-simulating seam;
- the reference model and the seeded model-based test (RFC 1 §9.1);
- the Go known-failure mechanism;
- the profiler;
- the traceability script.

**Scenario catalogue (component level):**

- **RFC 1 §9.4 Group A, silent data loss** (34 checks):
  - no zero-fill;
  - a write during offload;
  - fill safety and fill setting the offloaded bit;
  - release refusal;
  - partial release keeps the record whole;
  - repack copies only the live part, including across a crash;
  - an unlink or repack while a read is in flight;
  - removals survive to `Since`, and no resurrection;
  - removal records carried forward and dropped by version;
  - failed-sync windows;
  - losses identified by exact record, and a synced loss masking what it superseded;
  - re-append keeps its numbers;
  - offered bytes and repack sources are verified;
  - retained extents survive repack;
  - no pin on a segment under repack.
- **Group B, wedging and unbounded use** (29 checks):
  - reservations, reserve-ahead, handle close;
  - named refusals, share limits, repack at capacity;
  - descriptor bound, coalescing, index bound;
  - headroom is allocated, and the below-half rule;
  - pinned-segment bounds;
  - floor buckets;
  - `O(log n)` index bounds;
  - versions never repeat;
  - freed means freed;
  - reclamation is not starved, and removals do not wait on offload;
  - no deadlock;
  - background work is joined at close;
  - per-extent drop, share-aimed repack, segment accounting.
- **Group C, recovery** (17 checks):
  - reseed after repack;
  - release across a restart, and a lost release record;
  - fill beats an older write, and a stale fill;
  - incremental report;
  - epoch half must be zero;
  - monotonicity in shuffled segment order;
  - an attached share raises the counter;
  - unclaimed tags are the caller's to name;
  - restamp;
  - forget leaves nothing to settle;
  - stranded tag reported, then salvaged;
  - clone target survives a crash;
  - header-only records are never failed;
  - a loss survives a restart.
- **Group D, observability** (11 checks): wedge visible, accounting follows the cap,
  generation never repeats, `Stats` is cheap and keeps its inequalities, backlog age,
  clean bytes count as occupancy, every metric is reachable, loss events, per-segment held
  bytes, verification reports without repairing.
- **RFC 1 §9.2 edge cases**: zero-length and near-max-offset writes, three-way splits, a
  write straddling a segment, a record exactly filling a segment, truncate down and up,
  partially offloaded release, and the other listed cases.
- **RFC 28 §4.1–§4.2, format and recovery**:
  - golden byte vectors for every structure;
  - a bit flip in every region;
  - torn tail vs corruption, zero slots, scan resync;
  - catalog equals scan;
  - a foreign segment is not attached;
  - `format` at 55 or 57 bytes, or version 2;
  - unknown kinds 15 and 128;
  - an interrupted `Init`, `format.tmp` replace and spare creation;
  - an interrupted recycle;
  - unproven-tail re-verification with direct I/O;
  - idle seal;
  - mtime replay;
  - an older format opening on a newer binary, and the reverse refusal.
- **Platform** (RFC 28 Appendix A):
  - refuse a `nobarrier` mount;
  - refuse CoW without `+C`;
  - report a thin-provisioned device as a health condition;
  - `F_FULLFSYNC` on macOS, run as a CI matrix leg where available.
- **Model-based**: random sequences of write, fill, offload, release, truncate,
  deallocate, delete, settle, repack, crash and reopen, at least two files on two streams
  and two shares on one journal (RFC 1 §9.3).

**Scenario catalogue (system level, `test/e2e`, real `dfs` with v2; written as tests in
epic 10):**

- **Single node, several shares, one journal per device** (RFC 15 §2.2 and §2.4,
  RFC 8 §2.3):
  - N devices give N independent journals;
  - shares on one device keep their own limits, so one share at its limit leaves the
    other writing;
  - corruption or loss of one device affects only its shares;
  - start is refused with `storage` but no journal devices.
- **Durability through the protocols**:
  - NFS COMMIT and SMB flush survive a host crash (simulated at the seam or in a VM);
  - an unflushed write survives process death;
  - under `bounded`, unflushed data is lost by at most the bound on power loss;
  - a failed sync changes the NFS write verifier, so the client resends.
- **Cold reads**: evict, restart, read. Bytes come back from the remote tier and are never
  zeros. A read of a truly lost extent fails, never returning zeros (RFC 0 I1).
- **Reference workloads** ([RFC index](../rfcs/rfc-index.md)):
  - an FSLogix-style 30 GB container, with random overwrites behind one long-lived SMB
    handle;
  - a sign-in storm of concurrent cold opens;
  - sign-out compaction, a single large truncate;
  - read-mostly ingestion of about 7,000 small files.
- **Capacity**:
  - a journal filled past its maximum, giving retry-later with no data loss;
  - a full underlying filesystem, giving `ENOSPC` that is distinct from the journal's own
    refusal;
  - the descriptor bound under `ulimit -n`.
- **Lifecycle**:
  - share create, remove and re-create on the same device: no resurrection, and a new tag;
  - share removal forgets its tag;
  - restart with a removed share pending;
  - a disk moved to another installation, or a journal directory swapped: refused by
    `expect` and the installation ID.
- **Upgrade** (epic 10's preflight and adoption):
  - each row of the preflight table: adopted, fresh install, clean upgrade, refusal;
  - a refused start writes nothing: the DB, the devices and the v1 directories are
    byte-identical afterwards, and the previous v1 release starts on them;
  - a crash at every adoption step: before the adoption record, a re-run adopts again and
    the previous v1 release still starts; after it, the next start completes the v1
    `format` stamp;
  - after adoption, the previous v1 release refuses to start (`ErrFutureFormat`).
- **Other sources:**
  - fsync-failure semantics after the PostgreSQL "fsyncgate" findings: a failed fsync is
    not retried into success;
  - crash-consistency exploration in the style of ALICE and CrashMonkey: crash at every
    sync point and reorder unsynced writes;
  - `errors=remount-ro` on ext4;
  - XFS speculative preallocation (accounting follows the cap);
  - disk hot-removal mid-write;
  - a clock jump or a clock 5% fast (RFC 0 §1.4);
  - `SIGKILL` vs power loss, kept distinct;
  - a long soak with capacity below the data written.

### Epic 3 — On-disk format and platform layer ⛔ (≈10 subtasks, A)

Epic 3 implements RFC 28 §2 and Appendix A as pure, separately testable layers.

- **Codecs.** Encode and decode for:
  - the `format` file (56 bytes, `DJF1`);
  - the segment header (48 bytes, `DJH1`, unseeded CRC);
  - the seal marker (32 bytes, `DJS1`);
  - the record header (96 bytes, `DJR1`), with its 14 kinds, 8-byte alignment and the
    `write` mtime prefix;
  - the catalog entry (72 bytes) and the trailer (32 bytes, `DJT1`).
- **CRC32C**, seeded and unseeded.
- **The slot scanner**, with resynchronisation on 8-byte boundaries.
- **The `format` file's atomic replace** (`format.tmp`, fsync, rename, directory fsync).
- **The platform layer:**
  - preallocate plus zero-fill;
  - `fdatasync` on files and `fsync` on the directory on Linux;
  - `F_FULLFSYNC` on macOS;
  - the Windows directory flush;
  - direct-I/O reads (`O_DIRECT`, `F_NOCACHE`, `FILE_FLAG_NO_BUFFERING`);
  - refusing `nobarrier`, CoW-without-`+C` and a non-subvolume Btrfs directory;
  - thin-provisioning detection.

This epic carries the main fuzz targets, one per parser, and golden vectors checked into
`testdata`. Metrics: `torn_tails` and `scan_resyncs` hooks at the scanner. Candidate
subtasks: one per structure (6), scanner, `format` replace, platform per OS, CoW and
barrier probes.

### Epic 4 — Placement index and versions ⛔ (≈11 subtasks, B)

Epic 4 implements RFC 1 §4 and the version rules of §3.4 and §4.3. It is pure in-memory
code.

- **Index structure:**
  - an outer hash map from `FileID` to a per-file B-tree keyed on start offset;
  - held extents (disjoint), retained extents (may overlap) and removal markers;
  - split, narrow and remove in `O(log n)`;
  - span queries returning held extents and exact gaps in `O(log n + k)`.
- **Ordering:**
  - precedence by (version, sequence);
  - one sequence counter and one version counter per journal;
  - the version limit (`SetVersionLimit`, `Draw`, issue only below the mark);
  - the per-file change version;
  - the 4,096 floor buckets.
- **Bounds and reclamation bookkeeping:**
  - `ExtentLimit`, including the one-eighth allowance for removal splits;
  - held and retained bytes per segment in `O(1)`;
  - the per-(file, segment) drop ranges (at most eight, merging the smallest gap).
- **Instrumentation:**
  - comparison counters for the RFC 1 §9.7 "index cost" and "report cost" checks;
  - a bytes-per-entry check (≤ 56 at 10⁶ entries).

Fuzz: operation sequences against the reference model. Candidate subtasks: B-tree, file
index, span query, retained set, markers, precedence, counters and limit, floor buckets,
extent bound, drop ranges, per-segment accounting.

### Epic 5 — Segments, streams and accounting ⛔ (≈11 subtasks, A)

Epic 5 implements RFC 28 §2.2 and §2.4 (writing side), and RFC 1 §6's segment allocation,
§7.1's auto-retire mechanism, §7.3, §7.4 and §8.5.

- **Segments:**
  - fixed cap, allocated whole;
  - ids rising above every id on disk;
  - several append streams;
  - the file-append serialisation point;
  - writes split at the cap.
- **Spare pool:**
  - `.spare.tmp`, then rename, then directory sync;
  - pool target;
  - headroom and repack-reserve spares kept apart;
  - an immediate `spare` refusal and `spare_refusals`.
- **Seal and catalog:**
  - seal marker, footer and trailer, written durably and at most once;
  - an early seal truncates the free tail, a full seal keeps the cap;
  - idle seal.
- **Retirement:**
  - recycle (close, rename to a fresh id, directory sync, zero the slots);
  - unlink, with descriptors closed first;
  - ending a segment after a failed or short append on a recycled segment.
- **Accounting:** `UsedBytes` at the cap until unlink. Retirement is idempotent and
  subtracts exactly once. A double retire logs at `Warn` and is counted.
- **Descriptors:** a per-process bounded cache that reopens on miss, with
  `segment_reopens`.
- **Storage guard:** a shared/exclusive guard per segment that is not reader-preferring.

**The first two subtasks deliver the S3 minimal stream:** one stream, append and read,
with no pool and no seal.

### Epic 6 — Durability and loss (≈10 subtasks, A)

Epic 6 implements RFC 1 §5 and §3.8.

- **Sync:**
  - `Sync(ids…)` with group commit, one sync covering every waiter on the stream (the
    counted check is at most one sync per four writes);
  - the `bounded` policy (1 s timer per stream, on the virtual clock) and `standard`;
  - live `SetSyncPolicy`;
  - directory sync before any covering `Sync` returns;
  - the §5.1 ordering rules as assertions.
- **Failed-sync windows:**
  - never append to, or seal, a failed segment;
  - re-append with the original numbers, only from an in-memory copy or a re-read that
    verifies against the checksum kept at write time;
  - header-only records are always re-appended;
  - or fail the window, with unsynced loss records;
  - a superseded record is kept until its superseding write syncs (retained);
  - each failed window is reported once per file, and does not wedge the file.
- **Loss handling:**
  - loss records, synced vs unsynced, with their precedence;
  - the loss event ring with sequence numbers;
  - the loss generation, monotonic per directory within a process and raised before the
    failing `Sync` returns.
- **Metrics:** `syncs`, `sync_failures`, `sync_reappended_bytes`, the `SyncDuration`
  histogram, `LastSyncError`.
- **Span:** `sync`.
- **Counted check:** the mixed-durability check.

### Epic 7 — Recovery and identity (≈12 subtasks, A)

Epic 7 implements RFC 28 §3 and RFC 1's `Init`, `Open` and `Restamp`.

- **Identity and attach rules:**
  - `Init` refuses a non-empty directory;
  - `Open` never creates, and checks `expect`;
  - `expect` = 0 fails with a diagnosis naming the identity and installation ID;
  - `Restamp` rewrites only the matching identity;
  - orphans are age-gated;
  - foreign segments are reported and left alone.
- **Rebuild:**
  - by catalog or by scan, giving **identical** indexes;
  - in any segment order, concurrently;
  - markers start unsettled;
  - retained extents rebuilt from hold, unhold, stamp and offloaded records;
  - loss records re-entered in the ring as "found at open";
  - forget records applied.
- **Tail handling:**
  - torn tail vs corruption;
  - the unproven tail is fsynced, re-read with direct I/O, and re-appended from the repack
    reserve on failure.
- **Offload state:** rebuilt from offloaded and unmark records only. The stale rule drops
  content older than the reported range.
- **Counters:** the next sequence and version exceed everything on disk and the floor.
  Over-bound indexes still open.
- **Spares:** empty segments are re-adopted as spares.
- **Tag conflicts:** a `FileID` rebuilt under two tags is refused through either tag and
  named in `TagConflicts` until one of the tags is forgotten (RFC 1 §3).
- **Inspection and verify** (RFC 1 §4.4): read out the index, per-segment accounting, and
  a report-only verification. Repair is a separate, explicit action.

Metrics: `Recovery` (catalog vs scan count and duration), `RejectedSegments`,
`DamagedSegments`, `corrupt_extents{outcome}`, `stale_extents`. Span: `open`, with child
spans per phase. Also the J6, J7 and J9 benchmark harnesses (RFC 28 §4.3), recorded but
not gated.

### Epic 8 — Data path and capacity ⛔ (≈13 subtasks, B)

Epic 8 assembles epics 4–6 into the public API's core (RFC 1 §3–§3.2, §3.4, §3.6, §3.7,
§3.10, §6, §8).

- **Journal and handles:**
  - `Share`, which raises the counter above the share's floor and takes its sync policy;
  - `Handle.Close`, which waits for in-flight calls;
  - **a file is its share's** (RFC 1 §3): a call that appends a record of an untagged
    file claims it for the handle's tag, and the check and the claim are one step under
    the file append domain with the file index held for the write. A call naming a file
    under another tag is refused with `ErrOtherShare`, naming both tags, and serves,
    appends, changes and offers nothing of it. A read claims nothing. A file keeps its
    tag while any record of it remains, even after its index entry is dropped.
- **Core operations:**
  - `WriteAt`: the version is assigned inside the file append, and an extent is published
    only after its record is written;
  - `ReadAt`: an exact `missing` list, an untouched buffer, `held` versions, `asOf`;
  - `Fill`: the absence check is atomic with the write, it refuses when older than the
    file or its bucket, and it sets the offloaded bit. It is **not** a write path, so it
    has a separate entry point;
  - `Truncate`, `Deallocate`, `Delete`, `CloneTarget`, with markers;
  - `Since` and `Settle`;
  - `Extents`, `Files`, `DirtyFiles` (O(dirty files), no walk).
- **Capacity:**
  - atomic reservation against the share limit and the journal maximum;
  - `Reserve` and `Reservation`, covering pieces, footer entries, index entries, versions
    and spares;
  - headroom and the below-half rule;
  - the repack reserve;
  - immediate, named, I/O-free refusals.
- **Concurrency:**
  - the five lock domains and their total order, with a lock-order recorder in tests;
  - no I/O under the file index;
  - shutdown.
- **`Stats` aggregation:** served from counters, keeping the documented inequalities, with
  the per-share breakdown.

Metrics: `written_bytes`, `filled_bytes`, `fill_refused`, `reservation_refused`,
`headroom_draws`, `index_refusals`, `TagConflicts`. Spans: `fill`, `reserve_refused`.

### Epic 9 — Offload, reclamation, holds and tags (≈13 subtasks, B)

Epic 9 implements RFC 1 §3.3, §3.5, §3.11, §7.1–§7.2, and the tag lifecycle of §3.

- **Offload:**
  - `Offload` and `OffloadMany`;
  - a frozen `Offered` reader that verifies whole payloads before returning any byte;
  - `limit` with artificial ends;
  - `widen` neighbours;
  - `report` marking block by block in O(k log n);
  - partial success honoured;
  - unoffered reports rejected;
  - a removal during an offer does not interrupt it;
  - pin bounds (64 per offer, 256 journal-wide);
  - no pin on a segment under repack.
- **Marking and release:**
  - `MarkOffloaded` and `Unmark`, with the below-half rule;
  - `Release`, all-or-nothing, with release records;
  - auto-retire of segments that are no longer held, once their records are durable.
- **Repack:**
  - selection by live payload, within scope;
  - verify the whole source;
  - copy only the live parts, coalesced;
  - copies get a new sequence number, carried header-only records keep theirs;
  - drop decisions per extent by version, decided after the pass's copies;
  - ordering: copy, then sync, then repoint, then unlink;
  - copies draw on the reserve;
  - divisible and not starved;
  - `freed` reported only once durable.
- **Snapshot holds:**
  - `Hold`, `Unhold`, `Stamp`, `Held`;
  - retained-extent lifetimes;
  - `Superseded` in offers.
- **Tags:**
  - `Forget` (one durable header-only record, refused while a handle is open, idempotent),
    which also releases the files the tag claimed and clears its `TagConflicts` entries;
  - `Strand` (in-memory, excluded from serve, evict and offer).

Metrics: `offered_bytes`, `marked_bytes`, `released_bytes`, `freed_bytes`,
`repacked_bytes`, `PinnedSegments`, `DropRanges`. Spans: `offload_pass`, `repack_pass`.

### Epic 10 — Integration and the v1→v2 upgrade 🔀 (≈14 subtasks, B wiring + A config, upgrade and exporters)

Epic 10 connects v2 to the rest of DittoFS. The v1 survey found these callers:

- `pkg/controlplane/runtime/shares/` (`journal_open.go`, `blockstore_config.go`,
  `lifecycle.go`, `service.go`, `blockstore_ops.go`, `cold_seed.go`);
- `pkg/block/engine` (through the wide `journal.LocalStore`);
- `pkg/config/blockstore.go`;
- `cmd/dfs/commands/start.go`;
- `pkg/controlplane/runtime/init.go`;
- `pkg/controlplane/store/journal_root_migration.go`;
- `internal/adapter/common/errclassify.go`;
- the share API handlers and dfsctl `share` and `store block` commands;
- the test runtimes, fixtures and benches.

Work:

- **Configuration:**
  - a list of journal devices (path, maximum) replacing the per-share root under v2;
  - share-to-device placement;
  - per-share limit and `sync.policy`;
  - `ExtentLimit`, `DescriptorLimit`, `headroom_records`;
  - `salvage.path`;
  - start refused when `storage` has no device.
- **Per-device journal registry in the Runtime.** This replaces "open one journal per
  share" with "open one journal per device at start, attach a `Handle` per share":
  - `AddShare` maps to `Share`;
  - `RemoveShare` maps to `Close` then `Forget`, in the order RFC 16 §2.3.2 requires;
  - `CloseBlockStores` closes the journals.
- **Metadata the journal relies on.** RFC 16 defines all of it, and
  [#3120](https://github.com/marmos91/dittofs/pull/3120) implements it on
  `feat/storage-v2`. E10 consumes it and adds no control-plane fields of its own:
  - `entity.Installation`, the installation ID;
  - `Share.Tag`, minted from the installation's tag counter and never reused;
  - `VersionLimit` and `RaiseVersionLimit`, the durable per-share version limit
    (`S‖id‖vl`);
  - `VersionFloor`, **derived** from the version index, never stored: a stored floor could
    lag a file's `Version` and let a version be issued twice;
  - the `J‖journal` record, keyed by journal identity, naming the node and device a
    journal lives on (RFC 16 §4.2). #3120 declares its key kind; whether it writes the
    record is confirmed when E10 starts.
- **Engine bridge.** v2 exposes none of v1's `Hydrate`, `Commit` or carving, so the engine
  gets a narrow local-tier interface:
  - offload pass → carver and syncer → `report`;
  - cold read → `Fill`;
  - eviction → `Release` + `Repack`;
  - start → reseed `MarkOffloaded`/`Unmark`, `Strand` unclaimed tags, `Forget` removed
    shares, `Since` replay, settle after the metadata commit.
- **Version floor and version limit** (RFC 1 §4.3, RFC 8 §2.5):
  - each journal opens, and each `Share` is attached, with the **larger** of the share's
    `VersionFloor` and its `VersionLimit`, so the counter restarts above every version a
    crash may have lost;
  - before any version is issued, every served share's limit is written at the journal's
    `NextVersion` plus one block (proposal: 2²⁴), as a tracked max-write through
    `RaiseVersionLimit`. Only after every share's write commits is `SetVersionLimit`
    called, with the **smallest** limit over the journal's shares;
  - while serving, the trigger is evaluated on every version issued, not on a timer. When
    `VersionsLeft` falls below the larger of half a block and the peak version rate (the
    busiest second of the last 60 s) times the margin (30 s self-fence + existence age +
    1 s), every served share's limit is raised by a block, and the new limit is passed only
    after every raise has committed. The raises run in as many transactions as the store's
    key budget requires, never one over every share;
  - a write that reaches the limit is answered retry-later (`ErrDelay`), never no-space.
- **Store contracts the bridge relies on:**
  - the offload `report` carries the extents `Content.Commit` returned, and is called only
    after that commit; the block's put intent is recorded before the put;
  - `Settle` is called with the file's `Applied` from the store, not with the version
    submitted, because `Existence.Commit` may split a file over several transactions or
    fail one file of a batch.

  This follows the start order of RFC 8 §2.5 as far as the current engine allows. Gaps are
  recorded as `decision:` markers.
- **Error mapping:** `ErrNoSpace{limit}` maps to retry-later or no-space following
  RFC 8 §10.2, and `ENOSPC` stays distinct. `ErrOtherShare` maps to `ErrCorrupt`, never
  retried. Loss generation is folded into the NFS write
  verifier.
- **Salvage of stranded tags after serving.** Exports go under `salvage.path`, mode 0700,
  sealed with a salvage data key for encrypting shares, and a failed export becomes a
  health condition.
- **Exporters:**
  - `Stats` → Prometheus collectors labelled `journal` (and `share`), plus a `version`
    label (`v1` or `v2`). Metric names carry no version suffix (epic 1, D1);
  - `Hooks` → OpenTelemetry spans and the existing metrics recorder. Span names and
    attribute keys carry no version suffix either.
- **API and CLI:** API and `dfsctl` show journal stats per device and per share.
  `go run ./cmd/gendocs` is rerun afterwards.
- **System e2e tests.** E10 writes the system scenarios catalogued in epic 2 as tests,
  lists them as known failures, and turns green those its own wiring satisfies.
- **v1 retirement:** v2 replaces v1 in the Runtime, with no configuration key choosing
  between them. The `pkg/block/journal` callers listed above move to v2, and v1's package
  is deleted once none is left. What remains of v1 is the upgrade path below.

#### Upgrade from v1

The upgrade is one-way and needs no command. It rests on three things:

- **The `v1-clean` marker**, shipped in v1 ahead of v2
  ([#3156](https://github.com/marmos91/dittofs/issues/3156), released as v0.36.0). v1
  deletes `<journal.path>/v1-clean` before opening any journal. On a clean stop it drains
  every share to the remote and writes the marker only if all of them reached zero
  unsynced bytes. The marker exists only if deleting every v1 journal directory loses no
  data. A crash or a failed drain leaves no marker.
- **An adoption record**: the RFC 16 `Installation` record
  ([#3120](https://github.com/marmos91/dittofs/pull/3120)), minted by the explicit
  initialisation step and written last when v2 takes over the installation. This record,
  and nothing else, says that v2 is active. A configuration key
  could not do this job: the operator can edit it, so it can disagree with what is on
  disk.
- **A read-only preflight**, the first thing `dfs start` does. It runs before any store
  is opened for writing and before any device is initialised:

  | Adoption record | v1 directories | `v1-clean` | Action |
  | --- | --- | --- | --- |
  | present | any | any | normal start; v1 directories are leftovers |
  | absent | absent | — | fresh install: adopt |
  | absent | present | present | clean upgrade: adopt |
  | absent | present | absent | **refuse**: v1 stopped uncleanly, or a v1 release older than v0.36.0 wrote it |

  The refusal tells the operator to start v0.36.0 or later, let it sync, stop it cleanly,
  and retry. Because the preflight has written nothing, that is always possible.

Adoption order:

1. preflight (read-only);
2. initialise the v2 devices and whatever the RFC 16 store needs besides `Installation`.
   **Nothing the previous v1 release reads is modified**, so it still starts;
3. write `Installation`: the point of no return;
4. stamp a future `format` version in every v1 journal directory, so that v1 refuses them
   with `ErrFutureFormat` instead of serving stale data. This step is repeated at every
   start while v1 directories exist, so a crash between steps 3 and 4 leaves no gap.

Storage v2 also replaces the metadata store (RFC 16), so this preflight gates the storage-v2
upgrade as a whole, not only the journal. Migrating the metadata itself from the v1 stores
is outside this plan; this section fixes only the gate and its ordering.

A crash before step 3 leaves `v1-clean` in place: a re-run adopts again from the start,
and the previous v1 release still starts. v1 directories are never deleted automatically.
Removing them is an explicit cleanup.

What the upgrade costs:

- after step 3 there is no downgrade;
- v1's cold cache is dropped, and v2 refills it from the remote through `Fill`;
- a local-only share has no remote, so its v1 journal holds the only copy, and #3156 does
  not write the marker while such a share holds data. Such an installation stays refused
  until a migration path for local-only shares is decided here.

### Epic 11 — E2E green 🔀 (≈8 subtasks, A + B)

Epic 11 takes the suite from "listed" to "passing". The work:

- triage every remaining known failure into "fix" or "the test is wrong" (the latter
  decided by both engineers and recorded);
- fix each one and remove its entry;
- run the model-based test on many seeds and keep any failing seed as a regression case;
- run the hours-long soak (RFC 1 §9.1) with flat counters, descriptors and footprint under
  the profiler;
- run benchmarks J1–J5 and J8 on the reference box, with results recorded per the RFC
  index's rules;
- register the suites in `test/suites.yaml` at the right tiers: conformance per change,
  model on many seeds and soak nightly, benchmarks after merge and daily;
- confirm the list is empty for v2-specific tests.

The exit criterion is an empty v2 known-failure list. Any entry that remains must carry an
explicit, recorded decision.

### Epic 12 — Dashboards (≈6 subtasks, A)

The repository has no dashboards today. Epic 12 builds four Grafana dashboards as JSON
under `monitoring/grafana/journal/`, using the exporters from epic 10. Dashboard titles,
UIDs and queries say `journal`, never `journalv2`:

1. **Capacity and pressure**, per journal and per share:
   - `UsedBytes` vs `MaxBytes`, limit;
   - held, dirty and unreclaimed bytes;
   - headroom and repack reserve;
   - refusals by named limit;
   - `ExtentCount` vs `ExtentLimit`;
   - `VersionsLeft`.
2. **Durability and integrity:**
   - syncs and failures;
   - the `SyncDuration` histogram;
   - `LossGeneration`;
   - corrupt extents split into data loss and repairable;
   - stale extents, torn tails, scan resyncs;
   - damaged and rejected segments;
   - recovery time.
3. **Offload and reclamation:**
   - offered vs marked bytes;
   - `OldestDirty` (backlog age);
   - released vs freed bytes;
   - repacked bytes and write amplification;
   - pinned segments;
   - fill and `fill_refused`;
   - removal markers.
4. **Resources:** open descriptors vs limit, segment reopens, segments vs sealed, drop
   ranges, index memory estimate.

Each dashboard is tested:

- JSON schema lint;
- every PromQL query parses;
- every referenced metric exists in the exporter's registry, which catches a renamed
  metric;
- a scripted workload against a test server makes every panel non-empty, with alert-worthy
  panels (`data_loss`, `sync_failures`) driven by injected faults.

Candidate subtasks: one per dashboard (4), the test harness, and alert rules for the RFC's
named alerts (any `data_loss`, a growing `OldestDirty`, `RemovalMarkers` growing).

---

## 6. Risks and open points

- **The engine (RFC 8) does not exist yet.** Epic 10 bridges v2 into today's engine rather
  than building RFC 8. Start-order steps the current engine cannot honour are recorded as
  `decision:` markers, not silently dropped.
- **E10 depends on #3120.** The metadata RFC 1 assumes (installation ID, share tags, the
  durable version limit, the derived version floor, `Applied`) comes from RFC 16's store,
  which [#3120](https://github.com/marmos91/dittofs/pull/3120) implements. E10 cannot
  finish before it lands on `feat/storage-v2` (S5).
- **E10's estimate is soft.** Its ≈14 subtasks bridge into an RFC 8 engine and an RFC 16
  store that are both still being built. It is re-estimated when E10 starts.
- **Segments vs staging files** (RFC 28 §2.6) is gated by benchmark J9. This plan assumes
  segments. A J9 result that overturns that reopens epics 3, 5 and 7.
- **Cluster-only items** (RFC 10 replication, a non-zero epoch, the `generation` field,
  extension kinds 128+) are out of scope. The format reserves them, and the tests assert
  that they are refused.
- **Salvage** depends on RFC 5's master key and RFC 26's recovery import. Where those are
  missing, epic 10 implements the export path and its failure condition, and records what
  is stubbed.
- **Proposed constants** (256 MiB segment cap, 1 s sync bound, 64/256 pin bounds,
  65,536 headroom records, 4,096 floor buckets, 30 s / 16 MiB idle seal) are implemented
  as named constants with their RFC `decision:` notes, to be settled by measurement in
  epic 11.
