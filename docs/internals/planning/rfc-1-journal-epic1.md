# Epic 1: journal v2 interfaces

**Parent plan:** [rfc-1-journal-plan.md](rfc-1-journal-plan.md), epic 1.
**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §1, §3, §4, §6 and §7.2;
[RFC 28](../rfcs/rfc-28-journal-format.md) §2 and Appendix A;
[RFC 8 §2.1](../rfcs/rfc-8-engine.md) for the shared clock and fault interfaces.
**Owners:** A and B, with the split given per subtask.
**Estimate:** 10 subtasks.
**Blocks:** every other epic. The end of this epic is sync point **S1, interfaces
frozen**.

---

## 1. Goal

This epic declares **only interfaces**:

- the directory tree;
- every exported symbol and its signature;
- the internal seams between epics 3–9;
- how dependencies are injected.

It implements no behaviour. Every function body is a stub. At the end of the epic:

- `go build ./...` and `go vet ./...` pass;
- epic 2 can write its whole test suite against these signatures, and the tests compile
  and fail on their assertions;
- epics 3–9 can each start on their own side of a seam without waiting for another
  epic's implementation.

### 1.1 In scope

- the `pkg/block/journalv2` public package, with every symbol of RFC 1 §3, §4.3, §4.4 and
  §7.2;
- the value types, the error set and the observability contract (`Stats`, `Hooks`);
- the internal seam interfaces: storage, platform, format, index, segment, durability,
  recovery, reclaim;
- the shared clock and fault interfaces of RFC 8 §2.1, in `pkg/sim`;
- empty skeletons for the test packages epic 2 fills (`journalv2test`, `conformance`);
- the guard tests that keep the surface honest: the import rule, an API golden file, and
  a no-panic surface test.

### 1.2 Out of scope

- Any behaviour. That includes trivial behaviour such as `Version.Compare`, which epic 4
  implements.
- The test components themselves: the crash seam, the reference model, the
  known-failure list and the profiler all belong to epic 2.
- Wiring v2 into DittoFS, configuration keys and exporters. Those are epic 10.
- Cluster-only surface (RFC 10): no extension kinds, no non-zero epoch, no
  `ErrForgotten`.

---

## 2. General rules for each epic

> **Instruction for whoever expands an epic into its detailed description and subtasks,
> human or AI:** copy this section **verbatim** into that description. Every subtask must
> state how it meets each rule that applies to it. A rule that does not apply is named
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
   allocations-per-op assertions and the counted checks of RFC 1 §9.7.
5. **Public interface and internals.** Tests cover the epic's internal package API and the
   public v2 interface behaviour it enables, including concurrency under `-race`. Per the
   project rules, test at the layer that **consumes** a value, not the one that produces
   it. A regression test is validated by reverting the fix and watching it fail **on its
   assertion**.
6. **Known failures shrink.** Every e2e test an epic turns green is removed from the
   known-failure list in the same change. The runner fails if a listed test passes.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on
   `pkg/block/journal`. The import test enforces it. v1 is touched only in epic 10, and
   only at its callers.
8. **Real storage for storage properties.** No in-memory filesystem stands in for a check
   about durability, torn tails, unlink durability or corruption (RFC 1 §9.5). In-memory
   stand-ins are allowed only for pure logic (index, codecs).
9. **Code comment conventions** (`CLAUDE.md`). No epic, issue or plan IDs in source
   comments. Use `ponytail:` for knowingly-simple implementations and `decision:` for
   deliberate behaviour. Where the RFC already carries a ponytail or decision note, its
   marker goes at the code site that implements it.

### 2.1 How the rules apply to this epic

| Rule | Applies to epic 1 as |
| --- | --- |
| 1. Unit tests | **Applies.** The guard tests are this epic's unit tests: the import rule, the API golden file, and the no-panic surface test. |
| 2. Fuzz | **Does not apply.** No parser is implemented here. The fuzz targets belong to epic 3 (codecs) and epic 4 (operation sequences). |
| 3. Metrics and tracing | **Applies as declarations.** Every field of RFC 1 §3.7 and every counter of §3.9 is declared in `Stats`. The `Hooks` contract and its span and event names are fixed. Nothing is maintained yet. |
| 4. Profiling | **Does not apply.** There are no hot paths yet, and the profiler is epic 2's. |
| 5. Public and internals | **Applies.** The guard tests cover the public package and every internal seam package. |
| 6. Known failures | **Does not apply.** No e2e test exists yet. The list is created in epic 2. |
| 7. v2 ignores v1 | **Applies.** The import test forbids `pkg/block/journal` in every v2 package, test files included. |
| 8. Real storage | **Does not apply.** Nothing touches storage. |
| 9. Comments | **Applies.** Doc comments state contracts in terms of the code, with no RFC or epic numbers. The symbol-to-RFC map is in §6 of this file. Decisions taken here carry `decision:` markers at their declaration. |

---

## 3. Decisions this epic takes

Each decision below is recorded at its code site with a `decision:` marker and is
reviewed at the freeze (subtask 1.10).

**D1. Package location and name: `pkg/block/journalv2`.**
v1 keeps `pkg/block/journal` until epic 10 retires it. Renaming the package afterwards is
out of scope.

**D2. Value types live in a leaf package, `internal/core`, and the public package
re-exports them as type aliases.**
For example, `type Version = core.Version`.

- *Why:* the internal packages (format, index, segment, …) need `Version`, `FileID` and
  `SegmentID`. The public package imports those internal packages. If the types lived in
  the public package, the internal packages would have to import it, and that is an
  import cycle.
- *What it costs:* godoc shows the types as defined in `internal/core`.

**D3. The journal declares its own `Clock` interface, using standard-library types only:**

```go
type Clock interface {
    Now() time.Time                       // wall time, for mtimes and OldestDirty
    Elapsed() time.Duration               // monotonic reading since an arbitrary origin
    After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}
```

- *Why not use RFC 8's `Clock` directly:* RFC 1 §1.1 bars the journal from importing any
  module package, `pkg/sim` included.
- *Why an adapter rather than structural typing:* RFC 8's `Clock` uses a named `Mono` type,
  so Go's structural typing cannot make one interface satisfy the other. An adapter is
  needed, `journalv2test.Clock(sim.Clock) journalv2.Clock`. Epic 2 implements it for tests
  and epic 10 for production.
- *What would overturn it:* RFC 8 changing its `Clock` to standard-library types only.

**D4. The journal takes no `Fault` interface.**
Faults are injected by the storage seam implementation, which epic 2 writes in
`journalv2test` and which consults `sim.Fault`. The journal only ever sees
`storage.FS`.

**D5. Dependencies are injected through variadic options appended to RFC 1's
signatures.**
For example, `Open(dir, floor, expect, opts ...Option)`. A call written exactly as RFC 1
writes it still compiles. The options are:

- `WithClock`;
- `WithStorage(storage.FS)`. Its parameter type is internal, so only code under
  `pkg/block/journalv2/` can build a value to pass;
- `WithHooks`;
- `WithSettings`;
- `WithDescriptorBudget`.

**D6. `golang.org/x/sys` is allowed in `internal/platform` only.**
Everywhere else the rule is standard library only.

- *Why:* `fdatasync`, `fallocate`, `F_FULLFSYNC` and `O_DIRECT` are the "operating
  system's file interface" RFC 1 §1.1 allows. The `syscall` package covers them
  incompletely and is frozen.
- *How it is held:* the import test enforces the split per package.

**D7. Spans take no request context.**
RFC 1's operations carry no `context.Context`, so journal spans are roots. They are
correlated through attributes (journal, share tag, file).

- *What would overturn it:* a need to parent journal spans under protocol request spans.
  That would mean adding context-taking variants, not changing RFC signatures.

**D8. Stubs never panic.**
Every stub returns the zero value with `ErrNotImplemented`, or an empty iterator. A
`Settle`-style method with no result does nothing.

- *Why:* a panic aborts the test binary, and every later test in it then reports as not
  run, not as failing on its assertion. Epic 2 needs every test to fail on its own
  assertion.

**D9. `FileID` is an opaque struct built only by `NewFileID(id [16]byte)`.**
RFC 0 §3 requires a type that cannot be reached by conversion.

- *Limit:* the journal cannot import the metadata store's ID type. The constructor
  therefore takes the UUID bytes, and epic 10 calls it in exactly one place, from the
  metadata file ID.
- *Ceiling:* any 16 bytes are accepted. Recorded with a `decision:` marker.

**D10. Proposed RFC constants are named constants. Settings are fields.**

- Constants: segment cap 256 MiB, sync bound 1 s, pins 64 per offer and 256 per journal,
  4,096 floor buckets, idle-seal age 30 s.
- Settings: maximum bytes, append streams, `ExtentLimit`, `HeadroomRecords`,
  `IdleSealBytes`, spare-pool target, descriptor limit.
- `Settings.SegmentCap` exists for tests only. Zero means the constant. This is a
  `decision:` marker, because the RFC fixes the cap as a constant, and the tests need a
  journal of 1,000 segments to fit a temporary directory.

**D11. `SyncPolicy`'s zero value is `Bounded`**, which RFC 1 §5.2 names as the default.

**D12. The descriptor budget is a value shared across journals:
`NewDescriptorBudget(limit int) *DescriptorBudget`.**
RFC 1 §7.4 makes the limit per process, while each journal is built separately. A
journal built without a budget gets a private one at the default limit.

---

## 4. Target tree

```text
pkg/sim/
  doc.go
  clock.go          Clock, TestClock, Mono (RFC 8 §2.1, verbatim)
  fault.go          Fault, FaultAction
pkg/block/journalv2/
  doc.go            package contract: what the journal is and is not
  journal.go        Journal; Init, Open, Restamp; Close; Share, Forget, Strand; Draw, SetVersionLimit
  handle.go         Handle and its operations
  reservation.go    Reservation
  offload.go        Offer; Offload, OffloadMany, MarkOffloaded, Unmark, Release
  removal.go        Truncate, Deallocate, Delete, CloneTarget, CloneSpec, Since, Settle, Change, ChangeKind
  holds.go          Hold, Unhold, Held, Stamp
  repack.go         Repack, RepackScope
  types.go          aliases to internal/core
  errors.go         error set
  stats.go          Stats, ShareStats, Counters, LossEvent, Histogram, RecoveryStats, SyncFailure
  hooks.go          Hooks, SpanOp, EventKind, Event
  inspect.go        Inspector, IndexEntry, SegmentInfo, VerifyReport
  options.go        Option, With*, Settings, DescriptorBudget, Clock, constants
  imports_test.go   import rule for every package under journalv2
  api_test.go       API golden check
  surface_test.go   no-panic surface test
  testdata/api.golden
  internal/core/        value types
  internal/storage/     FS, File
  internal/platform/    OS storage.FS, Probe
  internal/format/      sizes, kinds, structures, codecs, CRC, Scanner
  internal/index/       Index, FileIndex
  internal/segment/     Store, Stream, Guard, Accounting
  internal/durability/  Coordinator, Window
  internal/recovery/    Rebuild, Sink, Verify
  internal/reclaim/     Repacker
  journalv2test/doc.go  empty skeleton for epic 2, plus the Clock adapter signature
  conformance/doc.go    empty skeleton for epic 2
```

---

## 5. Subtasks

Each subtask lists:

- **Owner**, and **Depends on**: the subtasks that must land first;
- **Deliverables**: the files and the symbols they must declare;
- **Done when**: the acceptance criteria;
- **Rules**: how each applicable general rule is met. A rule the subtask does not mention
  falls under §2.1's "does not apply" for this epic.

Every symbol carries a doc comment stating its contract precisely enough for epic 2 to
write a test from it. That contract covers the preconditions, the guarantee on success,
each error and what it means, and the concurrency guarantees. The comment states the
rule, not where the rule came from.

### 1.1 Package skeleton and guard tests

- **Owner:** A. **Depends on:** nothing.
- **Deliverables:**
  - the tree of §4, with a `doc.go` in every package;
  - `imports_test.go`, covering every package under `pkg/block/journalv2/`. It is modelled
    on v1's `foreign_imports_test.go`: it reads `go/build`, parses files built for other
    platforms, and checks test imports too. It allows:
    - the standard library and the module's own `pkg/block/journalv2/...` packages,
      everywhere;
    - `golang.org/x/sys`, in `internal/platform` only (D6);
    - `pkg/sim`, in `journalv2test` only.

    It forbids `pkg/block/journal` everywhere. The internal packages of the conformance
    and test packages count as part of the tree;
  - `api_test.go`. It renders every exported declaration of the public package and of
    every internal seam package with `go/doc` and `go/printer`, then compares the result
    with `testdata/api.golden`. Running with `-update` rewrites the golden file;
  - `surface_test.go`, with an empty table that later subtasks fill.
- **Done when:**
  - the import test fails on a planted `pkg/block/journal` import, on a planted
    third-party import in `internal/index`, and on a planted x/sys import in `internal/format`.
    Each is checked by reverting the plant after watching the failure;
  - `go build ./...` passes.
- **Rules:**
  - 1 and 5: the guard tests are this subtask's tests.
  - 7: v1 is forbidden by test.
  - 9: package docs describe behaviour.

### 1.2 Value types

- **Owner:** B. **Depends on:** 1.1.
- **Deliverables:** in `internal/core`, aliased in `types.go`:

  | Type | Definition |
  | --- | --- |
  | `Version` | `struct{ Epoch, Counter uint64 }`, with stubs for `Compare(Version) int`, `Less`, `IsZero`, `Bytes() [16]byte` and `VersionFromBytes([16]byte) Version`. The byte order is "epoch half then counter", matching how metadata stores a version. |
  | `FileID` | opaque struct, `NewFileID([16]byte) FileID`, `(FileID) Bytes() [16]byte` (D9) |
  | `Extent` | `struct{ Off, Len int64 }`, with stubs for `End()` and `Overlaps` |
  | `Held` | `struct{ Extent Extent; Version Version }` |
  | `ShareTag` | `uint32` |
  | `ShareID` | `[16]byte` |
  | `SnapshotCut` | `uint64` |
  | `SyncPolicy` | `uint8`, with `Bounded` (the zero value, D11) and `Standard` |
  | `JournalID`, `InstallationID` | `[16]byte` |
  | `SegmentID`, `Seq` | `uint64` |

- **Done when:**
  - every type is in the golden file;
  - the doc comments state the invariants. For example: `FileID` encodes no share; a
    version's epoch is zero in this format; a version is compared epoch first, as one
    unsigned 128-bit number.
- **Rules:**
  - 9: the `decision:` markers for D9 and D11 sit at their declarations.

### 1.3 Errors and the refusal taxonomy

- **Owner:** B. **Depends on:** 1.2.
- **Deliverables:** in `errors.go`:
  - **Sentinels:**

    | Sentinel | Returned when |
    | --- | --- |
    | `ErrNotImplemented` | a stub is called |
    | `ErrClosed` | a call reaches a closed journal or handle |
    | `ErrHandleOpen` | `Forget` is called while a handle of the tag is open |
    | `ErrJournalExists` | `Init` finds a `format` file or a segment |
    | `ErrNoJournal` | `Open` or `Restamp` finds no journal, and never creates one |
    | `ErrIdentityMismatch` | the identity on disk is not `expect` |
    | `ErrUnknownFormat` | the format version, a record kind or a non-zero epoch is not recognised |
    | `ErrStaleFill` | a `Fill` is older than the file |
    | `ErrNotOffloaded` | a `Release` names an extent whose offloaded bit is unset |
    | `ErrNotOffered` | a `report` names an extent the offer did not contain |
    | `ErrOfferEnded` | `report` is called after `fn` returned |
    | `ErrZeroLength` | a zero-length `Deallocate` or `CloneTarget` |
    | `ErrSyncFailed` | a sync window failed |

  - **Typed errors:**
    - `type Limit uint8`, with `LimitShare`, `LimitJournal`, `LimitIndex`,
      `LimitHeadroom`, `LimitVersion` and `LimitSpare`, plus `String()`.
    - `type NoSpaceError struct{ Limit Limit; Missing int64; Unit Unit }`. `Unit` is
      bytes, entries, versions or segments. It matches `ErrNoSpace` under `errors.Is`.
    - `IdentityError`, carrying the identity and installation ID found and whether
      segments are present, for the `expect == 0` diagnosis.
    - `SyncError`, naming the window, the files it failed, and the cause.
    - `NotOffloadedError`, listing the refused extents.
  - **The `ENOSPC` rule:** a full filesystem surfaces as the platform's
    `*fs.PathError` wrapping `syscall.ENOSPC`. It is **never** a `NoSpaceError`, so
    `errors.Is(err, ErrNoSpace)` and `errors.Is(err, syscall.ENOSPC)` are never both true
    for one error.
- **Done when:**
  - each error's comment states which calls return it and whether the caller should
    treat it as retry-later;
  - the surface test checks the `Is` and `As` relations. This is the one place where real
    behaviour is implemented, because the relations are part of the contract.
- **Rules:**
  - 1 and 5: the `Is`/`As` table test.

### 1.4 Lifecycle, injection, clock and fault interfaces

- **Owner:** A. **Depends on:** 1.2.
- **Deliverables:**
  - `journal.go`:
    - `Init(dir string, install InstallationID, opts ...Option) (JournalID, error)`;
    - `Open(dir string, floor Version, expect JournalID, opts ...Option) (*Journal, error)`;
    - `Restamp(dir string, expect JournalID, install InstallationID, opts ...Option) error`;
    - `(*Journal) Close() error`, documented with the shutdown contract of RFC 1 §8.7:
      new operations are refused, in-flight ones finish, and background work is joined
      before any segment closes.
  - `options.go`:
    - `Option`, and `WithClock(Clock)`, `WithStorage(storage.FS)`, `WithHooks(Hooks)`,
      `WithSettings(Settings)`, `WithDescriptorBudget(*DescriptorBudget)`;
    - `Settings{MaxBytes int64; Streams int; ExtentLimit int64; HeadroomRecords int;
      IdleSealBytes int64; SpareTarget int; SegmentCap int64}`, with a documented default
      per field;
    - the D10 constants;
    - `Clock` (D3);
    - `NewDescriptorBudget` (D12).
  - `pkg/sim/clock.go` and `pkg/sim/fault.go`: `Clock`, `TestClock`, `Mono`, `Fault` and
    `FaultAction`, copied verbatim from RFC 8 §2.1. `FaultAction` is a struct whose kind
    is one of pass, fail, delay, drop, tear-after-n or reorder, with the payload that kind
    needs.
  - `journalv2test/clock.go`: the adapter signature, `func Clock(c sim.Clock)
    journalv2.Clock`, stubbed. Epic 2 implements it.
- **Done when:**
  - a call written exactly as RFC 1 writes it compiles. Checked with a compile-only test
    file;
  - every setting documents its unit, default and refusal behaviour.
- **Rules:**
  - 3: `WithHooks` is the only observability entry point.
  - 9: `decision:` markers for D3, D5, D6, D10 and D12.

### 1.5 Share handles, the data path and capacity API

- **Owner:** B. **Depends on:** 1.3, 1.4.
- **Deliverables:** with the signatures exactly as RFC 1 gives them:

  - **`(*Journal)` methods:**
    - `Share(tag ShareTag, limit int64, floor Version, policy SyncPolicy) (*Handle, error)`
    - `Forget(tag ShareTag) error`
    - `Strand(tag ShareTag) error`
    - `Draw() (Version, error)`
    - `SetVersionLimit(mark Version)`
  - **`(*Handle)` methods:**
    - `Close() error`
    - `SetSyncPolicy(SyncPolicy) error`
    - `Reserve(n int64, writes int) (*Reservation, error)`
    - `WriteAt(id FileID, off int64, p []byte, mtime int64) (Version, error)`
    - `Sync(ids ...FileID) error`
    - `ReadAt(id FileID, off int64, p []byte) (n int, missing []Extent, held []Held, asOf Version, err error)`
    - `Fill(id FileID, off int64, p []byte, asOf Version, v Version) error`
    - `Truncate(id FileID, size int64) (Version, error)`
    - `Deallocate(id FileID, e Extent) (Version, error)`
    - `Delete(id FileID) (Version, error)`
    - `CloneTarget(id FileID, spec CloneSpec) (Version, error)`
    - `Since(id FileID, from Version) iter.Seq2[Change, error]`
    - `Settle(id FileID, v Version)`
    - `Extents(id FileID) ([]Extent, error)`
    - `Files() iter.Seq[FileID]`
    - `DirtyFiles() iter.Seq2[FileID, time.Time]`
    - `Stats() Stats`
  - **`(*Reservation)` methods:**
    - `WriteAt(id FileID, off int64, p []byte, mtime int64) (Version, error)`
    - `Close() error`
  - **Types:**
    - `CloneSpec` with its six fields;
    - `Change{Kind ChangeKind; Extent Extent; Version Version; Mtime int64; Clone *CloneSpec}`;
    - `ChangeKind`, with `ChangeWrite`, `ChangeTruncate`, `ChangeDeallocate`,
      `ChangeDelete`, `ChangeCloneTarget` and `ChangeLoss`.
- **Done when:** every doc comment states:
  - the refusal set of the call (which `Limit` values can refuse it);
  - whether it draws a version;
  - whether it draws on the headroom;
  - whether it is durable on return;
  - which lock domain it takes (§8.2).

  `Fill`'s comment states that it is not a write path, and the two methods stay separate,
  as RFC 1 requires.
- **Rules:**
  - 5: every method is in the surface test (1.10).

### 1.6 Offload, release, repack and holds API

- **Owner:** B. **Depends on:** 1.5.
- **Deliverables:**
  - **`(*Handle)` methods:**
    - `Offload(id FileID, limit, widen int64, fn func(o Offer, report func(offloaded []Extent) error) error) error`
    - `OffloadMany(ids []FileID, limit, widen int64, fn func(offers []Offer, report func(id FileID, offloaded []Extent) error) error) error`
    - `MarkOffloaded(id FileID, extents []Extent, oldest, newest Version) error`
    - `Unmark(id FileID, extents []Extent) error`
    - `Release(id FileID, extents []Extent) error`
    - `Hold(share ShareID, cut SnapshotCut, marks map[FileID]Version) error`
    - `Unhold(share ShareID, cut SnapshotCut) error`
    - `Held(share ShareID) int64`
    - `Stamp(id FileID, through Version, cut SnapshotCut) error`
  - **`(*Journal)` method:** `Repack(budget int64, scope RepackScope) (freed int64, error)`.
  - **Types:**
    - `Offer{ID FileID; Dirty, Neighbours []Extent; Superseded []Held; Oldest, Newest Version; Offered io.ReaderAt}`;
    - `RepackScope{Share ShareTag; Segments []SegmentID}`.
- **Done when:** the doc comments state:
  - the frozen-view guarantee and its lifetime (`Offered` fails outside the offered extents
    and after `fn` returns);
  - that `report` marks immediately and costs O(k log n);
  - that partial success is honoured;
  - that `Release` is all-or-nothing;
  - that `Repack`'s `freed` is reported only once durable;
  - that no lock is held across `fn`.
- **Rules:**
  - 3: the span names `offload_pass` and `repack_pass` are referenced from `hooks.go`.

### 1.7 Observability and inspection contract

- **Owner:** A. **Depends on:** 1.2.
- **Deliverables:**
  - **`stats.go`:** `Stats` with every field of RFC 1 §3.7, under the same names and
    kinds: `MaxBytes`, `UsedBytes`, `ReservedBytes`, `HeldBytes`, `DirtyBytes`,
    `OldestDirty`, `Files`, `ExtentCount`, `DropRanges`, `Segments`, `Sealed`,
    `UnreclaimedBytes`, `HeadroomBytes`, `RepackReserveBytes`, `PinnedSegments`,
    `OpenDescriptors`, `DescriptorLimit`, `DamagedSegments`, `LastSyncError`,
    `SyncDuration`, `RemovalMarkers`, `VersionLimit`, `VersionsLeft`, `NextVersion`,
    `ExtentLimit`, `Counters`, `RejectedSegments`, `Recovery`, `Losses`,
    `LossGeneration`, and `Shares map[ShareTag]ShareStats`. Supporting types:
    - `ShareStats`;
    - `Counters`, with every §3.9 counter: `WrittenBytes`, `FilledBytes`, `FillRefused`,
      `OfferedBytes`, `MarkedBytes`, `ReleasedBytes`, `FreedBytes`, `RepackedBytes`,
      `ReservationRefused`, `Syncs`, `SyncFailures`, `SyncReappendedBytes`,
      `HeadroomDraws`, `CorruptExtentsDataLoss`, `CorruptExtentsRepairable`,
      `StaleExtents`, `TornTails`, `ScanResyncs`, `IndexRefusals`, `SegmentReopens`,
      `SpareRefusals`, plus `DoubleRetires` (RFC 1 §7.3);
    - `Histogram`, with fixed buckets and standard-library types only;
    - `RecoveryStats`, `SyncFailure`, `LossEvent` and `LossReason`.

    `Stats`'s doc comment states the three guaranteed inequalities and that `Stats` is not
    a consistent snapshot.
  - **`hooks.go`:**
    - `Hooks{Span func(op SpanOp, attrs ...slog.Attr) (end func(err error)); Event func(Event); Logger *slog.Logger}`;
    - the `SpanOp` constants: `open`, `recovery_phase`, `sync`, `offload_pass`,
      `repack_pass`, `fill`, `reserve_refused`, `seal`, `retire`;
    - the `EventKind` constants: `loss`, `double_retire`, `sync_failed`, `window_failed`,
      `thin_provisioned`, `foreign_segment`, `damaged_segment`.

    The contract: a nil field is a no-op. A hook is called with no journal lock held, must
    not block, and must not call back into the journal.
  - **`inspect.go`:**
    - `(*Journal) Inspect() Inspector`;
    - `Inspector`, with `File(id) ([]IndexEntry, error)`,
      `All() iter.Seq2[FileID, []IndexEntry]`, `Segments() []SegmentInfo` and
      `Verify() (VerifyReport, error)`;
    - `IndexEntry`, which says whether the entry is held, retained or a marker, and
      carries the extent, version, location and offloaded bit;
    - `SegmentInfo{ID; Stream; Sealed; Failed; Allocated; HeldBytes; RetainedBytes; Damaged}`;
    - `VerifyReport{Unsupported []IndexEntry; Unreferenced []RecordRef; BadFooters []SegmentID; FootprintMismatch *int64}`.

    The doc comment states that verification reports and never repairs.
- **Done when:**
  - every row of the RFC 1 §3.7 and §3.9 tables maps to exactly one field. The map is
    filled into §6 of this file;
  - epic 12 can name its metric set from these fields alone.
- **Rules:**
  - 3: this subtask *is* the metrics and tracing contract.
  - 9: the D7 `decision:` marker sits on `Hooks`.

### 1.8 Storage, platform and format seams

- **Owner:** A. **Depends on:** 1.2.
- **Deliverables:**
  - **`internal/storage`:** the narrow seam of RFC 1 §1.3.
    - `FS`, with `Create(name) (File, error)`, which fails if the file exists;
      `Open(name) (File, error)`; `Rename`; `Remove`; `List() ([]string, error)`;
      and `SyncDir() error`.
    - `File`, with `ReadAt`; `ReadAtDirect`, which bypasses the page cache; `WriteAt`;
      `Allocate(off, n int64)`; `Sync()` (a data-only sync, or a full sync where the
      platform requires one); `Truncate`; `Size`; `Close`; and `Name`.

    Every call is one that a crash-simulating implementation can record and discard. The
    doc comment says so.
  - **`internal/platform`:**
    - `OS(dir string) (storage.FS, error)`;
    - `Probe(dir string) (Report, error)`, where
      `Report{Barriers bool; CopyOnWrite bool; NoCoWSet bool; BtrfsSubvolumeRoot bool; ThinProvisioned bool}`;
    - `Check(Report) error`, which gives the refusals of RFC 28 Appendix A.
  - **`internal/format`:**
    - size constants: 56, 48, 32, 96, 72 and 32 bytes, and 8-byte alignment;
    - magics: `DJF1`, `DJH1`, `DJS1`, `DJR1` and `DJT1`;
    - `Kind`, with constants 1–14 and the reserved ranges;
    - structs `FormatFile`, `SegmentHeader`, `SealMarker`, `RecordHeader`,
      `CatalogEntry` and `Trailer`, with fields exactly as RFC 28 §2;
    - the per-kind payload structs: write mtime, loss, clone-target spec and cut;
    - `Encode*` and `Decode*` functions, each taking the seed explicitly;
    - `Checksum(seed *core.JournalID, parts ...[]byte) uint32`;
    - `Scanner`, built with `NewScanner(r io.ReaderAt, size int64, seed core.JournalID)`,
      whose `Next() (Slot, error)` returns
      `Slot{Kind SlotKind; Offset int64; Header RecordHeader; Seal SealMarker}`. `SlotKind`
      is record, seal, zero or damage.
- **Done when:**
  - field order and widths in the doc comments match RFC 28 byte for byte, so epic 3 can
    write golden vectors from the comments alone;
  - every decode function documents the error each malformed input yields.
- **Rules:**
  - 2: the decoders' contract (accept or reject, never panic) is written into their doc
    comments, ready for epic 3's fuzz targets.
  - 7 and 8: the D6 marker sits at the platform package.

### 1.9 Internal seams: index, segment, durability, recovery, reclaim

- **Owner:** B for `internal/index`, A for the rest. **Depends on:** 1.8.
- **Deliverables:** one interface per package, plus a `New…` constructor stub. Each
  method is listed with the query or bound from RFC 1 §4.1 that it must meet:

  - **`internal/index` (implemented by E4; consumed by E7, E8, E9):**
    - `Index`, with `File(id) FileIndex`, `Drop(id)`, `Counters() *Counters`, the
      `Buckets` floor table, `ExtentCount()`, `SegmentLive(seg) (held, retained int64)`
      and `FileSegments(id) iter.Seq[SegmentRanges]`;
    - `FileIndex`, with `Lookup(off)`, `Span(off, n) (held []Entry, gaps []Extent)`,
      `Apply(rec Placement) (superseded []Entry)`, `Retain`, `Unretain`, `Mark(ext, lo, hi)`,
      `Unmark`, `Markers()`, `Settle(v)`, `All()` and `ChangeVersion()`;
    - `Counters`, with `NextSeq`, `NextVersion`, `SetLimit`, `Raise(floor)` and `Left()`;
    - `Entry{Extent; Version; Seq; Loc; Offloaded; Kind}`, and `Loc{Segment; Offset; Length}`.
  - **`internal/segment` (E5; consumed by E6, E7, E8, E9):**
    - `Store`, with `Stream(n) Stream`, `Read(loc, p) error` (taking the guard shared),
      `ReadVerified(loc) ([]byte, error)`, `Retire(seg, recycle bool) (freed int64, error)`
      (idempotent), `Guard(seg) Guard`, `Accounting() *Accounting`, `Segments()` and
      `TakeSpare(class SpareClass)`;
    - `Stream`, with `Append(hdr format.RecordHeader, payload []byte) (Loc, error)`,
      `Roll()`, `Seal()`, `Active() SegmentID` and `Failed() bool`;
    - `Guard`, with `RLock`, `RUnlock` and `Exclusive(func())`;
    - `SpareClass`: content, headroom or repack.
  - **`internal/durability` (E6; consumed by E8, E9):**
    - `Coordinator`, with `Track(file, loc, rec)`, `Sync(ids...) error`,
      `SetPolicy(tag, p)`, `Window(seg) *Window` and `LossGeneration() uint64`;
    - `Window`, with `Reappend() error` and `Fail() []LossRecord`;
    - `LossLog`, which is the ring.
  - **`internal/recovery` (E7; consumed by `journalv2.Open`):**
    - `Rebuild(fs storage.FS, seed core.JournalID, floor core.Version, sink Sink) (Result, error)`;
    - `Sink`, the callbacks through which records land in index and segment state;
    - `Result{BySegmentCatalog, ByScan int; Duration time.Duration; Rejected, Damaged []core.SegmentID; TornTails, Resyncs int}`;
    - `Verify(...) (VerifyReport, error)`.
  - **`internal/reclaim` (E9; consumed by `journalv2.Repack` and auto-retire):**
    - `Repacker`, with `Pass(budget int64, scope Scope) (freed int64, err error)` and
      `Candidates(scope) []SegmentID`.

  The package doc of each internal package names the lock domain(s) it owns (RFC 1 §8.2)
  and states the acquisition order a caller must respect.
- **Done when:**
  - every row of RFC 1 §4.1's table is a method with its bound in its doc comment;
  - every lock domain of §8.2 is owned by exactly one package;
  - a dependency diagram is drawn in §7 below, and it has no cycle.
- **Rules:**
  - 1 and 5: each seam can be faked behind its interface, so each epic tests on its own
    (rule 1).
  - 4: the counted checks of RFC 1 §9.7 are named on the methods that must expose
    comparison counts (`Lookup`, `Apply`, `Mark`).

### 1.10 Test skeletons, surface test, review and freeze

- **Owner:** A + B. **Depends on:** 1.1–1.9.
- **Deliverables:**
  - **Skeleton docs:** `journalv2test/doc.go` and `conformance/doc.go`, each stating what
    epic 2 will put there: the crash seam, the reference model, the profiler, and the
    conformance suite.
  - **`surface_test.go`:**
    - calls every exported function and method with zero or minimal arguments;
    - asserts that none panics, that each returns `ErrNotImplemented` or a zero value, and
      that iterators terminate. The assertion is written as "returns or errors cleanly",
      so it stays valid once epics implement the methods;
    - runs the error-relation table from 1.3.
  - **The committed `testdata/api.golden`.**
  - **§6 of this file:** the symbol-to-RFC-section map, completed.
  - **The joint review.** Both engineers walk through §6 row by row against RFC 1 §3–§4,
    §7.2 and RFC 28 §2.
- **Done when:**
  - `go test ./pkg/block/journalv2/... ./pkg/sim/...` is green;
  - `go vet` is clean;
  - both engineers have approved;
  - the parent plan records S1, with the date.
- **Rules:**
  - 1 and 5: this subtask completes the guard suite.
  - 6: the known-failure list starts in epic 2, from this frozen surface.

**Changes after the freeze.** A frozen signature changes only with both engineers
agreeing. The change regenerates `api.golden` in the same commit, and a line is added to
§8 of this file.

---

## 6. Symbol → RFC map

Subtask 1.10 completes this table, and it stays the traceability source after the
freeze.

| Symbol | Source | Subtask |
| --- | --- | --- |
| `Init`, `Open`, `Restamp` | RFC 1 §3; RFC 28 §3.4 | 1.4 |
| `Journal.Share`, `Forget`, `Strand` | RFC 1 §3 | 1.5 |
| `Handle.Close`, `SetSyncPolicy` | RFC 1 §3, §5.2 | 1.5 |
| `Handle.Reserve`, `Reservation` | RFC 1 §6 | 1.5 |
| `WriteAt`, `Sync` | RFC 1 §3.1 | 1.5 |
| `ReadAt`, `Held` | RFC 1 §3.2 | 1.5 |
| `Offload`, `OffloadMany`, `Offer`, `MarkOffloaded`, `Unmark` | RFC 1 §3.3 | 1.6 |
| `Fill` | RFC 1 §3.4 | 1.5 |
| `Release` | RFC 1 §3.5 | 1.6 |
| `Truncate`, `Deallocate`, `Delete`, `CloneTarget`, `CloneSpec` | RFC 1 §3.6 | 1.5 |
| `Extents`, `Files`, `Stats`, `DirtyFiles` | RFC 1 §3.7 | 1.5, 1.7 |
| `LossEvent`, `LossGeneration` | RFC 1 §3.8 | 1.7 |
| `Counters` | RFC 1 §3.9 | 1.7 |
| `Since`, `Settle`, `Change` | RFC 1 §3.10 | 1.5 |
| `Hold`, `Unhold`, `Held`, `Stamp` | RFC 1 §3.11 | 1.6 |
| `Version`, `Draw`, `SetVersionLimit` | RFC 1 §4.3 | 1.2, 1.5 |
| `Inspector`, `SegmentInfo`, `VerifyReport` | RFC 1 §4.4 | 1.7 |
| `Repack`, `RepackScope` | RFC 1 §7.2 | 1.6 |
| `DescriptorBudget` | RFC 1 §7.4 | 1.4 |
| `NoSpaceError`, `Limit` | RFC 1 §6, §4.2, §4.3 | 1.3 |
| `FileID` | RFC 0 §3; RFC 1 §2 | 1.2 |
| `storage.FS`, `storage.File` | RFC 1 §1.3 | 1.8 |
| `platform.Probe`, `platform.Check` | RFC 28 Appendix A | 1.8 |
| `format.*` | RFC 28 §2.1–§2.5 | 1.8 |
| `index.*` | RFC 1 §4.1, §4.2, §3.4 (floor buckets), §7.2 (drop ranges) | 1.9 |
| `segment.*` | RFC 28 §2.2, §2.4; RFC 1 §7.3, §7.4, §8.5 | 1.9 |
| `durability.*` | RFC 1 §5, §3.8 | 1.9 |
| `recovery.*` | RFC 28 §3 | 1.9 |
| `reclaim.*` | RFC 1 §7.1, §7.2 | 1.9 |
| `sim.Clock`, `sim.TestClock`, `sim.Fault` | RFC 8 §2.1 | 1.4 |
| `Hooks`, `SpanOp`, `Event` | this plan (general rule 3) | 1.7 |

---

## 7. Order of work and package dependencies

```text
1.1 skeleton ─┬─► 1.2 types ─┬─► 1.3 errors ─► 1.5 data path ─► 1.6 offload/repack ─┐
  (A)         │    (B)       ├─► 1.4 lifecycle (A) ─────────────┘                    │
              │              ├─► 1.7 observability (A)                               ├─► 1.10 freeze (A+B)
              │              └─► 1.8 storage/platform/format (A) ─► 1.9 seams (A+B) ──┘
```

**Parallelism.** A runs 1.1 → 1.4 → 1.7 → 1.8 → 1.9 (all but the index). B runs 1.2 →
1.3 → 1.5 → 1.6 → 1.9 (index). They meet at 1.10.

**Allowed package imports.** Arrows point at the imported package. There is no cycle.

```text
journalv2 ──► internal/{core, storage, format, index, segment, durability, recovery, reclaim}
reclaim    ──► index, segment, durability, core
recovery   ──► format, storage, core          (lands records through Sink, not into index directly)
durability ──► segment, core
segment    ──► format, storage, core
index      ──► core
format     ──► core
platform   ──► storage, core, x/sys
journalv2test ──► journalv2, internal/storage, pkg/sim
```

---

## 8. Change log after the freeze

| Date | Change | Agreed by |
| --- | --- | --- |
| | | |
