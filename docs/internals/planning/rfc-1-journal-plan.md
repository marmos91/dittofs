# RFC 1 — journal v2: implementation plan

**Scope:** [RFC 1 — the journal](../rfcs/rfc-1-journal.md), together with the parts of
[RFC 28 — journal format](../rfcs/rfc-28-journal-format.md) it cannot work without, and the
release-1 parts of [RFC 15 — topology](../rfcs/rfc-15-topology.md). The test foundations
follow [RFC 29 — test harness](../rfcs/rfc-29-test-harness.md).
**Team:** three people. **Track A** (storage) is @viquitorreis, **track B** (model and
operations) is @didimelli, and @simonebaracchi coordinates (§3.4).
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
- **Epic 10** is the journal's share of starting and initialising an installation: the
  start check, journal init inside the initialisation step, and the wipe of devices left
  by an interrupted step.
- **Epic 11** is stage 3 of [#3119](https://github.com/marmos91/dittofs/issues/3119): the
  engine (journal, carver, syncer, remote) rebuilt on the RFC 16 store, behind the
  filesystem-service engine interface. It is planned once that interface is agreed (S4a).
- **Epic 12** turns the e2e suite green.

Each epic is sized at about 10 subtasks. The detailed subtask breakdown of an epic is
written when that epic starts, under the rules of §1.5.

### 1.3 What epic 1 must deliver

Epic 1 is **interfaces only**. Its output is code that compiles and does nothing:

- **The packages it declares into**: the public package, `internal/core` and
  `internal/storage` (§4, epic 1).
- **Every exported symbol from RFC 1 §3–§4**, with its exact signature. That means
  `Init`, `Open`, `Restamp`, `Journal`, `Handle`, `Reservation`, and every method in
  §3.1–§3.11, §4.3 (`Draw`, `SetVersionLimit`), §4.4 (inspection) and §7.2 (`Repack`).
  It also means every value type (`FileID`, `Extent`, `Held`, `Version`, `ShareTag`,
  `SyncPolicy`, `Offer`, `Change`, `CloneSpec`, `RepackScope`, `Stats`, …) and the error
  set (`ErrNoSpace` carrying which limit refused, the stale-fill error, closed, forgotten,
  …).
- **The storage seam** (RFC 1 §1.3): `storage.FS`, `storage.File` and `WithStorage`. It
  is the one internal interface epic 1 declares, because it has two consumers before any
  implementation exists: `WithStorage` and epic 2's crash seam. Format and platform land
  with E3. The index, segments, durability, recovery and reclaim land as concrete types
  with E4–E9, each with its first caller (§1.5, rule 12).
- **How dependencies are injected**: clock, storage seam, tracer, logger, limits and
  settings. RFC 1 fixes `Open(dir, floor, expect)`, so epic 1 appends variadic options
  that keep that signature. `Open` refuses to run without a clock (`ErrNoClock`), because
  RFC 8 §2.1 rules out a silent fallback to the system clock (epic 1, D5).
- **The observability contract**: the `Stats` struct with every field of RFC 1 §3.7 and
  §3.9, a `Tracer` function type for spans built from standard-library types only, and a
  `*slog.Logger` option. There is no event callback: every condition worth an event is
  already a `Stats` field (epic 1, 1.7). The journal may not import a metrics or tracing
  library, so epic 11 adapts `Stats` to Prometheus and the tracer to OpenTelemetry.
- **Stub bodies**: every function returns `ErrNotImplemented` with zero values, or an empty
  iterator, and never panics (epic 1, D8, a review rule). This is what lets epic 2's tests
  compile and fail on their assertions. `Version`'s order and byte encoding are the one
  piece of behaviour implemented, because other components store them.

**Exit criteria:**

- `go build ./...` and `go vet ./...` pass.
- The kept tests pass: the import rule, the RFC signature compile check, typed errors
  matching their sentinels, and `Version`'s byte order (§1.5, rule 10).
- The import test passes, allowing stdlib only (plus `golang.org/x/sys` in
  `internal/platform`, epic 1 D6) and forbidding `pkg/block/journal`.
- Every symbol carries a doc comment stating its contract in terms of the code.
  `CLAUDE.md` keeps RFC section numbers out of source comments, so the map from each
  symbol to its RFC section lives in the epic's planning file instead.
- The team has reviewed and signed off. Changing a frozen signature after that needs the
  team to agree, and the change is noted in this file.

### 1.4 What epic 2 must deliver

Epic 2 makes "done" measurable before any implementation exists, following
[RFC 29](../rfcs/rfc-29-test-harness.md). It has three outputs.

**1. Shared test components that block everything else:**

- **Clock and fault implementations.** The set has one clock interface
  ([RFC index, test harness item 1](../rfcs/rfc-index.md)), which RFC 8 §2.1 declares in
  standard-library types (`Now`, `Elapsed`, `After`). The journal declares the subset it
  uses, so the production clock and the test clock satisfy it as they are, with no
  adapter (epic 1, D3). Epic 2 implements `TestClock` in `pkg/sim`, and the seeded fault
  schedule there too: `Fault` lands with the crash seam, its first consumer. `pkg/sim` is
  node-wide (RFC 8 §2.1) and imports nothing of the journal, so other components, open
  state first, use the same `TestClock` and `Fault` (epic 2, D5). The
  production clock (`pkg/clock`: `Clock` and `System`) lands with its first caller, the
  stage-3 composition root (epic 11).
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
- **Known failures, graded by `dt`.** RFC 29 §4.7's `known.yaml`, which `dt` already
  reads and grades, so epic 2 writes no reader of its own (epic 2, D1). A listed test that
  fails is graded as expected. **A listed test that passes fails the run**
  ([RFC index, harness requirement 6](../rfcs/rfc-index.md)), so a test that turns green
  forces its entry out of the list. An entry also names the failure it expects, once
  `dt` can match one ([#3391](https://github.com/marmos91/dittofs/issues/3391), an
  optional `message:` in RFC 29 §4.7), so a test failing for a new reason is graded new
  rather than known (epic 2, D4).
- **Profiling harness.** A helper wrapped around each test and benchmark that snapshots
  goroutines, open file descriptors and heap before and after. It fails on leaked
  goroutines or descriptors and on heap growth past a threshold, and writes pprof profiles
  as artefacts when it fails. The allocations-per-operation assertion of the RFC 1 §9.7
  counted checks lands with its first hot path, in epics 4 and 8, since a threshold cannot
  be set against a stub. The allocation and heap assertions are skipped under
  `-race`, which changes both, and run in a separate non-race job. The heap threshold is
  calibrated per test with slack, not one global number, so that it does not flake.

**2. The red end-to-end suite, at two levels:**

- **Component e2e (conformance).** These drive the public v2 interface on a real
  filesystem in a temporary directory, through the storage seam, on virtual time. They
  cover every named check of RFC 1 §9.4 (Groups A–D), RFC 1 §9.2's edge cases, and
  RFC 28 §4.1–§4.2's format and recovery checks. A check that needs `internal/format` or
  `internal/platform` lands with epic 3, which creates those packages, not here: the
  platform checks of RFC 28 Appendix A, the run of storage-agnostic tests on the
  operating system's storage, and any test that builds on-disk bytes with the encoders
  (§5, epic 3). Epic 2's format checks write their bytes by hand from RFC 28 §2.
- **System tests, as a catalogue only.** They cover what only the composed system shows:
  one journal per disk serving several shares, restart, device loss, a full disk, and the
  start check that refuses v1 state. No `dfs` can run v2 before epic 11 wires it, and there
  is no configuration key to select it, so a system test written now could only fail at
  start, never on its own assertion. Epic 2 therefore writes no system scaffolding: the
  existing `e2e` suite and its framework already start a real `dfs` and mount NFS and SMB,
  and the fixtures specific to the journal land with the first test that uses them (epic
  2, D11). Epic 2 keeps the catalogue, split by the tier each row runs at: the filesystem
  service's integration tier (`vfs.Filesystem` over the real engine, no kernel client) or a
  real `dfs` with kernel clients (epic 2, §7). **Epic 10 writes the start-check
  refusals**, which need no engine, and **epic 11 writes the rest**, listed as known
  failures. Epic 12 turns them green.

The scenario catalogue the suite must cover is in §5, epic 2, and in full in the epic's
file.

**3. A traceability matrix.** It maps every RFC 1 §9.4 row and every RFC 28 §4.2 row to
the test that implements it, and every other suite test (edge cases, model, counted,
damage and clock checks) to its row of RFC 1 §9.1, §9.2 and §9.7, RFC 28 §4.1 and §4.4,
RFC 0 §1.4 or RFC 8 §15 (epic 2, D3). A Go test fails CI when a row has no test, or a test
no row; `dt` fails it when a test was expected to run and did not. A check that did not
run is a failure ([RFC index, test tiers](../rfcs/rfc-index.md)).

**Exit criteria:**

- The whole suite compiles against epic 1's stubs and runs in CI.
- Every test fails without a build error, a panic or a skip. Until `Open` works, every
  test fails in the fixture, not on its own assertion (epic 2, D4).
- Every test is on the known-failure list, so the run is green.
- The matrix has no unmapped rows.
- The seam, clock, model and profiler each have their own unit tests. These are green from
  day one, because they do not depend on v2.

### 1.5 General rules for each epic

> **Instruction for whoever expands an epic into its detailed description and subtasks,
> human or AI:** copy this section so that the resulting epic is self-contained and does
> not depend on this file. 
> Every subtask must state how it meets each rule that applies to it. 
> A rule that does not apply is named with the reason.

1. **Independent unit tests.** Each epic carries its own unit tests. It may use another
   epic's pure in-memory implementation (E4's index, E3's codecs), the implementation of
   an epic it depends on once that has landed (E8 on E5's segments and E6's `Sync`), and the test components from epic 2. Epic 1 declares no internal interface
   to fake (rule 12).
2. **Fuzz tests where relevant.** Use Go native fuzzing (`testing.F`) on every parser of
   bytes the journal did not just write: records, segment headers, seal markers, catalogs,
   trailers, the `format` file. Also fuzz-drive operation sequences against the reference
   model. The property is that any input is either accepted as valid or rejected: never a
   panic, and never an extent the bytes do not contain.
3. **Metrics, and tracing where relevant.** Every counter and gauge RFC 1 §3.7 and §3.9
   assigns to the epic's area is maintained, and exposed through `Stats` (served from
   maintained counters, never a walk). Spans go through epic 1's standard-library
   `Tracer` and logs through the injected `*slog.Logger`, never a metrics or tracing
   library. Operations worth a span:
   open and recovery, sync, offload pass, repack pass, fill, and a refused reservation.
4. **Profiling.** Tests and benchmarks run under epic 2's profiling harness and fail on
   goroutine, descriptor or memory leaks and on anomalous resource growth. Hot paths
   (`WriteAt`, `ReadAt`, `Stats`, `report`, index lookup and insert) carry
   allocations-per-op assertions and the counted checks of RFC 1 §9.7. Allocation and
   heap assertions run outside `-race` only, because `-race` changes both.
5. **Public interface and internals.** Tests cover the epic's internal package API and the
   public v2 interface behaviour it enables, including concurrency under `-race`. Per the
   project rules, test at the layer that **consumes** a value, not the one that produces
   it. A regression test is validated by reverting the fix and watching it fail **on its
   assertion**.
6. **Known failures shrink.** Every e2e test an epic turns green is removed from the
   known-failure list in the same change. The runner fails if a listed test passes.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on
   `pkg/block/journal`. The import test enforces it. v1 code is neither ported nor
   bridged: it is deleted with the rest of v1 at the cutover (stage 7 of #3119). v2
   checks only whether v1 state exists, to refuse it at start (E10), and never reads
   v1's format.
8. **Real storage for storage properties.** No in-memory filesystem stands in for a check
   about durability, torn tails, unlink durability or corruption (RFC 1 §9.5). In-memory
   stand-ins are allowed only for pure logic (index, codecs).
9. **Code comment conventions** (`CLAUDE.md`). No epic, issue or plan IDs in source
   comments. Use `ponytail:` for knowingly-simple implementations and `decision:` for
   deliberate behaviour. Where the RFC already carries a ponytail or decision note, its
   marker goes at the code site that implements it.
10. **A test must be able to fail.** Every test is code maintained for the life of the
    project. Keep a test only if a plausible change would make it fail and nothing else
    would catch that change: not the compiler, not the PR diff, not a later epic's tests.
    A test that checks two `errors.New` values differ, that the standard library's
    `errors.As` works, or that a stub returns zero does not qualify. A test that a
    little-endian `Version.Bytes` would fail does. Review rules that no test can check
    (for example, stubs never panic) stay review rules.
11. **A symbol is exported only if RFC 1 §3 uses it.** Everything exported is frozen at
    S1, so a helper exported for convenience becomes a contract. Helpers the internal
    packages need (`Less`, `IsZero`, `End`, `Overlaps`, `Seq`, the proposed constants)
    stay in `internal/core`.
12. **Each subtask creates only what it declares.** No empty package and no stub without
    a caller. A package, an option or a test helper lands with its first consumer.

---

## 2. Epics at a glance

| # | Epic | Content | Est. subtasks | Owner | Blocks |
| --- | --- | --- | --- | --- | --- |
| 1 | **Interfaces** ⛔ | Public package, exported symbols, signatures, storage seam, `Stats`, `Tracer`, errors, stubs | 9 | team | **everything** |
| 2 | **E2E suite + test foundations** ⛔ | Test clock and fault schedule, crash-simulating seam, reference model, profiler, red conformance suite graded by `dt` from `known.yaml`, system scenario catalogue, traceability matrix | 17 | team | **all testing** in 3–12 |
| 3 | **On-disk format and platform layer** ⛔ | RFC 28 codecs, golden vectors, CRC32C, `format` file replace, per-OS sync, allocation, direct I/O and CoW checks; declares `internal/format` and `internal/platform`; the conformance tests that need them | 12 | A | 5, 7 |
| 4 | **Placement index and versions** ⛔ | Per-file B-tree, held, retained and marker sets, precedence, sequence and version counters, version limit, floor buckets, extent bound, drop ranges, per-segment bytes | 11 | B | 7, 8, 9 |
| 5 | **Segments, streams and accounting** ⛔ | Segment lifecycle, spare pool, append streams, seal, catalog and free tail, recycle and unlink, descriptor cache, storage guard, footprint accounting | 11 | A | 6, 7, 8 |
| 6 | **Durability and loss** | `Sync`, group commit, `bounded` and `standard` policies and timer, failed-sync windows (re-append or fail), loss records, the loss ring and generation | 10 | A | 8 (Sync), 11 |
| 7 | **Recovery and identity** | `Init`, `Open`, `Restamp`, rebuild by catalog and by scan, torn vs corrupt, unproven-tail re-verify, offload-state rebuild, stale rule, spare re-adoption, inspection and verify | 12 | A | 10, 11 |
| 8 | **Data path and capacity** ⛔ | `Journal`, `Share`, `Handle`, `Close`, `WriteAt`, `ReadAt`, `Fill`, removals, `Since` and `Settle`, `Draw`, `Reserve`, limits, headroom, named refusals, lock domains, `Stats` | 13 | B | 9, 11 |
| 9 | **Offload, reclamation, holds and tags** | `Offload`, `OffloadMany`, pins, `MarkOffloaded`, `Unmark`, `Release`, auto-retire, `Repack` and drop test, snapshot holds, `Forget`, `Strand` | 13 | B | 11 |
| 10 | **Start and initialisation** | Start check and its two refusals, `journalv2.Init` in the initialisation step and the `J‖journal` writes, wipe of devices left by an interrupted step, device config keys | 5 | A | 11 |
| 11 | **Engine on the RFC 16 store** 🔀 | Stage 3 of #3119, planned after S4a: engine wiring (`Fill`, `report`, `Release`), per-device registry, version floor and limit, store contracts, error mapping, salvage, exporters and alert rules, API and CLI, system e2e tests | planned after S4a | team | 12 |
| 12 | **E2E green** 🔀 | Burn down the known-failure list, soak, benchmarks J1–J5 and J8, CI tier registration, profiling baselines | 8 | team | release |
| | | | **~121 + E11** | | |

⛔ = blocker: other epics cannot finish until this one has delivered.
🔀 = synchronisation point: the team converges, and the work that follows depends on
everything before it.

---

## 3. Synchronisation points, blockers and the split

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
        │                   │
        ▼                   │
   A: E10 start + init      │        S4: core complete (E3–E9)
        └─────────┬─────────┘
                  │   ◄──────────── S4a: engine interface agreed with rif (#3119 stage 2)
                  ▼
       E11 engine on the RFC 16 store    S5: wired
                  │
                  ▼
            E12 e2e green
```

### 3.2 Synchronisation points

- **S1. Interfaces frozen** (end of E1). The team reviews every signature and the storage
  seam.
  Nothing in E3–E9 starts before this point.
- **S2. Red suite in CI** (end of E2). The conformance suite compiles, runs, fails, and is
  all listed as known failures. The system scenario catalogue is written. The seam, clock,
  model and profiler are green. Implementation epics start here.
- **S3. Walking skeleton** (mid E5 / mid E8). A minimal append stream from E5 (one stream,
  no spare pool, no seal) plus E8's `WriteAt` and `ReadAt` on top of E4's index. The first
  conformance tests turn green and leave the known-failure list, and the entries still red
  move from failing in the fixture to failing on a stub (epic 2, D4). **E8's integration
  tests are blocked on this point.** E5 therefore delivers the minimal stream as its first
  subtasks.
- **S4. Core complete** (E3–E9 done). All component conformance tests are green. Only
  system e2e tests remain on the known-failure list. E11 cannot finish before this point.
- **S4a. Engine interface agreed** (with rif, stage 2 of #3119). The filesystem service's
  engine interface is rif's; both sides agree on it before stage 3 starts. E11 is planned
  only once it exists, and its engine wiring, version-limit raise and store contracts
  cannot start before it. S4a is independent of S1–S4 and can be reached at any time
  before E11. It also settles who writes the system scenarios that run at the
  filesystem service's integration tier, and the shape of the `Content` conformance
  check that both a fake `Content` and the engine pass, which is E11's deliverable (epic
  2, §7).
- **S5. Wired** (end of E11). The engine runs on v2 and the RFC 16 store, and the system
  e2e suite, written in E10 and E11, runs against it. **S5 has an external dependency:**
  the RFC 16 metadata store of [#3120](https://github.com/marmos91/dittofs/pull/3120) must
  be on `feat/storage-v2`, because E10's initialisation and E11's engine work both rely
  on it. **The protocol rows have a second one:** a `dfs` serving NFS or SMB on v2 needs
  the filesystem service, `vfs.Filesystem` (#3119 stage 2, RFC 17), and the adapters moved onto
  it, not only E11. The write-verifier row is `vfs.Filesystem` behaviour (RFC 17 §5.8). The
  rows that need no kernel client (several shares per journal, capacity, cold reads,
  lifecycle) run at the filesystem service's integration tier once E11 and stage 2's
  `vfs.New` exist (epic 2, §7). E12 then burns down the known-failure list.

### 3.3 Blockers

- **E1 and E2** block everything. Keep them short and do them together.
- **E3** blocks E5 (segments are made of E3's records) and E7 (recovery parses E3's
  bytes).
- **E4** blocks E7 (rebuild fills the index), E8 and E9. It is pure in-memory code with no
  storage dependency, so it can start at S2.
- **E5** blocks E6, E7 and E8's integration tests.
- **E8** blocks E9: offload needs written content, and `report`, `Release` and `Repack`
  need the data path's locking and capacity.
- **E10** blocks E11's start order.
- **S4a (with rif)** blocks E11: its engine wiring, version-limit raise and store
  contracts cannot start before it.
- **E11** blocks E12's system tests.
- **#3119 stage 2 (external)**, the filesystem service, blocks the integration-tier system
  scenarios; it and the adapters moved onto it block the protocol scenarios.
- **#3120 (external)**, the RFC 16 metadata store, blocks E10's initialisation step, and
  E11's version-limit raise and store contracts.
- **The init command (RFC 16, external)** blocks E10's initialisation step.

### 3.4 Splitting the work

The split follows the RFC's natural divide: **track A owns the bytes on disk, track B owns
the state in memory and the operations on it.** Each track has a run of work with no
cross-track dependency.

- **Track A, storage:** @viquitorreis.
- **Track B, model and operations:** @didimelli.
- **Coordination:** @simonebaracchi keeps the plan and the epic files current and runs the
  reviews at each synchronisation point.

A subtask may move to the other track when one runs ahead; the epic file records its
owner (for example, epic 1's 1.4 is on track B).

| Phase | Track A — storage | Track B — model and operations | Joint |
| --- | --- | --- | --- |
| 0 | E1: skeleton and import test, observability, storage seam | E1: value types, errors, lifecycle, public API (`Journal`, `Handle`, ops) | review → **S1** |
| 1 | E2: test clock and fault schedule, crash-simulating seam, profiler, `raw` | E2: reference model, fixture and `dt` registration, conformance test bodies, traceability matrix | → **S2** |
| 2 | E3, then E5 (minimal stream first) | E4, then E8's storage-independent parts (share registry, capacity counters, version counter) | → **S3** |
| 3 | E6, then E7 | E8 remainder, then E9 | → **S4**; S4a with rif |
| 4 | E10: device config keys, start check, journal side of initialisation | E11 planning, once S4a exists | |
| 5 | E11 | E11 | → **S5** |
| 6 | E12 burn-down of recovery, format and durability failures | E12 burn-down of data-path and reclamation failures | release |

If track B finishes E9 early, it takes E7's inspection and verify subtasks (RFC 1 §4.4),
which read only the index and the segments.

---

## 4. Directory layout

E1 confirms or amends this layout. It is listed here so that every epic knows where its
code lives. Each package is created with its first symbol by the epic named on its line,
never as a placeholder (§1.5, rule 12).

```text
pkg/block/journalv2/              package journalv2 — public API (RFC 1 §3)
  doc.go  journal.go  handle.go  reservation.go
  types.go  errors.go  stats.go  trace.go  inspect.go
  options.go                      injection points; Clock is aliased from internal/core
  imports_test.go                 stdlib only (x/sys in internal/platform), no pkg/block/journal
  rfc_signatures_test.go          RFC 1 calls as written; a drifted signature stops the build
  errors_test.go                  every typed error Is its own sentinel
  internal/format/                RFC 28 §2 codecs, CRC32C, golden vectors       (E3)
  internal/platform/              per-OS sync/alloc/direct I/O/CoW probes        (E3)
  internal/core/                  value types, Clock, Settings, private limits   (E1)
  internal/storage/               the storage seam interface (RFC 1 §1.3)        (E1)
  internal/index/                 placement index, versions, drop ranges         (E4)
  internal/segment/               segments, streams, spares, descriptors, guard  (E5)
  internal/durability/            sync, group commit, failed windows, losses     (E6)
  internal/recovery/              open/rebuild/scan/verify                       (E7)
  internal/reclaim/               repack, retirement                             (E9)
  conformance/                    component e2e: RFC 1 §9.4, RFC 28 §4.2         (E2)
  journalv2test/                  crash seam, reference model, profiler, fixtures (E2)
pkg/sim/                          TestClock, Fault, Schedule; node-wide (RFC 8 §2.1) (E2)
pkg/clock/                        Clock + System, with its first caller (stage-3 composition root, E11)
test/journalv2/known.yaml         known failures, graded by dt (RFC 29 §4.7)     (E2)
test/journalv2/trace.yaml         RFC row → test or owner, checked by trace_test.go (E2)
test/e2e/journalv2_*_test.go      system e2e with kernel clients        (E10 and E11 tests)
```

The package is named `journalv2` while v1 exists. Renaming it once v1 is removed is out of
scope. The suffix stays in Go package and import paths only: metric and span names, log
fields and attribute keys say `journal`. v1 and v2 never share a binary, so no version
label is needed either (epic 1, D1).

---

## 5. The epics in detail

Each paragraph below gives the epic's content, its RFC sources, and candidate subtasks. A
detailed breakdown also copies §1.5, following the instruction there.

### Epic 1 — Interfaces ⛔ (≈9 subtasks, team)

Epic 1 creates the packages of §4 marked E1 and declares, with doc comments and stub
bodies, every symbol and signature of RFC 1 §3–§4 and §7.2. Of the internal interfaces it
declares only the storage seam, which has callers before any implementation (`WithStorage`
and epic 2's crash seam). Format and platform land with E3, and the index, segments,
durability, recovery and reclaim land as concrete types with E4–E9, each with its first
caller. **No behaviour is implemented**, except `Version`'s order
and encoding. It follows §1.5's rules 10–12: only tests that can fail, only symbols RFC 1
§3 uses, and only packages with a first symbol.

Candidate subtasks:

1. Package skeleton and the import test.
2. Value types and the version type: 128-bit, epoch plus counter, epoch held at zero. Its
   byte form is big-endian, epoch then counter, byte for byte the metadata store's
   `entity.JournalVersion`, so that `bytes.Compare` orders as `Compare` does.
3. Error set, with `ErrNoSpace` naming the refusing limit (share, journal, index,
   headroom, version, spare) and the missing amount.
4. `Init`, `Open` and `Restamp`, the options, and the journal's `Clock`. `Open` requires
   a clock.
5. `Journal`, `Handle` and `Reservation` methods.
6. `Offer`, `Change`, `CloneSpec`, `RepackScope` and the snapshot-hold signatures.
7. `Stats`, its per-share breakdown, the counters, `LossEvent`, the `Tracer` and the
   logger option, and the inspection and verify API (RFC 1 §4.4).
8. The storage seam (`storage.FS`, `storage.File`) and `WithStorage`.
9. Joint review and freeze.

### Epic 2 — E2E suite and test foundations ⛔ (17 subtasks, team)

Epic 2 delivers §1.4; its breakdown is
[rfc-1-journal-epic2.md](rfc-1-journal-epic2.md), which supersedes this section where they
differ. The suite must cover **as many use cases as can be named**. The scenario
catalogue below is the minimum. Each family becomes one or more subtasks. Each
test is named after its RFC row where it has one, and every test is listed as a known
failure.

**Foundations:**

- the test clock in `pkg/sim`;
- the crash-simulating seam, with `Fault` and the seeded fault schedule;
- the reference model and the seeded model-based test (RFC 1 §9.1);
- the fixture, and the suite's registration with `dt` and its `known.yaml`;
- the profiler;
- `raw`, RFC 28's bytes written by hand;
- the traceability map and its check.

**Scenario catalogue (component level):**

- **RFC 1 §9.4 Group A, silent data loss** (36 checks):
  - no zero-fill;
  - a write during offload;
  - an offer stopping at `through`, and a lost stamp still offered;
  - fill safety and fill setting the offloaded bit;
  - a read's `asOf` against a change still in flight;
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
- **Group C, recovery** (19 checks):
  - reseed after repack;
  - release across a restart, and a lost release record;
  - fill beats an older write, and a stale fill;
  - incremental report;
  - epoch half must be zero;
  - monotonicity in shuffled segment order;
  - an attached share raises the counter;
  - unclaimed tags are the caller's to name;
  - a cancelled call changes nothing or completes;
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
- **Model-based**: random sequences of write, fill, offload, release, truncate,
  deallocate, delete, settle, repack, crash and reopen, at least two files on two streams
  and two shares on one journal (RFC 1 §9.3).
- **Clock**: a step of the wall clock fires and delays no timer, driven on the test clock
  (the journal's half of RFC 8 §15's "one clock"). The 5%-fast leg is not the journal's:
  it cannot fail on a component with no wait on another clock's timeout, and runs at the
  node in epic 11 (RFC 0 §1.4).

**Scenario catalogue (system level; written as tests in epic 11, the start-check refusals
in epic 10).** Several shares, capacity, cold reads and lifecycle run at the filesystem
service's integration tier, the rest on a real `dfs` with kernel clients; the split and
each tier's dependencies are in the epic's file, §7:

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
- **Initialisation** (epic 10's start check and its share of the initialisation step):
  - each row of the start-check table: normal start, and the two refusals with their
    messages;
  - a refused start writes nothing: the stores, the devices and any v1 state are
    byte-identical afterwards;
  - a crash between journal init and `kvstore.Init`: a start reports the store lost, and
    a re-run of the initialisation step wipes the devices and succeeds;
  - a crash after `kvstore.Init`: the next start completes the `J‖journal` records.
- **Other sources**, kept to what the crash seam cannot cover at component level (it
  already covers fsyncgate-style failed fsyncs, a crash at every sync point and reordered
  writes; the scenarios that need special hosts are parked in §6):
  - `SIGKILL` vs power loss, kept distinct;
  - a long soak with capacity below the data written.

### Epic 3 — On-disk format and platform layer ⛔ (≈12 subtasks, A)

Epic 3 implements RFC 28 §2 and Appendix A as pure, separately testable layers. It also
declares `internal/format` and `internal/platform`, which epic 1 leaves to it (§1.5,
rule 12), and places epic 1's D6 `decision:` marker (`golang.org/x/sys` in
`internal/platform` only) at the platform package.

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
`testdata`. Metrics: the scanner returns the `scan_resyncs` contribution, and epic 7
folds it into `Stats`; `torn_tails` is epic 7's, decided at recovery (epic 3's P8).

**Conformance tests that land here.** These need `internal/format` or `internal/platform`,
so they cannot compile before this epic creates the packages, and they move here from
epic 2 (§1.5, rule 12). Those that need journal behaviour from later epics land listed as
known failures:

- **Platform** (RFC 28 Appendix A), in `conformance/platform`, on epic 2's device
  fixtures:
  - refuse a `nobarrier` mount (ext4 and Btrfs);
  - refuse CoW without `+C`, and a Btrfs directory that is not a subvolume's root;
  - report a thin-provisioned device as a `Warn` log line;
  - `F_FULLFSYNC` on macOS, on the file and the directory, run as a CI matrix leg where
    available;
  - RFC 28 §4.2's directory flush on NTFS, on real storage with a power cut.
- **The storage-agnostic leg:** the conformance tests that need no crash and no fault run
  once more on `platform.OS`, so the seam is never the only storage under test.
- **Planted records:** RFC 1 §9.4 Group C's "a file is its share's", which plants a record
  of a file under a second tag through the encoders.

Candidate subtasks: one per structure (6), scanner, `format` replace, platform per OS, CoW
and barrier probes, and the conformance tests above.

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
  - a record wholly covered by a release, a removal or a synced loss that outranks it, or
    of a forgotten tag, is not held, and recovery neither verifies nor reports its payload
    (RFC 28 §2.3);
  - no second durable index or log of the catalog (RFC 28 §2.1, §2.4);
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
`DamagedSegments`, `corrupt_extents{outcome}`, `stale_extents`, `torn_tails`, and the
scanner's `scan_resyncs` folded into `Stats`. Span: `open`, with child
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
  - `ReadAt`: an exact `missing` list, an untouched buffer, `held` versions, `asOf`; a
    payload that fails its check is never served (RFC 28 §2.3), and the record header a
    read finds is checked against the catalog entry that sent it there (RFC 28 §2.4);
  - `Fill`: the absence check is atomic with the write, it refuses when older than the
    file or its bucket, and it sets the offloaded bit. It is **not** a write path, so it
    has a separate entry point;
  - `asOf`: a write holds its file's append domain from drawing its version until the
    index has it, and a read takes its `asOf` under that domain, no later than it
    resolves its extents (RFC 1 §8.4), so a `Fill` from that read is refused over a
    change it did not see;
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
  - the file append domain: writes to one `FileID` are serialised (RFC 28 §2.2), from the
    version draw until the index has the extent, over epic 5's append streams;
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
  - a removal record whose retention counted a segment is kept until that segment's
    unlink is durable (RFC 28 §2.2);
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

### Epic 10 — Start and initialisation (≈5 subtasks, A)

Epic 10 is the journal's share of starting and initialising an installation, and nothing
else. Everything that needs the engine interface is epic 11's. v1's callers are not ported:
they are deleted with the rest of v1 at the cutover (stage 7 of #3119).

- **Device configuration keys:**
  - a list of journal devices (path, maximum);
  - start refused when `storage` has no device.
- **The start check** and its two refusals (below).
- **The journal side of the initialisation step:** `journalv2.Init` on every device, the
  `J‖journal` records written idempotently, and the wipe of devices left by an
  interrupted step (below).
- **Metadata it relies on**, from RFC 16's store
  ([#3120](https://github.com/marmos91/dittofs/pull/3120)), with no control-plane fields
  of its own:
  - `entity.Installation`, the installation ID;
  - the `J‖journal` record, keyed by journal identity, naming the node and device a
    journal lives on (RFC 16 §4.2). #3120 declares its key kind; whether it writes the
    record is confirmed when E10 starts.
- **System e2e tests:** the start-check refusals, which need no engine. They are written
  here and listed as known failures.

#### No upgrade from v1

Every storage-v2 installation is fresh. There is no migration from v1: existing
installations stay on v1 releases. If there is demand, a migration tool is written later as
its own piece of work, with its own RFC. It would span the metadata store (RFC 6, 7, 16),
the remote tier (RFC 4, 5), GC and backups (RFC 9, 26) and configuration (RFC 13, 15), not
only the journal.

**What RFC 0 fixes.** An installation exists only through an **explicit initialisation
step**, which writes the installation ID into the store's `Installation` record and into
every journal's `format` file. **A start never creates a store** (RFC 0 §1.4).

**The start check** is read-only and runs before anything is opened for writing:

| `Installation` | v1 state | Action |
| --- | --- | --- |
| present | any | normal start |
| absent | present | **refuse**: upgrading from v1 is not supported, and v2 needs a fresh installation |
| absent | absent | **refuse**, naming the init command |

"v1 state" means v1 journal directories, or a v1 metadata or control-plane store. The
check uses it only to choose the message. Without the first row the outcome is always a
refusal, so v1 state can never be read as a fresh install.

**The initialisation step:**

1. `journalv2.Init` on every device, with the installation ID the step mints. Nothing in
   the store is needed for this;
2. `kvstore.Init`, which writes the format record, `Installation` and the share list in one
   transaction. **This is the point of no return.** `kvstore.Open` refuses a store without
   the format record, so nothing else in the store can be written before it;
3. what the store holds besides, the `J‖journal` records included, written idempotently,
   and repeated at every start so that a crash after step 2 leaves no gap.

The step must be re-runnable. A crash between steps 1 and 2 leaves devices whose `format`
names an installation with no store, which a *start* must report as a lost store
(RFC 0 §1.4). Only the initialisation step may therefore wipe and re-initialise them,
since they have accepted no data.

**E10's share:**

- the journal side of the initialisation step: step 1, the journal part of step 3, and
  the wipe of devices left by an interrupted step;
- the start check and its two refusals.

The init command and the order of its store-side steps belong to RFC 16. E10 depends on
them as it depends on #3120.

### Epic 11 — Engine on the RFC 16 store 🔀 (planned after S4a, team)

Stage 3 of [#3119](https://github.com/marmos91/dittofs/issues/3119) is ours. The engine
(journal, carver, syncer, remote) is rebuilt on the RFC 16 store behind the
filesystem-service engine interface. There is no bridge into the v1 engine. Epic 11 is
broken down into subtasks and estimated only once that interface is agreed with rif
(S4a). The scope below is what it must cover; how it splits follows the interface.

- **Engine wiring.** v2 exposes none of v1's `Hydrate`, `Commit` or carving, so the
  engine drives the journal through RFC 1's surface:
  - offload pass → carver and syncer → `report`;
  - cold read → `Fill`;
  - eviction → `Release` + `Repack`;
  - start → reseed `MarkOffloaded`/`Unmark`, `Strand` unclaimed tags, `Forget` removed
    shares, `Since` replay, settle after the metadata commit, in the start order of
    RFC 8 §2.5.
- **Per-device journal registry.** One journal per device opened at start, a `Handle`
  attached per share:
  - adding a share maps to `Share`;
  - removing a share maps to `Close` then `Forget`, in the order RFC 16 §2.3.2 requires;
  - shutdown closes the journals.
- **The remaining configuration:** share-to-device placement, per-share limit and
  `sync.policy`, `ExtentLimit`, `DescriptorLimit`, `headroom_records`, `salvage.path`.
- **Metadata the journal relies on**, from RFC 16's store
  ([#3120](https://github.com/marmos91/dittofs/pull/3120)), with no control-plane fields
  of its own:
  - `Share.Tag`, minted from the installation's tag counter and never reused;
  - `VersionLimit` and `RaiseVersionLimit`, the durable per-share version limit
    (`S‖id‖vl`);
  - `VersionFloor`, **derived** from the version index, never stored: a stored floor could
    lag a file's `Version` and let a version be issued twice.
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
- **Store contracts the engine relies on:**
  - the offload `report` carries the extents `Content.Commit` returned, and is called only
    after that commit; the block's put intent is recorded before the put;
  - `Settle` is called with the file's `Applied` from the store, not with the version
    submitted, because `Existence.Commit` may split a file over several transactions or
    fail one file of a batch.
- **Error mapping:** `ErrNoSpace{limit}` maps to retry-later or no-space following
  RFC 8 §10.2, and `ENOSPC` stays distinct. `ErrOtherShare` maps to `ErrCorrupt`, never
  retried. It covers both a file owned by another share and a file recovery rebuilt under
  two tags. Loss generation is folded into the NFS write verifier.
- **Salvage of stranded tags after serving.** Exports go under `salvage.path`, mode 0700,
  sealed with a salvage data key for encrypting shares, and a failed export becomes a
  health condition.
- **Exporters:**
  - `Stats` → Prometheus collectors labelled `journal` (and `share`). Metric names carry
    no version suffix (epic 1, D1);
  - the `Tracer` → OpenTelemetry spans, and the logger → the server's `slog` logger. Span
    names and attribute keys carry no version suffix either;
  - three alert rules for RFC 1's named alerts: any `data_loss`, a growing `OldestDirty`,
    and `RemovalMarkers` growing.
- **API and CLI:** API and `dfsctl` show journal stats per device and per share.
  `go run ./cmd/gendocs` is rerun afterwards.
- **System e2e tests.** E11 writes the system scenarios catalogued in epic 2, except the
  start-check refusals of E10, lists them as known failures, and turns green those its
  own wiring satisfies. The integration-tier rows need #3119 stage 2's `vfs.New`, and
  the protocol rows the adapters moved onto it (S5).
- **The `Content` conformance check:** one check that both a fake `Content` and the engine
  pass, so the filesystem service's unit tier tests against the contract the engine
  meets. Its shape is agreed at S4a.

### Epic 12 — E2E green 🔀 (≈8 subtasks, team)

Epic 12 takes the suite from "listed" to "passing". The work:

- triage every remaining known failure into "fix" or "the test is wrong" (the latter
  decided by the team and recorded);
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

---

## 6. Risks and open points

- **Stage 3 of #3119 is ours.** The engine (journal, carver, syncer, remote) is rebuilt on
  the RFC 16 store behind the filesystem-service engine interface (E11). There is no
  bridge into the v1 engine.
- **E11 waits on S4a.** It is not estimated, and is planned only once the engine interface
  is agreed with rif (stage 2 of #3119).
- **The system scenarios wait on stage 2 too.** Under route B, NFS and SMB reach v2 only
  through `vfs.Filesystem` and the adapters moved onto it, so E11 alone turns no protocol
  scenario green (S5).
- **E10 and E11 depend on #3120.** The metadata RFC 1 assumes (installation ID, share tags,
  the durable version limit, the derived version floor, `Applied`) comes from RFC 16's
  store, which [#3120](https://github.com/marmos91/dittofs/pull/3120) implements. Neither
  can finish before it lands on `feat/storage-v2` (S5).
- **There is no upgrade from v1.** Storage v2 serves new installations only. A migration
  tool, if demand calls for one, needs its own RFC (E10, "No upgrade from v1").
- **Scenarios that need a host we don't have.** `errors=remount-ro` on ext4, XFS
  speculative preallocation (accounting follows the cap) and disk hot-removal mid-write
  need special hosts, and nothing in this plan provisions them. They stay out of the
  system e2e suite until one exists.
- **Segments vs staging files** (RFC 28 §2.6) is gated by benchmark J9. This plan assumes
  segments. A J9 result that overturns that reopens epics 3, 5 and 7.
- **Cluster-only items** (RFC 10 replication, a non-zero epoch, the `generation` field,
  extension kinds 128+) are out of scope. The format reserves them, and the tests assert
  that they are refused.
- **Salvage** depends on RFC 5's master key and RFC 26's recovery import. Where those are
  missing, epic 11 implements the export path and its failure condition, and records what
  is stubbed.
- **Proposed constants** (256 MiB segment cap, 1 s sync bound, 64/256 pin bounds,
  65,536 headroom records, 4,096 floor buckets, 30 s / 16 MiB idle seal) are implemented
  as named constants with their RFC `decision:` notes, to be settled by measurement in
  epic 12.
