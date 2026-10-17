# Epic 1: journal v2 interfaces

**Parent plan:** [rfc-1-journal-plan.md](rfc-1-journal-plan.md), epic 1.
**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §1, §3, §4, §6 and §7.2;
[RFC 28](../rfcs/rfc-28-journal-format.md) §2 and Appendix A;
[RFC 8 §2.1](../rfcs/rfc-8-engine.md) for the shared clock and fault interfaces;
[RFC 29](../rfcs/rfc-29-test-harness.md) for the test harness the guard tests feed.
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
- the journal's `Clock`: the subset of the set's clock (RFC 8 §2.1) that it uses;
- the test packages epic 2 fills (`journalv2test`, `conformance`), created only when their
  first symbol lands (plan §1.5, rule 12);
- the tests that can fail (plan §1.5, rule 10): the import rule, the RFC signature compile
  check, typed errors matching their sentinels, and `Version`'s byte order.

### 1.2 Out of scope

- Any behaviour, with two exceptions: `Version`'s order and byte encoding, which other
  components store and compare (1.2), and the `Is` relation of the typed errors (1.3).
- The test components themselves: the crash seam, the reference model, the
  known-failure list and the profiler all belong to epic 2.
- Wiring v2 into DittoFS, configuration keys and exporters. Those are epic 10.
- Cluster-only surface (RFC 10): no extension kinds, no non-zero epoch, no
  `ErrForgotten`.

---

## 2. General rules for each epic

The general rules are in [§1.5 of the plan](rfc-1-journal-plan.md#15-general-rules-for-each-epic).
They are linked rather than copied, so they keep one source. Every subtask states how it
meets each rule that applies to it; a rule that does not apply is named with the reason.

### 2.1 How the rules apply to this epic

| Rule | Applies to epic 1 as |
| --- | --- |
| 1. Unit tests | **Applies.** Epic 1 keeps exactly the tests that can fail (rule 10): the import rule, `rfc_signatures_test.go`, `TestTypedErrorsMatchTheirSentinel` and `TestVersionBytesOrder`. |
| 2. Fuzz | **Does not apply.** No parser is implemented here. The fuzz targets belong to epic 3 (codecs) and epic 4 (operation sequences). |
| 3. Metrics and tracing | **Applies as declarations.** Every field of RFC 1 §3.7 and every counter of §3.9 is declared in `Stats`. The `Hooks` contract and its span and event names are fixed. Nothing is maintained yet. |
| 4. Profiling | **Does not apply.** There are no hot paths yet, and the profiler is epic 2's. |
| 5. Public and internals | **Applies.** The import rule covers every package; the signature check covers the public package. |
| 6. Known failures | **Does not apply.** No e2e test exists yet. The list is created in epic 2. |
| 7. v2 ignores v1 | **Applies.** The import test forbids `pkg/block/journal` in every v2 package, test files included. |
| 8. Real storage | **Does not apply.** Nothing touches storage. |
| 9. Comments | **Applies.** Doc comments state contracts in terms of the code, with no RFC or epic numbers. The symbol-to-RFC map is in §6 of this file. Decisions taken here carry `decision:` markers at their declaration. |
| 10. Tests must be able to fail | **Applies.** See rule 1. The surface test and the API golden file are not kept. |
| 11. Export only what RFC 1 §3 uses | **Applies.** `Seq`, `Version.Less`/`IsZero`, `Extent.End`/`Overlaps` and the proposed constants are not exported. |
| 12. Create only what is declared | **Applies.** No empty package and no stub without a caller. |

---

## 3. Decisions this epic takes

Each decision below is recorded at its code site with a `decision:` marker and is
reviewed at the freeze (subtask 1.10).

**D1. Package location and name: `pkg/block/journalv2`. The `v2` suffix stays in Go
package and import paths only.**
v1 keeps `pkg/block/journal` until epic 10 retires it. A sibling package lets v2 be built while v1
keeps serving. Epic 10 then moves the callers over with no configuration key between them,
and retiring v1 becomes a plain directory delete. Renaming the package afterwards is out of scope.

- *Why the suffix stops at the package:* renaming a Go package later is cheap, because it
  touches only import paths inside this repository. Names users see are not. Metric
  names, span names, log fields and dashboard queries are built into users' dashboards
  and alerts, and dropping a suffix from them later breaks those.
- *The rule:* every name visible outside the Go code says `journal`, never `journalv2`.
  This covers metric names and labels, span and event names, `slog` attribute keys, and
  dashboard titles, UIDs and queries. Where the journal version matters to a consumer, it
  is exposed as a `version` label (`v1` or `v2`) or attribute, never encoded in a name.
- *Applies in this epic to:* the `SpanOp` and `EventKind` string values and the `slog`
  attribute keys in `hooks.go`. None of them carries a version. The `Stats` field names
  carry none either, so epic 10's exporters can name metrics from them directly.
- *What would overturn it:* v1 and v2 needing metrics with the same name but
  incompatible meanings, where a label cannot tell them apart.

**D2. Value types live in a leaf package, `internal/core`, and the public package
re-exports them as type aliases.**
For example, `type Version = core.Version`.

- *Why:* the internal packages (format, index, segment, …) need `Version`, `FileID` and
  `SegmentID`. The public package imports those internal packages. If the types lived in
  the public package, the internal packages would have to import it, and that is an
  import cycle.
- *What it costs:* godoc shows the types as defined in `internal/core`.

**D3. The journal's `Clock` is the subset of the set's clock it uses, in standard-library
types, with no adapter:**

```go
type Clock interface {
    Now() time.Time                        // wall time, for OldestDirty and LastSyncError
    Elapsed() time.Duration                // monotonic, since an origin the clock chose
    After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}
```

- *Why:* RFC 8 §2.1 now declares the set's one clock in standard-library types (`Now`,
  `Elapsed`, `After`), so the production clock and the test clock satisfy the journal's
  subset as they are. RFC 1 §1.1 still bars the journal from importing them; it does not
  need to.
- *What `Now` is not for:* mtimes. `WriteAt` takes the mtime from its caller, which stamps
  it with store time, so the journal never reads the clock for one.
- *Where the production and test clocks live* is decided with #3162, not here. The journal
  depends on neither package.

**D4. The journal takes no `Fault` interface.**
Faults are injected by the storage seam implementation, which epic 2 writes in
`journalv2test` and which consults the set's fault schedule. The journal only ever sees
`storage.FS`. The fault interface lands with epic 2's seam, its first consumer, and RFC 8
lists `FaultAction`'s kinds without declaring the type, so declaring it is epic 2's.

**D5. Dependencies are injected through variadic options appended to RFC 1's
signatures.**
For example, `Open(dir, floor, expect, opts ...Option)`. A call written exactly as RFC 1
writes it still compiles. `Option` is a struct whose apply function is unexported, so only
code inside the journal's tree can write one. The options are:

- `WithClock`. **`Open` requires it** and fails with `ErrNoClock` without it: a journal
  never falls back to the system clock, so a test that forgets its clock fails at once
  instead of running on real time (RFC 8 §2.1). `Init` and `Restamp` read no time and
  take none;
- `WithSettings`. Without it every setting takes its default;
- `WithDescriptorBudget`. Without it the journal gets a budget of its own;
- `WithStorage(storage.FS)`, declared in 1.8, and `WithHooks`, declared in 1.7, each with
  the subtask that declares its type.

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

**D8. Stubs never panic. This is a review rule, not a test.**
Every stub returns the zero value with `ErrNotImplemented`, or an empty iterator. A
`Settle`-style method with no result does nothing.

- *Why:* a panic aborts the test binary, and every later test in it then reports as not
  run, not as failing on its assertion. Epic 2 needs every test to fail on its own
  assertion.
- *Why not a test:* a test that calls stubs returning zero cannot fail (plan §1.5,
  rule 10).
- *What it requires of tests:* a test that expects a refusal asserts the **specific**
  error, through `errors.Is` with the named sentinel or `errors.As` with its type. A test
  that accepts any error passes against `ErrNotImplemented` and proves nothing.

**D9. `FileID` is an opaque struct built only by `NewFileID(id [16]byte) FileID`.**
RFC 0 §3 requires a type that cannot be reached by conversion.

- *Limit:* the journal cannot import the metadata store's ID type. The constructor
  therefore takes the UUID bytes, and epic 10 calls it in exactly one place, from the
  metadata file ID.
- *Ceiling:* any 16 bytes are accepted, the all-zero ID included, because `FileID{}`
  exists anyway. Recorded with a `decision:` marker.

**D10. Fixed values are private constants. Settings are fields.**

- Constants, private in `internal/core/limits.go`: segment cap 256 MiB, pins 64 per
  offer and 256 per journal, 4,096 floor buckets, idle-seal age 30 s. Only `SyncBound`
  (1 s) is exported, because a caller's sync policy is stated in terms of it.
- Settings, declared once in `internal/core` and aliased publicly: `MaxBytes`, `Streams`,
  `ExtentLimit`, `HeadroomRecords`, `IdleSealBytes`, `SpareTarget`.
- The segment cap is **not** a setting, and epic 1 declares no way to override it. The
  override lands with the first test that needs a journal of many segments in a
  temporary directory (plan §1.5, rule 12).

**D11. `SyncPolicy`'s zero value is `Bounded`**, which RFC 1 §5.2 names as the default.

**D12. The descriptor budget is a value shared across journals:
`NewDescriptorBudget(limit int) *DescriptorBudget`.**
RFC 1 §7.4 makes the limit per process, while each journal is built separately. A
journal built without a budget gets a private one at the default limit.

---

## 4. Target tree

The tree at the end of the epic. Each package appears with the subtask that declares its
first symbol, never as an empty placeholder (plan §1.5, rule 12). Where the set's
production clock and test clock live is decided with #3162; the journal imports neither.

```text
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
  options.go        Option, WithClock, WithSettings, WithDescriptorBudget, WithStorage, WithHooks, SyncBound
  imports_test.go          import rule for every package under journalv2
  rfc_signatures_test.go   calls written exactly as RFC 1 writes them; a drifted signature stops the build
  errors_test.go           every typed error Is its own sentinel
  internal/core/        value types, Clock, Config, Settings, DescriptorBudget, limits.go (private constants)
  internal/storage/     FS, File
  internal/platform/    OS storage.FS, Probe
  internal/format/      sizes, kinds, structures, codecs, CRC, Scanner
  internal/index/       Index, FileIndex
  internal/segment/     Store, Stream, Guard, Accounting
  internal/durability/  Coordinator, Window
  internal/recovery/    Rebuild, Sink, Verify
  internal/reclaim/     Repacker
```

`journalv2test` and `conformance` are created by epic 2, with their first symbol.

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

### 1.1 Package skeleton and the import rule

- **Owner:** A. **Depends on:** nothing.
- **Deliverables:**
  - the public package's `doc.go`. Every other package lands with the subtask that
    declares its first symbol;
  - `imports_test.go`, covering every package under `pkg/block/journalv2/`. It is modelled
    on v1's `foreign_imports_test.go`: it reads `go/build`, parses files built for other
    platforms, and checks test imports too. It allows:
    - the standard library and the module's own `pkg/block/journalv2/...` packages,
      everywhere;
    - `golang.org/x/sys`, in `internal/platform` only (D6);
    - the set's clock and test-clock packages, in `journalv2test` only.

    It forbids `pkg/block/journal` everywhere.
- **Done when:**
  - the import test fails on a planted `pkg/block/journal` import, on a planted
    third-party import in a non-platform package, and on a planted x/sys import outside
    `internal/platform`. Each is checked by reverting the plant after watching the failure;
  - `go build ./...` passes.
- **Rules:**
  - 1, 5 and 10: the import rule is a RFC 1 §1.1 MUST, and it can fail.
  - 7: v1 is forbidden by test.
  - 12: no empty packages.

### 1.2 Value types

- **Owner:** B. **Depends on:** 1.1.
- **Deliverables:** in `internal/core`, aliased in `types.go`:

  | Type | Definition |
  | --- | --- |
  | `Version` | `struct{ Epoch, Counter uint64 }`, **implemented**: `Compare(Version) int`, `Bytes() [16]byte` and `VersionFromBytes([16]byte) Version`. `Less` and `IsZero` are not exported (rule 11). `Bytes` is **big-endian, epoch then counter**: byte for byte the metadata store's `entity.JournalVersion`, so `bytes.Compare` on two `Bytes` orders as `Compare` does. It is never RFC 28's little-endian on-disk encoding, which belongs to `internal/format`. |
  | `FileID` | opaque struct, `NewFileID([16]byte) FileID`, `(FileID) Bytes() [16]byte` (D9) |
  | `Extent` | `struct{ Off, Len int64 }`. `End` and `Overlaps` are not exported (rule 11) |
  | `Held` | `struct{ Extent Extent; Version Version }` |
  | `ShareTag` | `uint32` |
  | `ShareID` | `[16]byte` |
  | `SnapshotCut` | `uint64` |
  | `SyncPolicy` | `uint8`, with `Bounded` (the zero value, D11) and `Standard` |
  | `JournalID`, `InstallationID` | `[16]byte` |
  | `SegmentID` | `uint64`. `Seq` stays internal (rule 11) |

- **Done when:**
  - the doc comments state the invariants. For example: `FileID` encodes no share; a
    version's epoch is zero in this format; a version is compared epoch first, as one
    unsigned 128-bit number;
  - `TestVersionBytesOrder` asserts that `bytes.Compare` of `Bytes` agrees with
    `Compare`, on counters that differ only in their low byte and across the epoch
    boundary. A little-endian `Bytes` fails it.
- **Rules:**
  - 9: the `decision:` markers for D9 and D11 sit at their declarations.
  - 10: `TestVersionBytesOrder` is kept because the encoding is a contract other
    components store.

### 1.3 Errors and the refusal taxonomy

- **Owner:** B. **Depends on:** 1.2.
- **Deliverables:** in `errors.go`:
  - **Sentinels:**

    | Sentinel | Returned when |
    | --- | --- |
    | `ErrNotImplemented` | a stub is called |
    | `ErrNoSpace` | a capacity limit refuses; always carried by a `NoSpaceError` (below) |
    | `ErrOtherShare` | a call through a handle names a file whose tag is another share's, or a file recovery rebuilt under two tags; carried by an `OtherShareError` (below) |
    | `ErrCorrupt` | a checksum does not verify |
    | `ErrNoClock` | `Open` is called without `WithClock` (D5) |
    | `ErrInvalidSetting` | a setting is out of its documented range |
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
    - `OtherShareError{File FileID; Tag, Owner ShareTag}`, naming the handle's tag and the
      tag that owns the file. It matches `ErrOtherShare` under `errors.Is`.
  - **The `ENOSPC` rule:** a full filesystem surfaces as the platform's
    `*fs.PathError` wrapping `syscall.ENOSPC`. It is **never** a `NoSpaceError`, so
    `errors.Is(err, ErrNoSpace)` and `errors.Is(err, syscall.ENOSPC)` are never both true
    for one error.
- **Done when:**
  - each error's comment states which calls return it and whether the caller should
    treat it as retry-later;
  - `TestTypedErrorsMatchTheirSentinel` checks that every typed error `Is` its own
    sentinel, which catches a copy-pasted sentinel. It does not test distinct sentinels or
    `errors.As`/`Unwrap`, which cannot fail or test the standard library (rule 10).
- **Rules:**
  - 1, 5 and 10: the typed-error test.

### 1.4 Lifecycle, injection and the clock

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
    - `Option`, a struct with an unexported apply function, and `WithClock(Clock)`,
      `WithSettings(Settings)`, `WithDescriptorBudget(*DescriptorBudget)` (D5);
    - aliases for `Clock`, `Settings` and `DescriptorBudget`, declared once in
      `internal/core`;
    - `SyncBound`, the one exported constant (D10);
    - `NewDescriptorBudget` (D12).
  - `internal/core`: `Clock` (D3), `Config`, `Settings{MaxBytes int64; Streams int;
    ExtentLimit int64; HeadroomRecords int; IdleSealBytes int64; SpareTarget int}` with a
    documented default per field, and `limits.go` with the private constants (D10).
  - `rfc_signatures_test.go`: calls written exactly as RFC 1 writes them, with no
    options. A dropped, reordered or retyped parameter stops the build. Later subtasks
    add their calls to it.
- **Done when:**
  - `Open` without `WithClock` returns `ErrNoClock`, and `Init` and `Restamp` take no clock;
  - every setting documents its unit, default and refusal behaviour (`ErrInvalidSetting`).
- **Rules:**
  - 10: the signature check can fail; it is the only test this subtask adds.
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
  - 10: every method's RFC 1 call is added to `rfc_signatures_test.go`.

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
    `LossGeneration`, `TagConflicts []TagConflict`, and `Shares map[ShareTag]ShareStats`.
    Supporting types:
    - `TagConflict{File FileID; Tags [2]ShareTag}`, a file recovery rebuilt under two
      tags;
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
    not block, and must not call back into the journal. `WithHooks(Hooks)` is added to
    `options.go` here, with the type it takes (D5).
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
    doc comment says so. `WithStorage(storage.FS)` is added to `options.go` here, with the
    type it takes (D5).
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

### 1.10 Review and freeze

- **Owner:** A + B. **Depends on:** 1.1–1.9.
- **Deliverables:**
  - **§6 of this file:** the symbol-to-RFC map, completed.
  - **The joint review.** Both engineers walk through §6 row by row against RFC 1 §3–§4,
    §7.2 and RFC 28 §2, and check the review rules: no stub panics (D8), nothing exported
    that RFC 1 §3 does not use (rule 11), no empty package or uncalled stub (rule 12).
- **Done when:**
  - `go test ./pkg/block/journalv2/...` is green;
  - `go vet` is clean;
  - both engineers have approved;
  - the parent plan records S1, with the date.
- **Rules:**
  - 10: no test is added here; the review checks that every kept test can fail.
  - 6: the known-failure list starts in epic 2, from this frozen surface.

**Changes after the freeze.** A frozen signature changes only with both engineers
agreeing. The change updates `rfc_signatures_test.go` in the same commit, and a line is added to
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
| `ErrOtherShare`, `OtherShareError`, `TagConflicts` | RFC 1 §3, §3.7 | 1.3, 1.7 |
| `ErrNoClock`, `ErrInvalidSetting` | RFC 8 §2.1; this epic (D5, D10) | 1.3, 1.4 |
| `FileID` | RFC 0 §3; RFC 1 §2 | 1.2 |
| `storage.FS`, `storage.File` | RFC 1 §1.3 | 1.8 |
| `platform.Probe`, `platform.Check` | RFC 28 Appendix A | 1.8 |
| `format.*` | RFC 28 §2.1–§2.5 | 1.8 |
| `index.*` | RFC 1 §4.1, §4.2, §3.4 (floor buckets), §7.2 (drop ranges) | 1.9 |
| `segment.*` | RFC 28 §2.2, §2.4; RFC 1 §7.3, §7.4, §8.5 | 1.9 |
| `durability.*` | RFC 1 §5, §3.8 | 1.9 |
| `recovery.*` | RFC 28 §3 | 1.9 |
| `reclaim.*` | RFC 1 §7.1, §7.2 | 1.9 |
| `Clock` | RFC 8 §2.1 (the subset the journal uses) | 1.4 |
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
journalv2test ──► journalv2, internal/storage, the set's clock and test-clock packages (epic 2)
```

---

## 8. Change log after the freeze

| Date | Change | Agreed by |
| --- | --- | --- |
| | | |
