# Epic 2: journal v2 red end-to-end suite and test foundations

**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §1.3 and §9;
[RFC 28](../rfcs/rfc-28-journal-format.md) §2, §4.1, §4.2 and §4.4;
[RFC 8 §2.1](../rfcs/rfc-8-engine.md) for the clock and the fault interface, and RFC 8 §15's
"one clock" row; [RFC 0 §1.4](../rfcs/rfc-0-data-lifecycle.md) for the clock bound;
[RFC 29](../rfcs/rfc-29-test-harness.md) §4.1, §4.3, §4.6 and §4.7 for registration and
grading; [the RFC index](../rfcs/rfc-index.md), "Test tiers" and "Test harness".
**Owners:** track A, storage (@viquitorreis), and track B, model and operations
(@didimelli), with the split given per subtask; @simonebaracchi coordinates and runs the
review at the end.
**Estimate:** 17 subtasks; sizes in §8.
**Depends on:** epic 1's frozen public surface of `pkg/block/journalv2` (the end of
epic 1 is sync point **S1, interfaces frozen**).
**Blocks:** all testing in epics 3–12. The end of this epic is sync point **S2, red suite
in CI**.

---

## 1. Goal

Epic 2 makes "done" measurable before any journal behaviour exists. It delivers:

- **the test components** every later epic tests with: a crash-simulating storage seam on
  a real filesystem, the set's fault interface with a seeded schedule, the test clock, a
  reference model of the journal, and a profiling harness;
- **the red conformance suite**: one Go test per named check of RFC 1 §9.4 and RFC 28 §4.2
  that can be written against the public surface, the edge cases of RFC 1 §9.2 and
  RFC 28 §4.1, the systematic damage of RFC 28 §4.4, the model-based test of RFC 1 §9.1,
  and the counted sync checks of RFC 1 §9.7;
- **grading**: the suite registered with `dt`, every test listed in its known-failure
  file, so the run is green while every test fails;
- **traceability**: a map from every RFC check row to the test that implements it, or to
  the epic that owns it, checked by a test. "Every row" covers the check tables of RFC 1
  §9.4 and RFC 28 §4.2, and the lists every suite test checks against: RFC 1 §9.2 and
  §9.7, RFC 28 §4.1 and §4.4, and the clock rows of RFC 0 §1.4 and RFC 8 §15 (D3).

At the end of the epic:

- the suite compiles against epic 1's stubs and runs in CI through `dt`;
- every test fails, none skips, none panics, and every one is a listed known failure, so
  the cell is green;
- every row in D3's scope maps to a test or to a named owner, and every suite test maps to
  a row;
- the seam, the fault schedule, the clock, the model and the profiler are implemented in
  full and their own tests are green, because they do not depend on the journal.

### 1.1 In scope

- `pkg/sim`: `TestClock`, `Fault`, `FaultAction` and the seeded `Schedule`.
- `pkg/block/journalv2/journalv2test`: the crash seam, the profiler, the fixture, the
  reference model and the hand-written byte builders.
- `pkg/block/journalv2/conformance`: the conformance tests, the model-based test and its
  corpus.
- `test/journalv2/`: the known-failure file and the traceability map with its check.
- `test/suites.yaml`: the `journal-conformance` suite, the `journal` group, and the new
  build tag added to `lint-go`'s vet line.
- The catalogue of system scenarios, as §7 of this file, for epics 10 and 11 to write.

### 1.2 Out of scope

- Any journal behaviour. A test never turns green in this epic.
- Checks that need a package this epic does not create (§2, rule 12): the encoders'
  golden vectors and the platform checks (epic 3), the index's comparison counters
  (epic 4), the lock-order recorder (epic 8). §6 lists each, with its owner.
- The allocations-per-operation assertion: it has no consumer until a hot path exists,
  and a threshold cannot be set against a stub. It lands with epic 4's index and epic 8's
  data path.
- The production clock `pkg/clock`, which lands with its first caller in epic 11.
- System e2e tests and their fixtures (D11).
- Tiers beyond `pr`, the many-seed and soak runs and the benchmarks: epic 12.

---

## 2. General rules for each epic

Every subtask states how it meets each rule that applies to it; a rule that does not apply
is named with the reason.

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

### 2.1 How the rules apply to this epic

| Rule | Applies to epic 2 as |
| --- | --- |
| 1. Unit tests | **Applies to the components.** The seam, the schedule, the clock, the model, the profiler and the byte builders each carry unit tests that are green at landing, since none of them depends on the journal. The conformance tests are the red suite, not unit tests. |
| 2. Fuzz | **Applies to the model.** A fuzz target drives operation sequences against the reference model alone, with the property that the model never panics and never reports bytes it was not given. Fuzzing the journal's parsers is epic 3's, where the parsers are. |
| 3. Metrics and tracing | **Applies as checks, not as code.** RFC 1 §9.4 Group D's checks assert each `Stats` field and counter from the public surface (2.5b). Nothing here maintains a counter. |
| 4. Profiling | **This epic builds it.** Every conformance test runs under the profiler through the fixture (2.3, 2.5a). The allocations-per-op assertion is out of scope (§1.2). |
| 5. Public and internals | **Applies.** Conformance tests drive only the public surface of `journalv2` and observe the disk only through the seam and the byte builders: they test the consumer of every value. The components test their own APIs. |
| 6. Known failures | **This epic builds it.** Every conformance test lands with its entry in `test/journalv2/known.yaml` (D1). No test turns green here. |
| 7. v2 ignores v1 | **Applies.** The import test covers `journalv2test` and `conformance`. Nothing in this epic reads v1's code or format, and v1's test helpers (its fake clock) are not reused. |
| 8. Real storage | **Applies, and is the reason for the seam.** The seam wraps a real directory. The reference model is in memory but stands in only for the expected answer, never for storage. |
| 9. Comments | **Applies.** Each conformance test's doc comment states the check in terms of the journal's behaviour, with no RFC section or epic number; the map from test to RFC row lives in `test/journalv2/trace.yaml` (D3). `decision:` markers for D5–D8 and D12 sit at their code sites. |
| 10. Tests must be able to fail | **Applies, with D4's limit.** Every conformance test asserts the specific outcome its RFC row names, through `errors.Is` or `errors.As` against the named sentinel where it expects a refusal, so a stub's `ErrNotImplemented` never satisfies it. |
| 11. Export only what RFC 1 §3 uses | **Applies to `journalv2` only.** This epic adds no exported symbol to the journal's public package. The segment-cap override is an unexported field reached through `internal/core` (D8). `pkg/sim` and `journalv2test` are test packages and export what their callers use. |
| 12. Create only what is declared | **Applies.** Each component lands with its first consumer: the clock with the schedule's delay, the schedule with the seam, the seam and profiler with the fixture's first tests, the byte builders with their own field-by-field tests against RFC 28 §2, which epic 3's golden vectors are checked against (2.13a). No system e2e scaffolding lands without a test (D11). |

---

## 3. Decisions this epic takes

Each decision is recorded at its code site with a `decision:` marker where it has one, and
is reviewed in 2.15.

**D1. Known failures are graded by `dt`, from a YAML list. Epic 2 writes no Go
known-failure reader.**
RFC 29 §4.7 replaces the Markdown lists with one `known.yaml` per suite, and `dt` already
reads and grades it (`test/harness/known`). A second matcher would drift from it.

- *The file:* `test/journalv2/known.yaml`, named by the suite's `known:` field.
- *One entry per test, never a pattern.* `dt` judges a pattern on every result it matched
  and calls it an unexpected pass only when all of them pass, so a pattern would let one
  test turn green without its entry leaving the list.
- *Each entry:* `status: fail`, `fix: planned`, `category: feature`, a reason naming
  what is missing (for example "no data path yet"), `issue:` naming the issue of the
  epic expected to turn it green, and `message:` naming the failure it is expected to
  fail with (D4).
- *`message:` is a `dt` change, not this epic's.* Today an entry matches by name only, so a
  listed test failing for any reason is graded `known`. [#3391](https://github.com/marmos91/dittofs/issues/3391)
  adds an optional `message:` to an entry, in RFC 29 §4.7 and `test/harness/known`: a
  failure counts as expected only if its message contains that text, and is graded new
  otherwise. Until it lands, entries carry no `message:` and D4's limit stands; the change
  that lands it, or the first journal change after it, adds `message:` to every entry.
- *What it gives:* a listed test that fails is graded `known`; a listed test that passes
  fails the cell (exit 1), so the epic that turns it green removes its entry in the same
  change; a test that skips, or does not run, is not run (exit 2, RFC 29 R10).

**D2. The conformance suite sits behind a build tag and is its own `dt` suite.**
The `unit` suite runs `go test ./pkg/...`, which would otherwise run every red test and
turn `unit` red.

- *The tag:* `journal_conformance`, on every file of `conformance/`.
- *The suite*, in `test/suites.yaml`:

  ```yaml
  journal-conformance:
    format: go-json
    cmd: go test -race -tags=journal_conformance -timeout=30m ./pkg/block/journalv2/conformance/...
    known: test/journalv2/known.yaml
    tier: pr
  ```

  and a `journal` group naming it, as RFC 29 §4.9 asks of a component.
- *Vetting:* `journal_conformance` is added to `lint-go`'s `go vet -tags=...` line, so the
  tagged files are compiled on every PR.
- *Tiers beyond `pr`* (many seeds, a non-race leg) are epic 12's.

**D3. Test names carry the RFC group; the map to RFC rows is a file, checked by a test.**

- *Names:* `TestRFC1A_NoZeroFill`, `TestRFC1B_ReserveAhead`, `TestRFC28C_SourcesAgree`,
  `TestEdge_WriteCrossesSegment`, `TestModel`, `TestCounted_SyncsPerWrite`,
  `TestDamage_BitFlip`, `TestClock_WallJumpMovesNoTimer`. One file per group:
  `rfc1_a_test.go`, …, `rfc28_c_test.go`, `edge_test.go`, `model_test.go`,
  `counted_test.go`, `damage_test.go`, `clock_test.go`.
- *The scope:* every test of the suite checks a row of one of these, and every one of them
  is parsed from the RFC file, so no test is exempt by name:

  | Source | Rows | Key |
  | --- | --- | --- |
  | RFC 1 §9.4, RFC 28 §4.2 | each table row, by group | `rfc1 / 9.4 / A / "no zero-fill"` |
  | RFC 1 §9.2, RFC 28 §4.1 | each bold heading of the list (`Extent shape`, `Lifecycle`, `Capacity and storage`, `Opening`); bullets above the first heading form one row keyed by the section alone | `rfc1 / 9.2 / "Extent shape"`, `rfc28 / 4.1 / -` |
  | RFC 1 §9.7 | each bold item (`syncs per write`, `index cost`, …) | `rfc1 / 9.7 / "syncs per write"` |
  | RFC 1 §9.1 | the `Model-based` row of the table | `rfc1 / 9.1 / "Model-based"` |
  | RFC 28 §4.4 | each bold kind of damage (`Bit corruption`, `Crashes`), and each row of the edits table by its `Edit` cell | `rfc28 / 4.4 / "Crashes"`, `rfc28 / 4.4 / edit / "a segment deleted"` |
  | RFC 0 §1.4, RFC 8 §15 | the named rows only: `clock bound`, and `one clock` | `rfc0 / 1.4 / "clock bound"` |

  A whole section is in scope for the first five sources, so a row added to one fails CI
  until it is mapped. From RFC 0 and RFC 8 only the named rows are, since the rest of those
  tables is other components' checks; for them the check fails only when a named row
  disappears.
- *The map:* `test/journalv2/trace.yaml`. Each row maps to a list of tests, or to
  `owner: epic N` with a reason, or both when a row is split (D9 shows how).
- *The check:* `test/journalv2/trace_test.go`, run by `unit`. It parses the rows of the
  scope above straight from the RFC files, and the conformance package's test functions
  with `go/parser`. It fails when an in-scope row has no entry, when an entry names a row
  that no longer exists, when a named test does not exist, or when a test of the suite is
  in no entry. It has no skip list: a test that checks no row is a test without a
  requirement, and is either given one in the RFC or deleted.
- *"Did not run"* is `dt`'s: in a suite with a known file, a test that skips or yields no
  result is exit 2. Together with the existence check, a mapped check that did not run
  fails CI.

**D4. Until `Open` works, every test fails in the fixture, not on its own assertion.**
Every conformance test opens a journal through the fixture, and `Init` and `Open` are
stubs until epics 7 and 8 land, so every test fails at the fixture's first step.

- *What S2 can check:* every test compiles, runs, fails without a panic or a skip, and is
  listed. The fixture reports its own failures with the prefix `fixture:`, so a fixture
  failure is never mistaken for the check's.
- *Stubs name themselves.* Once `Open` works, a test can fail on a journal call that is
  still a stub. The suite's assertion helpers report an error that `Is`
  `ErrNotImplemented` as `stub: <call>` (`stub: Offload`), never as the check's failure.
- *Failing for the right reason is graded, once `dt` can* (D1, #3391). Every entry carries
  `message: "fixture:"` until `Open` works. The change that makes `Open` work, at S3, moves the entries
  still red to `message: "stub:"`, so a test still failing in the fixture after S3 is
  graded new. From S3 on, an epic that implements a call a test was failing on either
  turns the test green or sets its entry's `message:` to the assertion it now fails on;
  a test that starts failing on anything else is graded new. At S4 no component entry
  is left, so none can still fail on a stub.
- *What it cannot check:* that a test's assertion is the right one. That is verified per
  test by the epic that turns it green, by the project rule: revert the implementing
  change and watch the test fail on its assertion.
- *What review checks instead:* each test against its RFC row, and that each refusal is
  asserted against its named sentinel, never as "any error".

**D5. The fault interface is RFC 8's, in `pkg/sim`, with the action kinds RFC 8 lists.**

```go
type Fault interface {
    At(op string, key []byte) FaultAction
}

type FaultAction struct {
    Kind  FaultKind     // Pass, Fail, Delay, Drop, Tear, Reorder
    Err   error         // Fail: the error returned
    Delay time.Duration // Delay: waited on the schedule's clock
    N     int64         // Tear: bytes kept
}
```

- *Why declared here:* `Fault` and `FaultAction` are the node's, not the journal's (RFC 8
  §2.1): every fault seam implements `Fault`, including a fault-injecting store and a fake
  `Content`. `FaultAction` is not a standard-library type, so it cannot be redeclared per
  component the way `Clock` is, and `pkg/sim` is its one home.
  [#3347](https://github.com/marmos91/dittofs/pull/3347) records that in RFC 8 §2.1; until
  it merges, `develop`'s RFC 8 still shows a tuple result, which no code implements.
- *The seam's operation names:* `create`, `open`, `allocate`, `write`, `read`,
  `read_direct`, `sync`, `truncate`, `close`, `rename`, `remove`, `list`, `sync_dir`. The
  key is the file name. These names are the seam's contract with every schedule, and are
  documented on the seam.
- *Not frozen until S2.* `FaultAction`, the kinds and the seam's operation names are
  reviewed with the filesystem service's owner at 2.15 before they freeze, since the
  store's and the engine's fault seams implement the same interface. A change after S2
  goes through RFC 8 §2.1.
- *`Schedule`* implements `Fault` from a seed and a list of rules (an op, a key pattern,
  a call count or probability, an action). The same seed gives the same actions in the
  same order of calls.

**D6. A hold is test choreography in the seam, not a fault.**
Many checks stop the journal at one call and act while it waits ("hold a `Fill` at the
storage seam after it has found the extent absent"). A `Delay` on the test clock cannot do
this, because advancing the clock to release it also fires the journal's own timers.

- *The API:* `seam.Hold(op, key) *Hold`, with `Reached() <-chan struct{}`, `Waiting() int`
  and `Release()`. A held call blocks until `Release`, on no clock.
- *Counted checks* use `Waiting()`: the syncs-per-write check holds each sync until all 16
  writers are waiting, so scheduling cannot decide the result (RFC 1 §9.7).

**D7. Seeds are deterministic, and a failing seed is kept, never retried into green.**
The index forbids retrying or quarantining a failing seeded test, while `dt` retries
failures on develop (RFC 29 §4.7).

- *Source of the seed:* `DITTOFS_SEED` when set, otherwise a fixed seed per test. A retry
  inherits the same environment, so it replays the same seed and cannot pass by drawing
  another.
- *Corpus:* `conformance/testdata/model/` holds sequences, not seeds: hand-written ones
  for the interleavings and edge cases RFC 1 §9.1 and §9.2 name, and the shrunk sequence
  of every seed that failed. The model-based test replays every one before the seeded
  sequence. A failing seed is shrunk to the shortest failing sequence, written to
  `t.ArtifactDir()` as a ready-to-commit corpus file, and printed; the fix that resolves
  it commits the file.
- *Random seeds* run only where a tier sets `DITTOFS_SEED` from a fresh draw, which epic 12
  registers.

**D8. The segment cap is overridden by an unexported setting reached only inside the
journal's tree.**
Checks over many segments (1,000 segments for pinned-segment bounds, recycling, the
descriptor bound) are unrunnable at 256 MiB per segment in a temporary directory. The cap
is not a setting, and no exported symbol may be added for it (rule 11).

- *How:* `core.Settings` gains an unexported field, set only through
  `core.SetSegmentCap(*Settings, int64)` in `internal/core`. The fixture, inside the
  journal's tree, sets it and passes the settings through the public `WithSettings`.
  Code outside `pkg/block/journalv2/...` cannot reach `internal/core`, so the public
  surface does not change. The field is honoured once epic 5 implements segments.
- *Default in the fixture:* 1 MiB. A check that names a size in the RFC (a 1 MiB record,
  a 64 KiB extent) keeps that size and states the cap it needs.

**D9. RFC 28's bytes are written by hand from RFC 28 §2, in `journalv2test/raw`.**

- *Why:* the format checks need on-disk bytes before the encoder exists, and an encoder
  checked against itself proves nothing. `raw` is written from the RFC tables alone, with
  the standard library's CRC32C (`hash/crc32`, Castagnoli), and never imports
  `internal/format`. Epic 3's golden-vector tests compare the encoders' output with
  `raw`'s, which makes `raw` an independent oracle.
- *What it builds:* the `format` file, a segment header, a record header of any kind with
  its payload, a seal marker, catalog entries and a trailer, plus a parser for the
  `format` file and the segment header so a test can read the identity of the journal it
  plants into.
- *Split rows.* "Kind values", "segment header and seal marker" and "`format` file" each
  have an encoding half (epic 3) and a refusal half (here). `trace.yaml` lists both.

**D10. Conformance tests reach the clock only through the fixture.**
Epic 1's import rule allows `pkg/sim` in both `journalv2test` and `conformance`, and
`conformance` needs it: a check that fails a sync builds its `sim.Schedule` rules and
`FaultAction`s. The clock is the exception. The fixture owns the one `TestClock` the
journal and the seam read, and exposes `Advance`, `Jump` and `Now`, so a test cannot
advance a clock the journal does not read. That is a review rule (2.15), not an import
rule; the import rule stays as epic 1 wrote it.

**D11. No system e2e scaffolding lands in this epic. Its catalogue does.**
No `dfs` can run journal v2 before epic 11, and there is no configuration key to select
it, so a system test written now could only fail at start. What a system test needs
already exists: the `e2e` suite and its framework start a real `dfs`, mount NFS and SMB,
and `dt` grades the suite against a known file once e2e opts in (RFC 29 §9). The fixtures
specific to the journal (several journal devices, a full device, a moved disk) would have
no caller here, so they land with epic 10's first test (rule 12). The catalogue is §7 of
this file.

**D12. The profiler uses the standard library only.**
The import rule forbids third-party packages in the journal's tree, test files included,
so `goleak` is not available.

- *Goroutines:* a snapshot of `runtime.Stack` before the test, compared after it, with
  the test runner's own goroutines filtered. The check retries for up to 2 s of real time,
  because goroutines a closed journal has joined can still be exiting. This is the one
  real-time wait in the suite.
- *Descriptors:* the entries of `/proc/self/fd` on Linux and `/dev/fd` on macOS. On other
  platforms the descriptor check does not run, and the profiler logs that it did not.
- *Heap:* `HeapAlloc` after `runtime.GC()`, against a threshold the fixture sets per test
  with slack. Skipped under `-race`, which changes it.
- *On failure:* goroutine, heap and allocation profiles go to `t.ArtifactDir()`, which `dt`
  collects with the cell's results.

---

## 4. Target tree

```text
pkg/sim/                         TestClock, Fault, FaultAction, Schedule        (2.2)
                                 node-wide (RFC 8 §2.1); imports nothing of the journal
  clock.go  fault.go  schedule.go  *_test.go
pkg/block/journalv2/
  internal/core/settings.go      unexported segment-cap field, SetSegmentCap    (2.5a, D8)
  journalv2test/                 test components, inside the journal's tree
    seam.go  hold.go             crash-simulating seam, holds                    (2.1, 2.2)
    prof.go                      profiler                                        (2.3)
    model/                       reference model                                 (2.4)
    fixture.go                   journal on a temporary directory, two shares    (2.5a)
    raw/                         RFC 28 bytes written by hand                    (2.13a)
  conformance/                   //go:build journal_conformance
    rfc1_a_test.go  rfc1_b_test.go  rfc1_c_test.go  rfc1_d_test.go
    rfc28_a_test.go  rfc28_b_test.go  rfc28_c_test.go  damage_test.go
    edge_test.go  model_test.go  counted_test.go  clock_test.go
    testdata/model/              hand-written sequences and shrunk failures      (2.6)
test/journalv2/
  known.yaml                     one entry per conformance test                  (2.5a onwards)
  trace.yaml  trace_test.go      RFC row → test or owner                         (2.14)
test/suites.yaml                 journal-conformance suite, journal group, vet tag (2.5a)
```

`new-package-tests.sh` requires a `_test.go` in every new directory. Each one above has its
own tests; `conformance/` is the tests.

---

## 5. Subtasks

Each subtask lists:

- **Owner**, and **Depends on**: the subtasks that must land first;
- **Deliverables**;
- **Done when**: the acceptance criteria;
- **Rules**: how each applicable general rule is met. A rule the subtask does not mention
  falls under §2.1's reading for this epic.

Every conformance subtask also:

- adds one entry per test to `known.yaml` (D1) and to `trace.yaml` (D3);
- asserts each refusal against its named sentinel or typed error (D4);
- runs every test at least on two files across two append streams and, where the check
  involves shares, two shares on one journal (RFC 1 §9.3), through the fixture's defaults;
- simulates storage loss only at the seam, never by killing the process (RFC 1 §1.3).

### 2.1 Crash-simulating storage seam

- **Owner:** A. **Depends on:** epic 1's storage seam (`storage.FS`, `storage.File`).
- **Deliverables:** in `journalv2test`, a `storage.FS` over a real directory that:
  - records, per file, every write, allocation and truncate since that file's last sync,
    and every create, rename and remove since the last directory sync;
  - on `Crash()`, discards exactly those: a file allocated and never synced comes back
    short or unallocated, an unsynced create disappears, an unsynced rename or remove is
    undone. It then closes every handle, so the journal can be reopened on the same
    directory;
  - supports the variants RFC 28 §4.4 names: *lost writes* (the default), *torn* (the
    last unsynced write kept up to byte n), and *reordered* (unsynced writes kept in a
    seeded order other than the one issued, where the filesystem allows it);
  - counts calls per operation and per file, and exposes the durable image of a file
    (what a crash would leave) for stale-read checks;
  - can serve reads of a named file from its durable image instead of the page cache
    (`StaleReads(name)`), which the "re-append trusts no page cache" checks need.
- **Done when:**
  - its unit tests, on a real temporary directory, show each discard rule: a synced write
    survives a crash, an unsynced one does not, a renamed file's directory entry survives
    only after `SyncDir`, a torn write keeps exactly n bytes, and a reordering never
    produces an order the filesystem cannot;
  - each test reverts the matching rule in the seam and watches the test fail.
- **Rules:**
  - 1, 10: the seam is fully implemented and its tests can fail.
  - 8: wraps a real directory; nothing is in memory but the record of unsynced operations.
  - 12: its first consumer within the epic is 2.2's fault wiring; its tests use it at once.

### 2.2 Fault, schedule and the test clock

- **Owner:** A. **Depends on:** 2.1.
- **Deliverables:**
  - **`pkg/sim`:**
    - `TestClock`: `Now`, `Elapsed`, `After` (the journal's `Clock`), plus `Advance(d)`
      and `SetRate(r)` from RFC 8 §2.1, and `Jump(d)`, which steps the wall clock alone:
      `Now` moves by d, `Elapsed` does not, and no timer fires. `After` fires only on
      `Advance`. `Jump` is beyond RFC 8 §2.1's `TestClock`, and is added there with the
      first check outside the journal that needs it;
    - `Fault`, `FaultAction`, `FaultKind` (D5);
    - `Schedule`: seeded, rule-driven, implementing `Fault`; `Delay` waits on the
      schedule's clock.
  - **The seam consults its `Fault`** before every operation and applies the action:
    `Fail` returns the error (`EIO`, `ENOSPC` or any given), `Drop` reports success and
    discards the effect, `Tear` keeps the first n bytes, `Delay` waits, `Reorder` marks the
    write for 2.1's reordering.
  - **Holds** (D6): `Hold`, `Reached`, `Waiting`, `Release`.
- **Done when:**
  - the same seed and rules give the same actions over the same calls, and a different
    seed gives a different sequence;
  - `After` never fires without `Advance`, and with `SetRate(1.05)` an `Advance` of 100 s
    fires timers due by 105 s;
  - a `Jump`, forward or back, moves `Now` by exactly its argument, leaves `Elapsed`
    where it was, and fires no timer;
  - each action kind is observable through the seam on a real file, including a failed
    `sync`, a failed `sync_dir` and a failed `read_direct`, the three RFC 1 §1.3 names;
  - a held call blocks until `Release`, and `Waiting` counts the blocked calls.
- **Rules:**
  - 1, 10: each property above is a test that a plausible bug fails.
  - 9: `decision:` markers for D5 at `FaultAction` and for D6 at `Hold`.
  - 12: `TestClock` lands with `Delay`, its first consumer; `Fault` with the seam.
  - `pkg/sim` is node-wide (RFC 8 §2.1) and imports nothing of the journal, so a component
    outside it (open state, which keeps a private fake clock today) takes `TestClock`
    as it is, with no wait on the rest of this epic.

### 2.3 Profiling harness

- **Owner:** A. **Depends on:** nothing.
- **Deliverables:** in `journalv2test`, `prof.Watch(t, opts)` registering a cleanup that
  checks goroutines, descriptors and heap growth, and writes profiles on failure (D12).
- **Done when:**
  - a planted leaked goroutine, a planted open descriptor and a planted retained 64 MiB
    buffer each fail it, with the profile written to `t.ArtifactDir()`;
  - a test that leaks nothing passes it 100 times in a row under `-race` (the heap check
    off) and without (on), so it does not flake.
- **Rules:**
  - 4: this subtask is the profiling harness. The allocation assertion is not here (§1.2).
  - 9: `decision:` marker for D12's real-time retry.
  - 12: its consumer is the fixture (2.5a); its tests exercise it on planted leaks.

### 2.4 Reference model

- **Owner:** B. **Depends on:** epic 1's value types.
- **Deliverables:** `journalv2test/model`, a pure in-memory model of RFC 1 §2–§3 as the
  journal's caller sees it:
  - per share tag, per file: each byte held or absent, its version, its offloaded bit,
    and the removal markers with their versions and settled state;
  - the operations the model-based test drives: `WriteAt`, `Fill`, `Offload` and its
    report, `Release`, `Truncate`, `Deallocate`, `Delete`, `Settle`, `Since`, `Repack`
    (a no-op on content), `Crash` with the set of operations a returned `Sync` covered,
    and `Reopen`;
  - after a crash, the model holds exactly what RFC 28 §4.4 allows: everything a returned
    `Sync` covered, and for every other write either all of it or none of it, as a set of
    acceptable states the comparison accepts;
  - `Compare(j)`: reads every file of the model through a journal handle and reports the
    first difference in bytes, `missing` extents, offloaded bits or `Since` output.
- **Done when:**
  - table tests cover each operation's effect, written from RFC 1's text, including
    "fill below a removal's version is excluded" and "a release keeps the bytes absent";
  - a fuzz target drives random sequences through the model alone, never panicking and
    never reporting bytes it was not given.
- **Rules:**
  - 1, 2, 10: table tests and the fuzz target.
  - 8: the model is the expected answer only; it never stands in for storage.
  - 12: `Compare` lands here with its first caller in 2.6.

### 2.5a Fixture and suite registration

- **Owner:** B. **Depends on:** 2.1, 2.2, 2.3, epic 1.
- **Deliverables:**
  - **The fixture**, `journalv2test.New(t)`: a temporary directory on the real
    filesystem, the seam with an empty schedule, a `TestClock`, the profiler, `Init` and
    `Open` with `WithStorage`, `WithClock` and settings of two append streams and a 1 MiB
    segment cap (D8). It attaches two shares on request and offers `Crash()` (seam crash,
    then reopen with the recorded identity and floor), `Reopen()`, `Advance`, `Jump` and
    `Now`. Every failure of its own starts with `fixture:`, and its assertion helpers
    report a stub as `stub: <call>` (D4).
  - **`internal/core`:** the unexported segment-cap field and `SetSegmentCap` (D8).
  - **Registration** (D1, D2): the `journal_conformance` tag, the `journal-conformance`
    suite and `journal` group in `test/suites.yaml`, the tag on `lint-go`'s vet line, and
    `test/journalv2/known.yaml`.
- **Done when:**
  - `dt run journal-conformance` is green, with every test graded `known`;
  - removing any one entry from `known.yaml` makes the run fail with that test as new;
  - a fixture test that does not open a journal (the seam and clock wiring) is green.
- **Rules:**
  - 4: every test runs under the profiler through the fixture.
  - 6: the known list exists from this subtask on.
  - 11: no exported symbol is added to `journalv2` (D8).
  - 12: the fixture and the suite land with their first tagged tests, 2.5b's (one PR with
    2.5b is fine); 2.7–2.13b depend on this subtask only.

### 2.5b RFC 1 Group D and the clock check

- **Owner:** B. **Depends on:** 2.5a.
- **Deliverables:**
  - **RFC 1 §9.4 Group D**, eleven checks: wedge is visible, accounting follows the cap
    (on the test host's filesystem; the speculative-preallocation leg is parked, §6),
    generation never repeats, stats are cheap (the part the public surface can show:
    `Stats` returns while a write is held at the seam, and its cost is flat from 10³ to
    10⁵ held extents), named inequalities hold, backlog age, clean bytes count as
    occupancy, metrics are reachable, loss events, segment held-bytes (against a walk of
    `Inspect().All()`), verification reports.
  - **The clock check** (RFC 8 §15's "one clock", the journal's half, through the
    fixture): the wall clock stepped back with `Jump` delays no timer, so the bounded
    sync still fires within its bound of monotonic time. A forward step proves nothing,
    since a journal may sync early. The journal reads time only through the clock it is
    given and sizes every wait on `Elapsed`; a journal that timed a wait by `Now` fails.
  - **Not here: a clock 5% fast.** With the journal's clock fast, its timers fire early by
    any other clock, so the bounded sync can only meet its bound and the check cannot
    fail (rule 10). RFC 0 §1.4's clock bound is about a wait that must outlast another
    clock's timeout, and the journal has none; the node-level check runs in epic 11 (§6).
- **Done when:** as 2.7.
- **Rules:**
  - 3: Group D asserts the observability contract from `Stats` only.
  - 10: the clock check fails on a journal that arms its sync timer from `Now`.

### 2.6 Model-based test

- **Owner:** B. **Depends on:** 2.4, 2.5a.
- **Deliverables:** `TestModel`: random sequences of write, fill, offload, release,
  truncate, deallocate, delete, settle, repack, crash and reopen, over at least two files
  on two streams and two shares on one journal, compared with the model after every step
  (RFC 1 §9.1, §9.3). Crashes use the seam's three variants. Seeds, shrinking and the
  corpus follow D7.
- **Done when:**
  - the corpus in `testdata/model/` replays before any random seed;
  - a planted model divergence (the journal wrapper reporting one wrong byte) is caught
    and shrunk to a sequence of at most a few steps, and the corpus file is written to
    `t.ArtifactDir()`;
  - the test is listed as a known failure.
- **Rules:**
  - 2: the operation-sequence fuzzing of rule 2 is this test, seeded.
  - 9: `decision:` marker for D7 where the seed is chosen.
  - 10: the planted divergence shows it can fail on its assertion.

### 2.7 RFC 1 Group A, part 1: read, fill, offload, release and removals

- **Owner:** B. **Depends on:** 2.5a.
- **Deliverables:** seventeen checks of RFC 1 §9.4 Group A: no zero-fill, write during
  offload, offer stops at `through` with per-extent stamps, lost stamp is offered, fill
  safety, a read's `asOf` against a change in flight, fill sets the bit, refusal,
  partial release keeps the record whole
  (by catalog and by scan), release frees nothing inside a segment, unlink under read,
  removals survive to `Since`, no resurrection, removal during offload, neighbours are
  frozen, offered bytes verified, no pin on a segment under repack.
- **Done when:** each test performs the RFC row's steps in order and asserts each outcome
  the row names. Corruption is introduced through the seam's durable image.
- **Rules:**
  - 5: the checks observe the disk only through the seam's call counts and images, never
    through internal packages.
  - 8: every "crash and reopen" is the seam's.

### 2.8 RFC 1 Group A, part 2: repack, sync windows and losses; counted sync checks

- **Owner:** A. **Depends on:** 2.5a.
- **Deliverables:**
  - nineteen checks of RFC 1 §9.4 Group A: repack copies the live part, crash mid-repack,
    repack under read, removal outlives its segment, removal records carried forward,
    copy loses to a newer write, loss raises the loss generation, a recycled segment keeps
    its new records, failed sync window, repack verifies the
    whole source, retained extents survive repack, removal dropped by version, drops
    decided after the pass's copies, carried release keeps its sequence, re-append trusts
    no page cache, loss by exact record, a synced loss masks what it superseded, a failed
    window is reported once, a re-append keeps its numbers;
  - the counted checks of RFC 1 §9.7 that need only the seam: **syncs per write** (16
    writers held at the seam until all wait, at most one sync per four writes) and
    **mixed durability** (bytes each small writer's sync waited for stay within one
    sync's interval of the streaming writer's appends).
- **Done when:** as 2.7. The counted checks use holds (D6), never timing.
- **Rules:**
  - 4: the counted checks are rule 4's for the sync path; they assert counts, not time.
  - 8: failed syncs and stale reads come from the seam on a real directory.

### 2.9 RFC 1 Group B, part 1: capacity, refusals and versions

- **Owner:** B. **Depends on:** 2.5a.
- **Deliverables:** fifteen checks of RFC 1 §9.4 Group B: reservation, reserve ahead,
  handle close, named refusal, share limit, repack at capacity, repack beside a share at
  its limit, headroom is allocated, offloaded records below half the headroom, records
  without bytes at the limit, no version repeats, floor buckets, segment and record
  accounting, freed means freed, storage returned.
- **Done when:** as 2.7. "No I/O through the storage seam" is asserted from the seam's
  call counts; "returns without waiting" with a hold that is never released.
- **Rules:**
  - 10: each refusal is asserted with `errors.As` on `NoSpaceError` and its `Limit`, so a
    refusal naming the wrong limit fails.

### 2.10 RFC 1 Group B, part 2: reclamation, bounds and concurrency

- **Owner:** B. **Depends on:** 2.5a.
- **Deliverables:** thirteen checks of RFC 1 §9.4 Group B: descriptors, coalescing, index
  bound refuses, pinned segments bounded, pins bounded across offers, a failed window does
  not wedge, sync failure, reclaim not starved, removals do not wait on offload, no
  deadlock (the public half: concurrent readers, writers, fills, repack and release finish
  under `-race` with no call blocked past a bound of virtual time), background joined,
  removal records dropped per extent, repack aimed at a share.
- **Not here:** "bounds" (comparison counts per lookup, epic 4) and the lock-order recorder
  half of "no deadlock" (epic 8). Both need internal instrumentation (§6).
- **Done when:** as 2.7. Checks at 10⁴–10⁶ extents or 1,000 segments use the 1 MiB cap
  and say so in their doc comment.
- **Rules:**
  - 4: the descriptor bound is read from `Stats` and from the profiler's descriptor count.
  - 5: concurrency runs under `-race`.

### 2.11 RFC 1 Group C: recovery

- **Owner:** A. **Depends on:** 2.5a; 2.13a's `raw` for the planted records.
- **Deliverables:** eighteen checks of RFC 1 §9.4 Group C: reseed after repack, release
  survives restart, release record lost, fill beats an older write, stale `Fill`,
  incremental report, epoch half is zero (a record with a non-zero epoch planted with
  `raw`), monotonicity (including shuffled segment order, by renaming segment files at the
  seam before reopen), attached share raises the counter, unclaimed tags are the caller's
  to name, a cancelled call changes nothing or completes (the second leg cancels a
  `WriteAt` held at the seam after its append), restamp after a lost store, forget leaves nothing to settle, unclaimed tag
  reported, a file is its share's (the planted record under a second tag built with
  `raw`), clone target survives a crash, header-only records are never failed, a loss
  survives restart.
- **Not here:** "stranded tag salvaged after serving" needs the store, a recovery import
  and the master key: epic 11 (§6).
- **Done when:** as 2.7.
- **Rules:**
  - 8: every crash is the seam's; planted bytes are written to the real directory.

### 2.12 Edge cases

- **Owner:** A. **Depends on:** 2.5a.
- **Deliverables:** one test per edge case of RFC 1 §9.2 and RFC 28 §4.1:
  - **extent shape:** a zero-length write; a write near the largest offset; a three-way
    split; an overwrite over several whole extents and parts of two; one extent overwritten
    many times with flat held bytes; a write across a segment boundary; a record exactly
    filling a segment; a file whose first held byte is far from zero; a write larger than
    the rest of its segment and one larger than a whole segment, with a crash between the
    pieces;
  - **lifecycle:** truncate inside an extent, to zero and write again, down and back up;
    deallocate inside and across extents; `Fill` over held, dirty and absent parts; an
    `Offload` whose `fn` reports extents it was not offered, or none; a partly offloaded
    `Release`; releasing the middle, one end, and every part in separate calls; a segment
    whose records are all released becoming reclaimable without a read;
  - **capacity and storage:** a reservation of exactly the remaining space and one byte
    over, against the share and the journal; `ENOSPC` from the filesystem below the
    journal's maximum, distinct from `ErrNoSpace`; a sync that fails then succeeds on the
    same stream; truncate, delete, release, mark and seal at the maximum; one segment
    retired by eviction and by repack, footprint falling once and never below zero;
  - **opening:** an empty directory, one holding only a `format` file, one from an older
    format, and `Init` on each.
- **Done when:** as 2.7. Each test's name says its case (`TestEdge_ThreeWaySplit`).
- **Rules:**
  - 10: each case asserts the outcome its RFC section defines, not merely "no error".

### 2.13a RFC 28 bytes by hand: `raw`

- **Owner:** A. **Depends on:** epic 1 only; it needs neither the fixture nor the seam.
- **Deliverables:** **`journalv2test/raw`** (D9), with tests that its bytes match the
  RFC 28 §2 tables field by field (offsets, magics, sizes, checksum coverage).
- **Done when:** `raw`'s tests are green and fail on a wrong offset, size or checksum
  range.
- **Rules:**
  - 1, 10: `raw`'s tests fail on a wrong offset or checksum range.
  - 7: `raw` is written from RFC 28, not from v1's segment code.
  - 12: its first consumer is its own tests, a check in their own right, since epic 3's
    golden vectors are compared with them; 2.11 and 2.13b follow.

### 2.13b RFC 28 checks and systematic damage

- **Owner:** A. **Depends on:** 2.5a, 2.13a.
- **Deliverables:**
  - **RFC 28 §4.2 Group A**, nine checks: corrupt dirty, corrupt offloaded, directory
    entry survives, footer is not authoritative, unmark clears, write carries its mtime,
    unproven tail verified at open, failed directory sync is a failed window, a zero slot
    is not the end. (Directory flush on NTFS is epic 3's.)
  - **Group B**: segment reclaimable.
  - **Group C**, the 22 rows that need no encoder, plus the refusal halves of three
    split rows (D9): rebuild, sources agree, synced damage is not a torn tail, bits
    survive restart, reseed by version, trailer torn, truncated sealed segment, foreign
    segment, version floor, stale extent, unidentified directory, journal gone, spares are
    re-adopted, a spare is whole or absent, retired segments are recycled, a recycled
    segment's old catalog is never read, interrupted `Init`, unknown version, neighbours
    survive, a scan resynchronises, torn tail cut off, a bad copy does not mask a good one;
    and the refusal halves of kind values (kinds 15 and 128 fail the open), segment header
    and seal marker (a foreign header is foreign, not damaged; a seal marker copied into a
    payload does not stop a scan), and `format` file (55 or 57 bytes, a bad checksum and
    version 2 fail the open; a crash on either side of the `format.tmp` rename leaves old
    or new whole). "Record layout" and "trailer" are encoder goldens, epic 3's.
  - **RFC 28 §4.4, injected systematically:** a bit flipped in turn in every region (record
    header and payload, catalog entry, footer, trailer, segment header, seal marker,
    `format`), each with its required outcome; lost writes at every sync point of every
    operation that syncs; torn writes cut at every byte of the final record and of the
    trailer; reordered unsynced writes. After each, the reopened journal is compared with
    the reference model. Plus the table of edits from outside, one test per row.
- **Done when:** as 2.7.
- **Rules:**
  - 8: every flipped bit and every crash is applied to a real file.

### 2.14 Traceability

- **Owner:** B. **Depends on:** 2.5a; filled as 2.5b–2.13b land, finished after them.
- **Deliverables:** `test/journalv2/trace.yaml` with every row of D3's scope, and
  `test/journalv2/trace_test.go` (D3).
- **Done when:**
  - every in-scope row maps to a test, to an owner, or to both, and every suite test,
    `TestEdge_*`, `TestModel`, `TestCounted_*`, `TestDamage_*` and `TestClock_*` included,
    maps to a row;
  - deleting a row's entry, renaming a mapped test, adding a conformance test with no
    entry, or adding a bullet heading to RFC 1 §9.2 each fails `trace_test.go`.
- **Rules:**
  - 1, 10: the check fails on each of the three plants above.
  - 6: the map and the known list are kept together: a test leaving `known.yaml` stays in
    `trace.yaml`.

### 2.15 Review and S2

- **Owner:** the team, with the filesystem service's owner for `pkg/sim` (D5). **Depends
  on:** 2.1–2.14.
- **Deliverables:** the joint review. It walks each conformance test against its RFC row
  and checks the review rules: refusals asserted against named sentinels (D4), no
  `t.Skip`, no panic, no real-time wait outside the profiler's teardown (D12), every
  check on two files, two streams and, where relevant, two shares, no clock reached but
  the fixture's (D10), and, once #3391 has landed, a `message:` on every `known.yaml`
  entry (D1). It freezes `FaultAction`, its kinds and the seam's operation names (D5).
- **Done when:**
  - `dt run journal` is green on `develop`'s CI, with every conformance test graded
    `known`;
  - `trace_test.go` and the components' tests are green in `unit`;
  - the team has approved, and S2 is reached.
- **Rules:**
  - 6: from S2 on, each implementation epic removes the entries it turns green.

---

## 6. Rows owned outside this epic

Every other row in D3's scope lands as a test in this epic (§5). These do not, and
`trace.yaml` names their owner:

| Row | Owner | Why |
| --- | --- | --- |
| RFC 1 B "bounds" | epic 4 | comparison counts per lookup need the index's counters |
| RFC 1 B "no deadlock", lock-order half | epic 8 | the lock-order recorder is internal to the data path |
| RFC 1 C "stranded tag salvaged after serving" | epic 11 | needs the store, a recovery import, `salvage.path` and the master key |
| RFC 1 D "accounting follows the cap", speculative-preallocation leg | parked | needs a filesystem host nobody provisions (XFS); the leg on the test host's filesystem is here |
| RFC 28 A "directory flush on NTFS" | epic 3 | real NTFS and a power cut |
| RFC 28 C "record layout", "trailer" | epic 3 | golden vectors of the encoders |
| RFC 28 C "kind values", "segment header and seal marker", "`format` file", encoding halves | epic 3 | golden vectors of the encoders, compared with `raw` (D9) |
| RFC 1 §9.7 "index cost", "index memory" | epic 4 | the index's comparison counters and its bytes per entry |
| RFC 1 §9.7 "allocations per operation" | epic 8 | needs a hot path; a threshold cannot be set against a stub (§1.2) |
| RFC 1 §9.7 "report cost" | epic 9 | needs offload over the index's counters |
| RFC 8 §15 "one clock", node half | epic 11 | two `TestClock`s across every component of a node; the journal's half is 2.5b |
| RFC 0 §1.4 "clock bound" | epic 11 | a wait on another clock's timeout; the journal has none (2.5b) |

---

## 7. System scenario catalogue

These cover what only the composed system shows. **Epic 10 writes the start-check and
initialisation rows; epic 11 writes the rest**, each listed as a known failure in its
suite's known file until it passes. The journal-specific fixtures land with the first
test that uses them (D11).

Under route B of #3119, a `dfs` serving NFS or SMB on journal v2 needs more than epic 11:
the filesystem service, `vfs.Filesystem` (stage 2, RFC 17), and the adapters moved onto it
(RFC 17 §9's slices, stage 5). The catalogue is therefore split by the tier each row can
run at, and the dependencies are named per tier. Who writes the integration-tier rows,
the journal team or the filesystem service's owner, is agreed at S4a.

### 7.1 At the filesystem service's integration tier

`vfs.Filesystem` over the real engine on a temporary journal, the in-memory store, a fake
remote tier and the fake clock, with no node and no kernel client (RFC 17 §8, the
integration tier). **Depends on:** epic 11 and #3119 stage 2's `vfs.New` with its
integration harness (RFC 17 §9, slice 1).

- **Several shares, one journal per device** (RFC 15 §2.2, §2.4; RFC 8 §2.3): N devices
  give N independent journals; shares on one device keep their own limits, so one share at
  its limit leaves the other writing; corruption or loss of one device affects only its
  shares.
- **Capacity:** a journal filled past its maximum gives retry-later with no data loss; a
  full underlying filesystem gives `ENOSPC`, distinct from the journal's own refusal.
- **Cold reads:** evict, restart, read: bytes come back from the remote tier, never zeros;
  a read of a truly lost extent fails, never returning zeros (RFC 0 I1).
- **Lifecycle:** share create, remove and re-create on one device, with no resurrection
  and a new tag; share removal forgets its tag; restart with a removal pending.

### 7.2 A real `dfs` with kernel clients

Under `test/e2e`, a real `dfs` on journal v2 with kernel NFS and SMB clients.
**Depends on:** epic 11, #3119 stage 2 (`vfs.Filesystem`) and the adapters moved onto it. The
write verifier is the filesystem service's (RFC 17 §5.8), from the engine's loss
generation, so the durability rows need RFC 17 §9's slice 4 (`Write`, `Read`, `Commit`
and the verifier) as well. Epic 10's rows need only a `dfs` that starts.

- **Durability through the protocols:** NFS COMMIT and SMB flush survive a host crash
  (simulated at the seam or in a VM); an unflushed write survives process death; under
  `bounded`, unflushed data is lost by at most the bound on power loss; a failed sync
  changes the NFS write verifier, so the client resends.
- **Reference workloads** (the RFC index): an FSLogix-style 30 GB container with random
  overwrites behind one long-lived SMB handle; a sign-in storm of concurrent cold opens;
  sign-out compaction, a single large truncate; read-mostly ingestion of about 7,000 small
  files.
- **Process limits:** the descriptor bound under `ulimit -n`.
- **Start and devices:** start is refused with `storage` but no journal devices; a disk
  moved to another installation, or a journal directory swapped, refused by `expect` and
  the installation ID.
- **Start and initialisation (epic 10):** each row of the start check (normal start, the
  two refusals with their messages); a refused start writes nothing, leaving stores,
  devices and any v1 state byte-identical; a crash between journal init and the store's
  init, after which a start reports the store lost and a re-run of initialisation wipes
  the devices and succeeds; a crash after the store's init, after which the next start
  completes the journal records.
- **Process death against power loss:** `SIGKILL` and a power cut kept distinct, and a
  long soak with capacity below the data written (the soak itself is epic 12's).

### 7.3 The `Content` conformance check

One check that both a fake `Content` and the engine pass, so the filesystem service's unit
tier (RFC 17 §8, a fake engine) tests against the same contract the engine meets. It is
not this epic's, which tests the journal alone; it is a named deliverable of epic 11,
agreed with its shape at S4a.

---

## 8. Order of work

```mermaid
flowchart TD
  S1(["S1 interfaces frozen"]):::sync
  subgraph A["Track A — storage, @viquitorreis"]
    T1["2.1 crash seam"] --> T2["2.2 fault, schedule, clock"]
    T3["2.3 profiler"]
    T8["2.8 Group A part 2, counted syncs"]
    T11["2.11 RFC 1 Group C"]
    T12["2.12 edge cases"]
    T13a["2.13a raw"]
    T13["2.13b RFC 28 checks, damage"]
  end
  subgraph B["Track B — model and operations, @didimelli"]
    T4["2.4 reference model"]
    T5["2.5a fixture, registration"]
    T5b["2.5b Group D, clock"]
    T6["2.6 model-based test"]
    T7["2.7 Group A part 1"]
    T9["2.9 Group B part 1"]
    T10["2.10 Group B part 2"]
    T14["2.14 traceability"]
  end
  S1 --> T1 & T3 & T4 & T13a
  T2 & T3 --> T5
  T4 & T5 --> T6
  T5 --> T5b & T7 & T8 & T9 & T10 & T12 & T13
  T13a --> T11 & T13
  T5 --> T11
  T5b & T6 & T7 & T8 & T9 & T10 & T11 & T12 & T13 --> T14
  T14 --> R["2.15 review"]:::sync --> S2(["S2 red suite in CI"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

**Parallelism.** Track A starts with 2.1 → 2.2, 2.3 and 2.13a; track B with 2.4. They
meet at 2.5a, which track B writes on track A's components. After 2.5a the conformance
subtasks are independent of one another, except that 2.11 and 2.13b take `raw` from
2.13a, which needs nothing but epic 1. Track B closes with 2.14; the team meets at 2.15.

**Sizes.** Relative, from the number of checks and of new components, and recalibrated at
2.15: **S** is one component or up to ten checks, **M** a component with crash semantics
or eleven to twenty checks, **L** more than twenty checks or a systematic sweep.

| Subtask | Size | Basis |
| --- | --- | --- |
| 2.1 crash seam | M | lost, torn and reordered crashes on a real directory |
| 2.2 fault, schedule, clock | M | three components, holds |
| 2.3 profiler | S | one component |
| 2.4 reference model | M | every operation, crash states, a fuzz target |
| 2.5a fixture, registration | S | the fixture, the suite, CI wiring |
| 2.5b Group D, clock | M | twelve checks |
| 2.6 model-based test | M | one test, shrinking and the corpus |
| 2.7 Group A part 1 | M | seventeen checks |
| 2.8 Group A part 2, counted | M | nineteen checks, two counted checks |
| 2.9 Group B part 1 | M | fifteen checks |
| 2.10 Group B part 2 | M | thirteen checks, some at 10⁶ extents |
| 2.11 Group C | M | eighteen checks |
| 2.12 edge cases | L | about thirty cases |
| 2.13a `raw` | S | byte builders and their field tests |
| 2.13b RFC 28 checks, damage | L | thirty-five checks and the systematic sweeps of RFC 28 §4.4 |
| 2.14 traceability | S | one map, one parser of the RFC sections |
| 2.15 review | M | every test against its row |

**Allowed imports.** Arrows point at the imported package.

```text
pkg/sim        ──► standard library
journalv2test  ──► journalv2, internal/core, internal/storage, pkg/sim
conformance    ──► journalv2, journalv2test, journalv2test/model, journalv2test/raw,
                   pkg/sim (fault rules only; the clock through the fixture, D10)
test/journalv2 ──► standard library (reads the RFCs and the conformance sources)
```

---

## 9. Open points

- **Epic 3 and the planted record.** "A file is its share's" plants its second-tag record
  with `raw` here, so epic 3 does not write that check; it only adds the encoder halves of
  §6.
- **Which version the "unknown version" row means.** RFC 28 §4.2, row "unknown version":
  "Write a trailer with a future format version". The trailer's field is the "catalog
  layout version, 1 for this layout; independent of the `format` file's" (RFC 28 §2.5),
  while an unknown segment header version fails the open (RFC 28 §2.2). 2.13b's row test
  plants different bytes under each reading. **Decides:** RFC 28 owner.
- **Suite budget on PRs.** The suite runs on `pr` from S2. Against stubs it is fast; once
  checks at 10⁶ extents pass, its duration is measured with `dt history`, and epic 12
  moves the heavy checks to `merge` or `nightly` with expected-skip entries if the `pr`
  budget needs it.
- **The `e2e` known file.** The system tests of epics 10 and 11 need the `e2e` suite to
  have opted in to a known file (RFC 29 §9). If it has not by then, epic 10 creates it.
