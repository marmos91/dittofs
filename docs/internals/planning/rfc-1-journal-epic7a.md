# Epic 7a: journal v2 recovery and identity, reopening a journal

**Sources:** [RFC 28](../rfcs/rfc-28-journal-format.md) §2.1 (what open reads of the `format`
file and the directory), §2.2 (the `.spare.tmp` an open unlinks, the fresh id above every id the
directory names, a recycled segment's old life and a failed append's slot as an open finds them),
§2.3 (the kinds and coverage rules a rebuild reads), §2.4 and §2.5 (reading a catalog), §3.1,
§3.3 (torn and corrupt records, the seal marker proven on the device, the scope of the refusal,
the sync marks, which ranges name no record, and a read that fails, as far as an open meets
them), §3.4 (the open's half), §4.2 and §4.4 (the rows that reopen a journal);
[RFC 1](../rfcs/rfc-1-journal.md) §3 (`Open`, its `floor`, `expect` and `marks`, `SyncMark`,
`*UnreadableRangesError`, the context rule for `Open`, a file's tag rebuilt from its records,
forget records applied at recovery), §3.7 (`Recovery`, `RejectedSegments`, `DamagedSegments`,
`SyncMarks`, `TagConflicts`, `Shares`, `Files` and `DirtyFiles` rebuilt), §3.8 (loss records at
open), §3.9 (`torn_tails`, `scan_resyncs`), §3.10 (markers and synced losses unsettled after a
restart), §4.2 (an index over its bound at open), §4.3 (the counters at recovery, the epoch half
and the largest values, any segment order), §5.4 (the failed records an open drops, a reopen in
place), §6 (the open reserve and the deferral past it), §7.1 (the torn-tail exception), and the
rows of §9.4 Groups C and D that reopen a journal.
**Track:** A.
**Epic 7a estimate: 15 subtasks, 53.25 man-days.**
**Depends on:** sync point **S3, the walking skeleton**; E3's `format` file, segment header, seal
marker, record and catalog codecs, its name classifier (3.2), its slot scanner with
resynchronisation (3.6) and the platform layer whose `ReadAtDirect` reads through the page cache
until this epic (3.7); E4's rebuild builder (E4-11), which takes records in any order and
concurrently, applies precedence, markers, floor buckets, drop ranges, offloaded and unmark
records and fills, and rebuilds a file under two tags as rivalled; E5's segment set and footprint
ledger (T5.1), descriptor cache (T5.2), spare pool with `Adopt` (T5.5), seal writer that takes its
entries as data (T5.7), end of a segment (T5.10), retirement (T5.9) and its id allocator seeded
at open; E6's `Durable` and `WaitDurable` (T6.1), loss records with `Lose`, its `AtOpen` and
`Deferred` classes, the ring with `Place(found at open)` and the per-directory registry of the
generation and the failed sequence numbers (T6.2), the failed segment (T6.3), the read-only state
(T6.6) and the corruption handler (T6.7); E8's `datapath.New(deps, state)` and the minimal `Init`
and `Open` it extends (T8.15), the conflict flag (T8.4), the open reserve's admission class
(T8.6), the dirty-file list (T8.9) and `Stats` aggregation (T8.13); E9a's liveness tracker with
`Held` and `Droppable` (T9.1), which the rebuild fills and the emptying asks.
**Blocks:** E9a (its T9.1 tracker starts from what T7.6 registers, and its crash tests reopen
through this `Open` once every crash reopens, after T7.11), E10 (the start opens every journal
through this `Open`), the engine's start of RFC 8 §2.5 (opening each journal with its floor,
`expect` and marks, then `Since` replay over what this epic rebuilt), E7b (which extends this
`Open`), and E12 (every conformance row that reopens a journal).

---

## 1. Goal

Epic 7a makes a journal that has run open again. A directory a process wrote — closed cleanly,
killed, cut off by a power loss at the storage seam, or turned read-only and reopened in place by
the same process — reopens through `Open(ctx, dir, floor, expect, marks)` to the index its
segments support, with every write a returned `Sync` covered held and reading its bytes, nothing
served that the segments do not hold or that a lost record may have superseded, and the counters
above every number on disk, every sync mark and the floor. It is the part of recovery the
engine's start needs to serve a share after a restart, and the part a crash test exercises. It
writes no new on-disk structure: it reads what epics 3 to 6 write, and writes only what RFC 28 §3
asks an open to write (a truncation, a seal, a re-append of the tail the device lost, an ended
segment's emptying, a loss record, a spare's zeroed slots and one directory sync).

Where RFC 28 asks an open for the metadata reconcile, for holding an unread record, or for an
operator's acknowledgement, this epic stops short and refuses the open with a named error,
never serving what those paths exist to keep from being served (D13, D14, D15). E7b replaces
each refusal.

**At the end of the epic:**

- `Open` of a directory holding segments attaches every segment of this journal, reports and
  leaves alone a foreign one or a file it does not recognise, unlinks every `.spare.tmp`, fails on
  a `format` file or a segment version it does not recognise, and keeps T8.15's identity,
  `expect` and platform refusals for such a directory;
- the index is rebuilt from catalogs where they verify and by scan otherwise, in any segment
  order and concurrently, and both give the same index; the active segments are always scanned;
- a torn tail and a torn slot are never reported, synced damage always is, a scan steps over
  zeros a later verifying record follows, and a sync mark proves what lies below its `Through`;
- loss records, forget records, equal-numbered copies and the records a read-only journal failed
  earlier in the process are applied as RFC 1 §3.8, §3 and §5.4 say, and every rebuilt marker
  and synced loss starts unsettled;
- the next sequence number, segment id and content version exceed every one on disk, every sync
  mark's and the floor; a file's tag is rebuilt, and a file under two tags is refused through
  either; `Files`, `DirtyFiles`, the offloaded bits, `SyncMarks` and the per-share accounting are
  what the running journal had;
- every segment that passes as a spare is re-adopted after one directory sync, with its slots
  zeroed where a recycle left them dirty, and a segment a sync mark names never is;
- the tail no sync is proven to have reached is read through the cache, synced and re-read past
  the page cache, and what the device lost is re-appended from the first read's copy; a seal
  marker no successor names counts only once read from the device; a torn tail is cut and its
  segment sealed, and a segment that must be ended is ended and emptied into a spare before the
  journal serves;
- the open's loss records draw on the open reserve and are deferred past it, so a journal at its
  maximum opens;
- `Stats` reports `Recovery`, `RejectedSegments`, `DamagedSegments`, `SyncMarks`, `torn_tails` and
  `scan_resyncs`, and the open runs under an `open` span with a child span per phase;
- the crash checks of RFC 28 §4.4 pass over `Open`, except a flipped bit in a record header that
  leaves a range that names no record, and every conformance test this epic turns green has left
  `test/journal/known.yaml`.

### 1.1 In scope

- The open's phases, the context check between them, and the attach pass over the directory
  (T7.1).
- Rebuild by scan (T7.2) and by catalog (T7.3), fed to E4's builder, with every read at open
  retried and an unreadable one refusing the open (D15).
- Telling torn from corrupt, the damage an open reports, and finding a range that names no
  record (T7.4).
- The record kinds E4's builder leaves to recovery: loss and forget records, equal-numbered
  copies, and the records a read-only journal failed (T7.5).
- Counters above the disk and the floor, tags, the derived lists and accounting, the liveness
  registration, and the journal's assembly through E8's constructor (T7.6).
- Spare re-adoption and the open's directory sync (T7.7).
- Reads past the page cache on Linux, macOS and Windows (T7.8), the unproven tail's and an
  unnamed seal marker's proof (T7.9), and the re-append of what the device lost (T7.14).
- Sync marks at open: the counters above them, the proof below `Through`, the lost-tail check,
  and `Stats.SyncMarks` (T7.15).
- Cutting a torn tail, sealing or ending a segment, and the open's loss records on the open
  reserve or deferred past it (T7.10).
- Emptying the segments the open ends, and retiring every segment that is not held before it
  (T7.11).
- The open's statistics and span (T7.12).
- The crash and damage matrix over `Open`, and the known-failure burn-down (T7.13).

### 1.2 Out of scope

- `Init`, and `Open` of a directory holding a `format` file and no segment: E8's T8.15, which
  this epic extends (D1). `Init` makes no spare: the pool fills when the journal is assembled
  (E5's T5.5).
- `Restamp` (RFC 1 §3, RFC 28 §3.4, "A lost store") and its row "restamp after a lost store": E7b.
- `RebuildFormat`, `AcknowledgeUnreadable` and `Acknowledge` (RFC 1 §3, RFC 28 §3.3 "An operator
  acknowledges a range that never reads", §3.4): E7b. This epic returns the
  `*UnreadableRangesError` they start from (D15).
- Holding an unread record at open, the suspect segment, `SuspectSegments`, and a file unread
  until its record reads (RFC 28 §3.3 "A read that fails is not a record that does not verify"):
  E7b. This epic refuses the open on any read at open that fails (D15).
- The metadata reconcile's journal half: appending the `reconcile` record into its spare,
  sealing a segment that holds a range with no footer, `Recovery`'s reconcile-owed and counted
  fields, `reconciled` records counting a range, and the reconcile spare among the pool's
  classes (RFC 28 §3.3, RFC 1 §3.7, §6): E7b. This epic finds the ranges, refuses the open on
  them and on a verifying `reconcile` or `reconciled` record (D14), and refuses on a lost synced
  tail (D13).
- Inspection's index read-out (`File`, `All`) and report-only `Verify` with its footprint
  mismatch check (RFC 1 §4.4): E7b. The per-segment accounting, `Inspector.Segments`, is E9a's
  T9.8, since repack's segment scope reads it; this epic marks each segment it reports damaged,
  and each damaged one it empties or retires, so that read-out keeps its row until close (T7.12).
- The J6, J7 and J9 benchmarks (RFC 28 §4.3): E7b.
- Retained extents rebuilt from hold, unhold and stamp records (RFC 28 §3.1, RFC 1 §3.11): E7b.
  Nothing writes those records before E9b's holds; an open that finds one refuses (D6).
- The rows and halves that assert an open succeeds over a range that names no record, which
  pass only once the reconcile exists: RFC 28 §4.2 C "a range that names no record", "the
  reconcile stays owed", "a recovery seal holds no catalog of a range", "a counted range is not
  counted again", "a lost synced tail", "a scan resynchronises" (its corrupt length field in a
  sealed segment with no catalog), the named-predecessor half of "truncated sealed segment", and
  the RFC 28 §4.4 bit flips in a record header that leave such a range: E7b. Until then the
  open refuses each (D13, D14), and T7.4, T7.13 and T7.15 test the refusal.
- The rows that hold an unread record: RFC 28 §4.2 A "a failed read is not damage", "unproven
  tail with a failed re-read", the second half of "unreadable is not empty", "an unread file at
  start" and the acknowledgement rows: E7b. Until then the open refuses (D15).
- The offload rows that reopen a journal: RFC 28 §4.2 C "bits survive restart", "reseed by
  version" and "stale extent", and A "unmark clears", with the stale rule and `MarkOffloaded`'s
  and `Unmark`'s semantics (RFC 28 §3.2): E9b. This epic rebuilds the bits from the records on
  disk, through E4's builder (D5).
- The liveness of a header-only record and of a segment (RFC 1 §7.2): E9a's T9.1, which this
  epic fills at open (T7.6) and asks before it retires or carries anything (T7.11).
- Unlinking an orphan, a **MAY** of RFC 28 §3.4: not adopted (D2).
- Writing a footer for a segment a previous process sealed without one, a **MAY** of RFC 28 §2.4:
  not adopted (D3).
- Writing segments, seals, catalogs, spares, recycles and unlinks, and the idle seal: E5. Byte
  layouts, checksums and the slot scanner: E3. Sync, failed windows, loss records, the loss ring
  and generation, the read-only state and the corruption rule at read time: E6. This epic calls
  them.
- The caller's side of RFC 28 §3.4 and RFC 1 §3: recording an interrupted `Init`'s identity,
  a recovery import opening adopted journals first, naming unclaimed tags to `Strand` and
  `Forget`, retrying a refused reopen, recording sync marks: the engine's start (RFC 8 §2.5) and
  E10.

### 1.3 Names used here

- **E1**, epic 1, interfaces: the public types, the error set, `Stats` and the storage seam.
- **E2**, epic 2, red e2e suite and test foundations: the fixture, the reference model, the
  test clock, the fault schedule, the crash-simulating seam, the profiler and the conformance
  suite with its `known.yaml`.
- **E3**, epic 3, on-disk format and platform layer: the codecs, the name classifier (3.2), the
  slot scanner (3.6), the platform layer (3.7) and the platform checks (3.11, 3.12). Its subtasks
  are numbered 3.1 to 3.12 in its document.
- **E4**, epic 4, placement index and versions. Its subtasks are numbered E4-1 to E4-12 in its
  document; E4-11 is the order-independent rebuild builder.
- **E5**, epic 5, segments, streams and accounting.
- **E6**, epic 6, durability and loss.
- **E7a**, this epic: the part of epic 7, recovery and identity, that reopens a journal.
- **E7b**, the rest of epic 7: `Restamp`, `RebuildFormat`, `AcknowledgeUnreadable` and
  `Acknowledge`, unread records held at open, the journal half of the metadata reconcile, the
  index read-out and `Verify`, the recovery benchmarks, and retained extents from hold records.
  It has no document yet.
- **E8**, epic 8, data path and capacity: the journal's constructor, the minimal `Init` and
  `Open` (T8.15), the open reserve's admission class, `Stats` aggregation.
- **E9a**, the part of epic 9, offload, reclamation, holds and tags, that the engine without
  offload needs: repack, auto-retire, `Forget` and `Strand`, `Inspector.Segments`, and the
  liveness tracker of its T9.1, which says whether a header-only record and a segment are live.
- **E9b**, the rest of epic 9: `Offload`, `report`, pins, `MarkOffloaded`, `Unmark`, `Release`,
  the stale rule, snapshot holds and `Stamp`.
- **E10**, epic 10, start and initialisation: the start check and the device configuration.
- **E11**, epic 11, the engine on the RFC 16 store: the caller that opens each journal at
  start.
- **E12**, epic 12, e2e green: the burn-down of the known-failure list.
- **S1**, interfaces frozen: E1's exported surface no longer changes.
- **S2**, red suite in CI: E2's suite runs in CI from every pull request.
- **S3**, walking skeleton: E5's append streams with the spare pool, the seal, the roll and the
  end event (its T5.1, T5.2, T5.4a, T5.4b, T5.5, T5.6, T5.7 and T5.10), E6's T6.1 to T6.4, T6.7
  and T6.9, and E8's `WriteAt`, `ReadAt` and a minimal `Init` and `Open` of a directory with no
  segment, with `Share` for their handles (its T8.1, T8.2, T8.3 and T8.15), on top of E4's index.
- **S4**, core complete: E3 to E9 done; all component conformance tests are green.

## 2. General rules for each epic

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
   v1's journal (`pkg/block/journal` until #3645 deleted it). The import test enforced it. v1 code is neither ported nor
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

### 2.1 How the rules apply

| Rule | Subtasks | How |
| --- | --- | --- |
| 1 Unit tests | all | every R-number has a test that can fail. Subtasks use E3's codecs and scanner, E4's builder, E5's segment set, pool and seal writer, E6's `WaitDurable`, `Lose` and `Corrupt`, E8's `New` and E9a's tracker as they stand, and E2's seam, fault schedule and model. Until the checkpoint after T7.10 the public `Open` of a directory holding segments still answers T8.15's `ErrNotImplemented` once the attach pass is done, and tests drive the internal `recovery.Run` directly (§7) |
| 2 Fuzz | T7.4 | the codecs and the scanner are fuzzed by E3. This epic adds `FuzzOpen`: a small journal directory whose segment bytes the fuzzer mutates, opened through `recovery.Run`; the property is that the open either fails with a named error or yields only extents whose bytes some segment holds, never a panic. Seeded with the RFC 28 §4.4 regions |
| 3 Metrics and tracing | T7.4, T7.10, T7.12, T7.15 | `Recovery` (segments read by catalog and by scan, and how long), `RejectedSegments`, `torn_tails` and `scan_resyncs` are set as the open finds each, `SyncMarks` once the tails are proven (T7.15), and `DamagedSegments` is raised through the segment set's damage mark; `corrupt_extents` is E6's, raised by `Lose`. The `open` span wraps `Open`, with one child span per phase (attach, rebuild, tail proof, re-append, marks, cut and seal, emptying, assembly) |
| 4 Profiling | T7.2, T7.3, T7.13 | every test runs under E2's profiler: an open that fails or is cancelled leaks no descriptor or goroutine; a rebuild's peak heap is recorded at 10⁶ records outside `-race` (the builder's own `ponytail:` names its ceiling). The hot paths of rule 4 are other epics'; the open has no counted check of RFC 1 §9.7 |
| 5 Public and internals, `-race` | all | the internal `recovery` package is tested directly, and the public `Open` once the checkpoint after T7.10 enables it. The concurrent rebuild runs under `-race` (T7.2). A value this epic rebuilds is checked where it is consumed: `ReadAt`, `Since`, `Files`, `DirtyFiles`, `Stats` and the refusals of E8, not the builder's state |
| 6 Known failures | T7.10, T7.11, T7.13 | the conformance tests that reopen a journal leave `known.yaml` as the subtask that turns them green lands; T7.13 removes the rest and re-points the entries this epic does not turn green to the epic that will |
| 7 v2 ignores v1 | all | enforced by the import test. Nothing of v1's journal is read; the open never looks for v1 state (that is E10's start check) |
| 8 Real storage | T7.4, T7.7, T7.9, T7.10, T7.11, T7.13, T7.14, T7.15 | every torn, crash, unlink and damage check runs on E3's `platform.OS` through E2's crash-simulating seam, never an in-memory filesystem. Direct reads (T7.8) are checked on a real file whose page cache holds bytes the device does not |
| 9 Comment markers | T7.1, T7.2, T7.3, T7.4, T7.5, T7.7, T7.8, T7.10, T7.11, T7.14, T7.15 | the RFC's markers at their code sites: RFC 28 §3.3's two `ponytail:` notes on the failed slot and the forward search (T7.4), its `decision:` on emptying an ended segment whole (T7.11), its `ponytail:` on a segment's absence taken as its retirement (T7.15), and its two `decision:` notes on a failed read (T7.2); RFC 1 §6's `decision:` on the open reserve (T7.10) and its `ponytail:` on a loss record's bytes in a copy (T7.11). This epic's D2 (T7.1), D3 (T7.3), D4 (T7.14), D6 (T7.5), D8 (T7.2), D11 (T7.8), D12 (T7.7), D13 (T7.15), D14 (T7.2, T7.4) and D15 (T7.1, T7.2) at theirs |
| 10 A test must be able to fail | all | §6 names, per test, the change that would make it fail |
| 11 Exported only if RFC 1 §3 uses it | T7.1, T7.2, T7.15 | the S1 surface lacks four names RFC 1 §3 declares for the open: `Open`'s `marks` argument and `SyncMark` (T7.15), `SegmentRange` and `*UnreadableRangesError` wrapping `ErrUnreadable` (T7.1, T7.2), and `Stats.SyncMarks` (T7.15). This epic adds them as RFC 1 §3 writes them (§9). `Stats.Recovery`, `RejectedSegments`, `DamagedSegments`, `TagConflicts`, `TornTails` and `ScanResyncs` exist since S1. The `recovery` package is internal |
| 12 Created with its consumer | all | `internal/recovery` lands with T7.1, called by `Open`; each file lands with the subtask that uses it. `ReadAtDirect` exists since E3, reading through the page cache; T7.8 replaces its per-OS bodies, and T7.9 is the first caller that needs them |

## 3. Requirements and invariants

Every MUST and MUST NOT that RFC 28 and RFC 1 place on this epic, numbered so that subtasks and
tests can cite them. Obligations E7b, E9a or E9b carry are in §1.2 and §4, not here.

**Attaching the directory**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | `Open` never creates a journal. On a directory holding segments it keeps every refusal T8.15 makes: no `format` file is `ErrUnidentified`; an identity other than `expect` fails; `expect` zero fails naming the journal identity, the installation ID and that the directory holds segments; the platform checks run before the open writes anything | MUST | RFC 1 §3, RFC 28 §3.4 | T7.1 |
| R2 | A `format` file that does not verify, is not 56 bytes, or carries a version the binary does not recognise fails the open; it is never upgraded or repaired. A `format` file no read can read fails the open with `*UnreadableRangesError` naming `format` | MUST | RFC 28 §2.1, RFC 1 §3 | T7.1 |
| R3 | Open ignores a `format.tmp` | invariant | RFC 28 §2.1 | T7.1 |
| R4 | A file the journal does not recognise is left alone, not deleted | MUST | RFC 28 §2.1 | T7.1 |
| R5 | Open unlinks every `.spare.tmp` it finds | invariant | RFC 28 §2.2 | T7.1 |
| R6 | The id of every spare or recycle made after open is above every id the directory named at open, `.seg` and `.spare.tmp` files included, and above every sync mark's segment | invariant | RFC 28 §2.2, §3.3 | T7.1, T7.15 |
| R7 | Recovery reads only the `format` file and the segments, consults no component outside the journal, and does not require the process that wrote the segments to have exited cleanly | MUST NOT | RFC 28 §2.1, §3.1 | T7.2, T7.6 |
| R8 | A segment whose header verifies and names another journal identity contributes nothing to the index, is reported, and is left on disk untouched | MUST NOT | RFC 28 §3.1, §3.4 | T7.1, T7.7 |
| R9 | A segment whose format version the binary does not recognise, or that is newer than the `format` file's, fails the open | invariant | RFC 28 §2.2 | T7.1 |
| R10 | An orphan is never unlinked unless the journal is certain it wrote it and an age gate has passed, and a file whose name the journal could not have produced is never unlinked | MUST NOT | RFC 28 §3.4 | T7.1 |
| R11 | `Open` checks its context between recovery phases; a cancelled open returns the context's error and leaves the directory for a later `Open` to recover | invariant | RFC 1 §3 | T7.1 |

**Rebuilding the index**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R12 | On open the placement index is reconstructed, including a removal marker for every truncate, deallocate, delete and clone target record on disk and the fill floor buckets at the highest version on disk or in the floor; every rebuilt marker, and every synced loss record on disk but a `superseded` one, starts unsettled, so `Since` yields it until `Settle` covers it; precedence decides between two records covering one byte | MUST | RFC 28 §3.1, RFC 1 §3.10 | T7.2, T7.5, T7.6 |
| R13 | The mapping from extents to records is derived: a catalog is never authoritative, the segments win where they disagree, and no second durable index or log of the catalog is kept | MUST NOT | RFC 28 §2.4 | T7.3 |
| R14 | Recovery can rebuild by scan alone, and reads a sealed segment's catalog where it verifies | MUST, SHOULD adopted | RFC 28 §3.1 | T7.2, T7.3 |
| R15 | Catalogs and scan produce an identical index | MUST | RFC 28 §3.1 | T7.3 |
| R16 | A short file, a trailer whose magic or checksum fails, no verifying seal marker for this segment at `footerOffset` − 32, an `entryCount` that does not fit, a footer checksum that fails, or a catalog layout version not recognised is "no footer": the segment is scanned, and none is an error | MUST | RFC 28 §2.4, §2.5 | T7.3 |
| R17 | The active segments are always scanned, and a recycled segment's old trailer and footer are never read as its catalog | MUST | RFC 28 §2.4, §3.1 | T7.3 |
| R18 | A header-only record whose payload the index needs (`loss`, `clone target`) is read at its catalog entry's `recordOffset` and verified like any record, and its header checked against the entry; a mismatch is corruption | MUST | RFC 28 §2.4 | T7.3 |
| R19 | Recovery processes segments in any order, concurrently, and never depends on ascending segment order | MUST NOT | RFC 1 §4.3 | T7.2 |
| R20 | A record wholly covered by a release, truncate, deallocate, delete, clone target or synced loss record that outranks it, named by a loss record (a `superseded` one only over its own extent), or of a tag a later forget record names, is not held, and recovery does not verify or report its payload; a record a `superseded` loss record names over part of its extent is held over the rest, which is verified, and dropped with a loss record of its own over each corrupt run | MUST NOT | RFC 28 §2.3, RFC 1 §3.8 | T7.2, T7.5 |
| R21 | A verifying header whose kind the segment's format version does not define fails the open in every range, the extension range included, and is never skipped by its length | invariant | RFC 28 §2.3 | T7.2 |
| R22 | A record or a floor whose version has a non-zero epoch half, or a record whose version counter or sequence number is the largest 64-bit value, fails the open | MUST | RFC 1 §4.3 | T7.2, T7.6 |
| R23 | An index over its extent bound at open still opens; `WriteAt` and `Fill` are then refused until the count drops below the bound | MUST | RFC 1 §4.2 | T7.6 |

**Losses, forgets, copies and the records a read-only journal failed**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R24 | No record carrying a sequence number a loss record names is held, the original or a copy, even where it verifies, except that a `superseded` loss record reaches it only over its own extent. An unsynced loss record reaches nothing else; a synced one also outranks, like a release record, every record of its extent below its version | MUST | RFC 1 §3.8, RFC 28 §2.3, §3.1 | T7.5 |
| R25 | Every loss record still on disk at open is placed in the loss ring, marked as found at open, and a later open does not report it as new | invariant | RFC 1 §3.8 | T7.5 |
| R26 | A forget record is applied to every record of its tag with a lower sequence number: none of them is held, retained or rebuilt as a marker | MUST | RFC 1 §3, RFC 28 §3.1 | T7.5 |
| R27 | Each record a read-only journal failed earlier in this process, found on disk at the next open of its directory in the process, is dropped before the journal serves, with an unsynced loss record that takes the open reserve before any other and is never deferred; past the reserve the open appends none of them and instead empties each segment still holding one, leaving the failed records out of the copy. The open publishes no event and raises neither the loss generation nor a file's loss sequence | invariant | RFC 1 §5.4, §6 | T7.5, T7.10, T7.11 |
| R28 | Among copies with equal numbers, recovery uses one that verifies. A copy that does not verify while another verifies is not a loss and gets no loss record; it is damage reported with its segment where proven synced, and torn and unreported otherwise | MUST | RFC 28 §3.3 | T7.5 |

**Counters, tags and derived state**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R29 | A file's tag is rebuilt from its records and kept while any record of it is on disk; a `FileID` rebuilt under two tags has every call through either tag refused, and is named in `TagConflicts`, until one tag is forgotten | MUST | RFC 1 §3, §3.7 | T7.6 |
| R30 | The next sequence number exceeds every one on disk, every sync mark's `Sequence` and every number on the directory's failed list (E6's per-directory list of records a read-only journal failed earlier in the process), and the next content version every one on disk and the floor, all taken from the maxima the rebuild observed and the marks it was given, never from a persisted counter | MUST | RFC 1 §4.3, §5.4, RFC 28 §3.1, §3.3 | T7.6, T7.15 |
| R31 | `Files`, `DirtyFiles`, the per-share accounting of `Stats().Shares` (every tag holding a record, attached or not), the footprint and the per-segment counters after open are those the segments describe | MUST | RFC 1 §3.7 | T7.6 |
| R32 | Every header-only record the open attaches is live by RFC 1 §7.2's rule as the running journal would have it: a forget record while any segment holding a record of its tag below its sequence number remains, an unsynced loss record while any segment holding the record it names remains, a removal, release or synced loss record while it may outrank a record in another segment or its marker is unsettled | invariant | RFC 1 §3, §7.2, RFC 28 §3.1 | T7.6 |
| R33 | An extent's offloaded bit after open is set only where an offloaded record covering it decides it, by RFC 28 §3.2's rule, or where it was filled; it is never inferred from a record's age, its segment's seal state, or the absence of a crash | MUST NOT | RFC 28 §3.2 | T7.6 |
| R34 | The open sets each stream's entry in `Stats.SyncMarks` to what it recovered: `Through` and `Offset` the end of the stream's last verifying record, a re-append included, and `Sequence` that record's number | invariant | RFC 28 §3.3, RFC 1 §3.7 | T7.15 |

**Torn and corrupt**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R35 | In the newest segment of a stream, the torn tail (everything after the last verifying record) and a torn slot (a bad record before a later verifying one that no later record and no sync mark proves synced) are treated as never written and never reported as damage; a verifying record after a torn slot is attached | MUST, MUST NOT | RFC 28 §3.3 | T7.4, T7.15 |
| R36 | A bad record, a zero slot included, below the synced-through offset of a later verifying record, below a sync mark's `Through` in its segment, or before a verifying seal marker, is corruption and is reported | MUST | RFC 28 §2.3, §3.3 | T7.4, T7.15 |
| R37 | A zero header slot ends a scan only where no verifying header follows it at any 8-byte boundary to the end of the segment; zeros stepped over are neither damage nor a torn tail | invariant | RFC 28 §3.3 | T7.4 |
| R38 | A scan that meets a record that does not verify refuses it and resynchronises at the next verifying header; the records after it are attached; each such scan is counted in `scan_resyncs` | MUST | RFC 28 §3.3, RFC 1 §3.9 | T7.2 |
| R39 | Where a catalog gives record boundaries, a refusal is confined to the records that do not verify | MUST | RFC 28 §3.3 | T7.3 |
| R40 | A seal marker that does not verify is part of the torn tail, cut with its footer and the segment sealed again, unless a successor names its segment, when it is damage and the segment is reported damaged | invariant | RFC 28 §3.3 | T7.4 |
| R41 | An unverifiable record or a missing seal marker in a segment named as another's predecessor is corruption, and the segment is reported damaged; a torn footer after the seal marker is only "no footer" | invariant | RFC 28 §3.3 | T7.4 |
| R42 | A segment header that does not verify is damage only where proven synced (a verifying record names a synced-through offset past it, a verifying seal marker follows it, or a successor names the segment), and torn otherwise; either way the segment's records are attached on their own checksums, it is treated as one that could have been open at a crash unless a successor names it, and a torn header ends the segment | invariant | RFC 28 §3.1 | T7.4 |
| R43 | A record found not to verify at open has the extents it backs left out of the index and serves no byte, never zeros; its loss is reported as data loss or repairable by its offloaded bit; its loss record is appended once, unless another copy verifies, on the open reserve, and past the reserve the loss is deferred: the extent is dropped, the event published as found at open and marked deferred, a synced one yielded by `Files` and `Since` until `Settle` covers it, and the record written into the copy that retires its segment | MUST | RFC 28 §3.3, RFC 1 §3.8, §6 | T7.10, T7.11 |
| R44 | A range that names no record — a refused range the torn-tail rule makes corruption in a segment whose catalog does not verify, a refused range below a sync mark's `Through` in such a segment, a named predecessor's missing end, or a lost synced tail — is found with the bounds RFC 28 §3.3 gives, and the content it may have superseded is never served | invariant | RFC 28 §3.3 | T7.4, T7.15 |
| R45 | `DamagedSegments` counts every segment the open reports damaged, never one with only a torn header, tail or slot unless a sync mark proves it synced, never a torn slot above every mark's `Through`, and keeps counting it until the journal closes, even once the open empties or retires it | invariant | RFC 1 §3.7 | T7.4, T7.12, T7.15 |
| R46 | A read at open that fails with an I/O error is never evidence of zeros, of a torn write or of corruption: its bytes are never classed, a segment it touches is never a spare, its range is never truncated or cut off, and no loss record is appended for it. A read is started again only while fewer than 3 attempts have been made, less than 15 s has passed since the first, and the open's deadline has not passed | MUST NOT | RFC 28 §3.1, §3.3, RFC 1 §3 | T7.1, T7.2, T7.7 |

**Tails and segments the open ends**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R47 | In the newest segment of each stream, unless a seal marker proven on the device follows its last record, every record past the highest synced-through offset any verifying record names is verified in place before the journal serves: it is read and checked, the segment is synced, and the record is re-read past the page cache and checked again; one that verifies there stays where it is | MUST | RFC 28 §3.1 | T7.8, T7.9 |
| R48 | A seal marker no successor names proves its seal only once the segment is synced and the marker re-read past the page cache verifies; one that does not, or whose segment's sync fails, is treated as absent: the segment is unsealed, its tail verified as R47 says, and the marker part of its torn tail. A read through the cache after that sync proves nothing more | invariant | RFC 28 §3.3 | T7.9 |
| R49 | A record of the unproven tail that verified at the first read and not past the page cache, and every record of a tail whose sync fails, is re-appended, keeping its numbers, from the copy the first read verified, to a spare that is then synced, drawing on the repack reserve; one that verified at neither read is torn; the segment whose sync failed is a failed segment, never sealed | MUST | RFC 28 §3.1, RFC 1 §5.3 | T7.14 |
| R50 | A torn tail is truncated at its start, never below a verifying record and so never below the highest synced-through offset a verifying record names; the truncation is made durable and the segment sealed with its seal marker, footer and trailer after the last verifying record, before anything is appended; the stream continues in a new segment. This truncation and the seal's free tail are the only bytes inside a segment the journal frees | MUST | RFC 28 §3.3, RFC 1 §7.1 | T7.10 |
| R51 | A torn slot before a verifying record (zeros included), a record the re-read of an unproven tail finds not to verify, a torn segment header, or a failed append's slot found again ends the segment: it is truncated the same way and gets no seal marker, and the stream continues in a new segment naming no predecessor | invariant | RFC 28 §3.1, §3.3 | T7.10, T7.14 |
| R52 | Once its tail is proven, a segment is sealed after its last verifying record, unless a sync of it failed in this open, which leaves it a failed segment that is never sealed, or the open ends it | invariant | RFC 28 §3.1 | T7.10 |
| R53 | Every segment the open ends is emptied before the journal serves, after every unproven tail is proven or re-appended: the open's loss records are appended and synced first; then every attached segment that is not held is retired, never a spare, each retirement refilling the reserve; then each ended segment's records that repack would carry forward are re-appended whole, never merged, keeping their numbers, with the loss record of each record that fails only when read for the copy and has no other verifying copy and each deferred loss whose record is in the segment, into a spare that is sealed and synced; a record whose copy already verifies in a segment the open neither ends nor saw fail is skipped, and that segment stays held; then the ended segment is unlinked and the directory synced. A target whose sync fails is a failed segment and its records go once more to another spare; a second refusal in a row, or no spare to be had, fails the open and leaves the ended segment on disk. A segment holding a deferred loss is never retired without that copy. A crash part way leaves no record held twice | invariant | RFC 28 §3.3, RFC 1 §6, §7.2 | T7.11 |
| R54 | The open's re-appends and copies draw on the repack reserve, never on a share's limit, and its loss records on the open reserve, so a journal opened at its maximum, or one whose headroom ran out before the crash, still opens | invariant | RFC 28 §3.1, §3.3, RFC 1 §6 | T7.10, T7.11, T7.14 |

**Spares**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R55 | A segment under a name this journal produces, whose header slot is zeros, does not verify, or verifies naming this journal (with its file's segment id or another), in which no record header and no seal marker verifies, which no successor names and in which no read failed, is re-adopted as a spare, counted toward the headroom, the repack reserve or a stream's next segment, and never treated as an orphan; a segment whose id a sync mark names is never re-adopted | MUST | RFC 28 §3.1 | T7.7, T7.15 |
| R56 | The identity in a segment header is checked before the record-less test, so a foreign segment is never re-adopted or zeroed | invariant | RFC 28 §3.1 | T7.7 |
| R57 | A re-adopted spare's size is checked against the cap and, where the platform reports it, that no extent is unallocated or unwritten; one that fails is unlinked and made again | invariant | RFC 28 §3.1 | T7.7 |
| R58 | A re-adopted spare whose header slot or trailer slot is not zeros has both zeroed and synced before a stream may take it | invariant | RFC 28 §3.1, §2.2 | T7.7 |
| R59 | Open syncs the directory once before any re-adopted spare is taken; if that sync fails the open fails, takes no spare and unlinks none | invariant | RFC 28 §3.1, §2.2 | T7.7 |

**Sync marks**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R60 | `Open` takes the caller's sync marks and checks the scanned tails against them after every unproven tail is verified or re-appended, so no record the open appends lands past a mark's offset | MUST | RFC 1 §3, RFC 28 §3.3 | T7.15 |
| R61 | A segment holding no verifying record whose id a mark names is ended, takes no append, and is unlinked by the open's emptying; a mark whose segment is not on disk is read as retired | invariant | RFC 28 §3.3 | T7.15 |
| R62 | A mark's tail is lost when, after recovery's own tail re-appends, its segment is on disk with its last verifying record, if any, ending below the mark's `Offset`, and no copy of the record carrying the mark's `Sequence` verifies anywhere; a lost tail is never read as a torn tail | invariant | RFC 28 §3.3 | T7.15 |

**Observability**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R63 | `Stats().Recovery` reports how many segments the last open read by catalog and by scan, and how long it took | MUST | RFC 1 §3.7 | T7.12 |
| R64 | `Stats().RejectedSegments` counts the segments the open did not attach | MUST | RFC 1 §3.7 | T7.1, T7.12 |
| R65 | `torn_tails` counts the torn tails truncated at open | MUST | RFC 1 §3.9 | T7.10 |
| R66 | `scan_resyncs` counts the scans that found a record by resynchronising past a bad one | MUST | RFC 1 §3.9 | T7.2, T7.12 |
| R67 | Every field this epic sets is served by `Stats` from a maintained value, never a walk of the index or the segment set | MUST NOT | RFC 1 §3.7 | T7.12 |

**Test rows**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R68 | RFC 28 §4.2 C "foreign segment"; "segment header and seal marker" (its foreign half); the halves of "unidentified directory", "journal gone" and "interrupted `Init`" that hold a segment; the open-failure halves of "`format` file" and "kind values"; A "`format` rebuilt" (its first half: the open fails with `*UnreadableRangesError` naming `format`); RFC 28 §4.4 "a file the journal did not name added" | test row | RFC 28 §4.2 A, C, §4.4 | T7.1, T7.2 |
| R69 | RFC 28 §4.2 C "sources agree", "trailer torn", "unknown version", "neighbours survive"; A "footer is not authoritative" (its rebuild half) | test row | RFC 28 §4.2 A, C | T7.3 |
| R70 | RFC 28 §4.2 C "synced damage is not a torn tail", "truncated sealed segment" (its newest-segment half); the scan half of "segment header and seal marker"; A "a zero slot is not the end" (its reopen half) | test row | RFC 28 §4.2 A, C | T7.4 |
| R71 | RFC 1 §9.4 C "a loss survives restart", "forget leaves nothing to settle", "header-only records are re-appended outside a read-only journal" (its reopen half); RFC 1 §9.4 "removals survive to `Since`" (its reopen half); RFC 28 §4.2 C "a bad copy does not mask a good one", "a partly superseded record is still checked" | test row | RFC 1 §9.4 C, RFC 28 §4.2 C | T7.5 |
| R72 | RFC 1 §9.4 C "monotonicity", "epoch half is zero", "clone target survives a crash", "unclaimed tag reported", "unclaimed tags are the caller's to name", "a file is its share's" (its planted half), "a cancelled call changes nothing or completes" (its reopen half); RFC 28 §4.2 C "version floor" (its first half), A "write carries its mtime" (its reopen half) | test row | RFC 1 §9.4 C, RFC 28 §4.2 A, C | T7.6 |
| R73 | RFC 28 §4.2 C "spares are re-adopted", A "directory entry survives", "unreadable is not empty" (its first half), and the open halves of C "a spare is whole or absent" and "retired segments are recycled" | test row | RFC 28 §4.2 A, C | T7.7 |
| R74 | RFC 28 §4.2 A "unproven tail verified at open", C "a seal marker only the cache holds" | test row | RFC 28 §4.2 A, C | T7.9, T7.14 |
| R75 | RFC 28 §4.2 C "rebuild", "torn tail cut off", "a recycled segment's old catalog is never read", "an open at an exhausted headroom"; RFC 1 §9.4 D "metrics are reachable" (the torn tail) | test row | RFC 28 §4.2 C, RFC 1 §9.4 D | T7.10 |
| R76 | RFC 28 §4.2 C "an ended segment is emptied once", the ended-segment half of "an open at an exhausted headroom", and the failed-append reopen of "a recycled segment's old catalog is never read" | test row | RFC 28 §4.2 C | T7.11 |
| R77 | RFC 1 §9.4 D "metrics are reachable" (the rejected segment) | test row | RFC 1 §9.4 D | T7.12 |
| R78 | RFC 28 §4.4 over `Open`: lost, torn and reordered writes at every sync point of every operation that syncs, `Open`'s own included, and a bit flipped in every region but a record header that leaves a range that names no record, each reopen agreeing with the reference model as §4.4 says; the model run with crashes | test row | RFC 28 §4.4, RFC 1 §9.1 | T7.13 |
| R79 | RFC 28 §4.2 C "a mark proves only up to `Through`" (its torn-slot, torn-header and counter halves) | test row | RFC 28 §4.2 C | T7.15 |

## 4. Decisions this epic takes

- **D1, `Open` extends T8.15 through the same constructor.** T8.15 builds a fresh journal
  through `datapath.New(deps, state)`. This epic's `Open` runs `recovery.Run`, which returns the
  rebuilt state, and calls the same `New` with it, so there is one assembly. T8.15's refusals
  stay where they are, and run before the attach pass; the `ErrNotImplemented` T8.15 returns for
  a directory holding a segment is removed at the checkpoint after T7.10 for a directory whose
  open ends no segment and at T7.11 for the rest (§7), and the `decision:` marker T8.15 put on it
  is removed with it. `Open` gains its `marks` argument at T7.15, before the checkpoint.
- **D2, no orphan is unlinked.** RFC 28 §3.4 lets an implementation unlink an orphan it is
  certain it wrote, after an age gate. In this format version every `.seg` file this journal
  wrote either verifies as its own and is attached, passes as a spare and is re-adopted (R55), or
  is a mark's empty segment, which the emptying unlinks (R61); what is left is foreign, or under
  a name it could not have produced, or a file it does not recognise, none of which it may
  unlink. So the open reports orphans in `RejectedSegments` and unlinks none. A `decision:`
  marker at the attach pass names the ceiling: orphans cost only their space and an operator's
  attention, and it is overturned by an orphan class this journal can prove it wrote that
  accumulates in the field.
- **D3, no footer is written for a segment sealed by a previous process.** RFC 28 §2.4 permits
  it (a **MAY**) after a verifying scan. Such a segment is one sealed at a crash after its marker
  and before its footer was durable; the open already seals a torn tail with a footer (R50), and
  any other is scanned at every open. A `ponytail:` marker at the catalog reader names the cost:
  one scan per such segment per open; upgrade to writing the footer once J6 shows these scans.
- **D4, the tail's re-appends and the emptying's copies share one copier, whose target is
  sealed.** RFC 28 §3.1 re-appends the tail the device lost "to a spare that is then synced";
  §3.3 copies an ended segment's records "to a spare, which it seals and syncs", with a second
  spare after a failed sync and the open failing after a second refusal. T7.14 builds that
  copier once, sealing the target in both uses, and T7.11 reuses it. Sealing a tail re-append's
  target costs one seal marker and footer per stream re-appended, and its seal marker proves the
  copies synced to a later open, so a second crash never reads them as an unproven tail again.
  A `decision:` marker at the copier says so; overturned if J6 shows those seals at open.
- **D5, offloaded and unmark records are fed to E4's builder as they are.** E4-11's builder
  already applies RFC 28 §3.2's rule (the highest sequence number among the marks at or above a
  piece's version decides its bit, a fill marks its own content) and nothing else sets a bit at
  rebuild. So the bits after open are rebuilt here at no extra cost (R33); the stale rule and
  the reseed are E9b's.
- **D6, a hold, unhold or stamp record refuses the open.** Retained extents rebuilt from them
  (RFC 1 §3.11) are E7b's, and nothing appends them before E9b's holds. An open that finds one
  that verifies and is not covered fails with `ErrNotImplemented` naming the kind and the
  segment, rather than dropping a snapshot's retained content in silence. A `decision:` marker at
  the feed says so and that it is withdrawn with E7b. E9b's holds therefore ship no earlier than
  E7b (§9).
- **D7, the emptying asks E9a's tracker; it carries no superset.** RFC 28 §3.3 retires every
  attached segment that is not held before the emptying and carries "the records of the segment
  that repack would carry forward". Both questions are E9a's `Held` and `Droppable` (its T9.1),
  which T7.6 fills at open, so T7.11 waits for T9.1 rather than carrying a superset: a superset
  would retire fewer segments than RFC 28 §3.1 asks, and an emptying at the maximum could then
  run out of spares.
- **D8, the rebuild feeds E4's builder from one goroutine per segment, bounded.** E4-11 chose
  concurrent feeders. The open runs at most `GOMAXPROCS` segment readers, each scanning or
  reading the catalog of one segment and feeding its records; a `ponytail:` marker names the
  ceiling (one segment per reader, so one huge active segment is read by one goroutine) and the
  upgrade (splitting a scan at catalogued offsets) when J6 shows it.
- **D9, the open's loss records go through `Lose` directly, on the open reserve.** During the
  rebuild a record dropped as corrupt is left out of the builder's feed and noted with whether
  recovery proved it synced. Once `New` has assembled the journal, T7.10 calls E6's `Lose` for
  each record of R27 first, marked `AtOpen` and never `Deferred`, then for each corrupt drop no
  loss record on disk names, marked `AtOpen` while the open reserve lasts and `Deferred` past it,
  so the loss record, the event, the generation and `corrupt_extents` follow E6's one rule.
  `Lose` appends before it returns; T7.10 then waits on `WaitDurable` for those records before
  T7.11 retires or empties anything (RFC 28 §3.3). `Corrupt`'s read-time hand-off to a worker is
  not used at open, so nothing waits on a background append. A record a loss record on disk
  already names is not handed over again (R25, R43).
- **D10, the open writes nothing until the rebuild has finished.** The attach pass and the
  rebuild only read (and unlink `.spare.tmp` files, which hold nothing). Every write — the
  directory sync, a spare's zeroing, the tail's sync, a re-append, a truncation, a seal, a loss
  record, an emptying — comes after, so a cancelled or failed open before that point leaves the
  directory as it found it, and one after it leaves only states a later open recovers (R11).
- **D11, a direct read the filesystem refuses fails the open.** `O_DIRECT` is refused by some
  filesystems and `F_NOCACHE` is advisory (E3's open point P12). Until RFC 28 says otherwise, a
  direct read that fails with the platform's refusal fails the open naming the rule, as the
  platform checks do, rather than falling back to the page cache and trusting what R47 exists not
  to trust. A `decision:` marker at `ReadAtDirect` names the ceiling: a journal on such a
  filesystem cannot reopen after a crash that left an unproven tail.
- **D12, the allocation check of a re-adopted spare queries Linux only.** RFC 28 §3.1 checks
  "where the platform reports it". On Linux the open asks `FIEMAP` for unwritten or missing
  extents; on macOS and Windows it checks the size alone. A `ponytail:` marker names the gap and
  the upgrade (`F_LOG2PHYS_EXT` and `FSCTL_QUERY_ALLOCATED_RANGES`) when a short spare is seen on
  either.
- **D13, a lost synced tail refuses the open.** RFC 28 §3.3 counts a lost tail as a range that
  names no record and has the engine reconcile it. The reconcile's journal half is E7b's. This
  epic runs the check (R62), which costs a comparison per mark, and on a lost tail fails the open
  with `ErrNotImplemented` naming the mark's segment, its `Offset` and `Sequence`, before it
  appends anything past the mark. It never reads the loss as torn. A `decision:` marker at the
  check names the ceiling — a device that loses an acknowledged sync keeps its journal closed
  until E7b, and every retry meets the same loss — and is withdrawn with E7b.
- **D14, a range that names no record, or a verifying `reconcile` or `reconciled` record,
  refuses the open.** Serving the rest could serve content the lost record superseded (RFC 28
  §3.3's I1 case), and the reconcile that would gate it is E7b's. T7.4 finds every such range
  with RFC 28 §3.3's bounds (R44), and T7.2 meets the two record kinds; either fails the open
  with `ErrNotImplemented` naming the segment and the range, or the kind, before anything is
  written. A `decision:` marker at each site names the ceiling — a rotted header in a segment
  with no verifying catalog, a truncated named predecessor, or a rotted record below a mark's
  `Through` keeps the journal closed until E7b — and is withdrawn with E7b. The RFC 28 §4.4 bit
  flips that make such a range wait for E7b too (§1.2).
- **D15, any read at open that fails refuses the open.** RFC 28 §3.3 holds a record whose read
  fails as unread and opens the journal where a header or a catalog names it, failing the open
  only for a range nothing names. Holding an unread record, the suspect segment and the unread
  file are E7b's. This epic retries each read as RFC 28 §3.3 says (3 attempts, 15 s, the open's
  deadline) and then fails the open with `*UnreadableRangesError` naming every range it could not
  read (or `format`), before it writes anything, never classing the bytes as zeros, torn or
  corrupt, never making the segment a spare. The error is the one RFC 1 §3 gives for an unnamed
  range, so the engine's start already refuses the journal's shares and retries the open on it.
  RFC 28 §3.3's two `decision:` notes on a failed read go at the retry, and a `decision:` marker
  of this epic's names the ceiling — a transient fault on a named record keeps the journal closed
  until the read succeeds — and is withdrawn with E7b. A read past the page cache in the tail's
  proof that fails is not such a read: R49 re-appends the record from its first copy (T7.14).

## 5. Target tree

The journal's code lives at `pkg/block/journal` on the programme branch.

```text
pkg/block/journal/
  journal.go             T7.1, T7.10, T7.15   Open runs recovery.Run, then datapath.New (D1); the marks argument
  errors.go              T7.1    SegmentRange, *UnreadableRangesError wrapping ErrUnreadable (D15)
  stats.go               T7.15   Stats.SyncMarks
  internal/
    recovery/
      run.go             T7.1, T7.15   Run(ctx, dir, floor, expect, marks): the phases, the context check, the open span
      attach.go          T7.1    the directory listing, .spare.tmp, the id high-water mark, headers, foreign, orphans (D2)
      read.go            T7.1    the open's reads, retried, and the unreadable ranges (D15)
      scan.go            T7.2    scanning a segment and feeding the builder, the concurrent readers (D8), the reconcile kinds (D14)
      catalog.go         T7.3    reading a catalog, demotion to a scan, header-only payloads (D3)
      tail.go            T7.4    torn against corrupt, stream topology, the segment's fate, ranges that name no record (D14)
      kinds.go           T7.5    loss and forget records, copies, the read-only drops, the hold refusal (D6)
      assemble.go        T7.6    counters, tags, derived lists and accounting, liveness registration, New
      spares.go          T7.7    re-adoption, the slots' zeroing, the directory sync (D12)
      prove.go           T7.9    the unproven tail's first read, sync and direct re-read; the unnamed seal marker's proof
      seal.go            T7.10   cutting, sealing, ending, the open's loss records (D9)
      empty.go           T7.11   retiring what is not held, emptying the ended segments (D7)
      stats.go           T7.12   Recovery, RejectedSegments, torn_tails, scan_resyncs
      copy.go            T7.14   the copier: whole records, numbers kept, a sealed and synced spare, one retry (D4)
      marks.go           T7.15   the marks: counters, the proof below Through, the mark's segment, the lost-tail check (D13)
      *_test.go
    platform/
      direct_linux.go    T7.8    ReadAtDirect with O_DIRECT (D11)
      direct_darwin.go   T7.8    ReadAtDirect with F_NOCACHE
      direct_windows.go  T7.8    ReadAtDirect with FILE_FLAG_NO_BUFFERING
      alloc_linux.go     T7.7    the FIEMAP query (D12)
  conformance/
    rfc28_c_test.go      T7.9, T7.10, T7.11, T7.15   rows RFC 28 §4.2 lists with no test yet (§9)
test/journal/known.yaml  T7.10, T7.11, T7.13   entries removed and re-pointed
```

## 6. Subtasks

### T7.1 The open's phases and the attach pass

**Subtask T7.1 estimate: 4.5 man-days.**

- **Track:** A. **Depends on:** S3; T8.15; E3's name classifier (3.2), `format` and segment
  header codecs, platform checks.
- **Covers:** R1–R6, R8–R11, R46 (the retry, the `format` file and the headers), R64, R68.
- **Deliverables:** `internal/recovery/run.go`: `Run(ctx, dir, floor, expect) (State, error)`,
  which runs T8.15's identity and platform checks, then the phases in order, checking `ctx`
  before each (R11, D10), under the `open` span with one child span per phase. `attach.go`: lists
  the directory once; classifies every name through E3's classifier; ignores `format.tmp`;
  unlinks every `.spare.tmp` and syncs nothing yet (D10); records the highest id any `.seg` or
  `.spare.tmp` names, for E5's allocator (R6); reads each segment header and sorts the segments
  into *own* (header verifies and names this journal's identity), *foreign* (verifies, another
  identity: reported, counted in `RejectedSegments`, left on disk), *unverified header* and *zero
  header slot* (both decided by T7.4 and T7.7), and fails the open on a segment version the binary
  does not know or newer than the `format` file's (R9). Files the classifier does not recognise
  are left alone and logged once at `Debug`; orphans are reported and never unlinked (D2, with its
  `decision:` marker). `internal/recovery/read.go`: every read the open makes goes through one
  helper that starts a read again only while fewer than 3 attempts have been made, less than
  15 s has passed since the first, and `ctx` allows, with RFC 28 §3.3's two `decision:` notes on a
  failed read, and collects each range no attempt read; once the rebuild is done, any such range
  fails the open with `*UnreadableRangesError` naming every one, before anything is written (R46,
  D15, with its `decision:` marker). `errors.go`: `SegmentRange` and `*UnreadableRangesError`
  wrapping `ErrUnreadable`, as RFC 1 §3 writes them; a `format` file no read can read fails the
  open with it at once, `Format` set (R2). `Open` calls `Run` and, until the
  checkpoint after T7.10, returns T8.15's `ErrNotImplemented` once the attach pass succeeds.
- **Acceptance criteria:**
  - A directory holding segments and no `format` file, one whose `format` names another identity,
    and `expect` zero each fail as T8.15 says, and the directory is byte-for-byte unchanged.
  - A 55- or 57-byte `format` file, a bad checksum and version 2 each fail the open.
  - A `format` file whose every read fails with `EIO` fails the open with `*UnreadableRangesError`
    whose `Format` is set, and nothing in the directory changes.
  - A segment header read that fails twice and then succeeds attaches the segment; one that
    fails every attempt, after at most 3 attempts or 15 s on the test clock, fails the open
    naming the segment.
  - A foreign segment is counted, left on disk with its bytes unchanged, and contributes nothing;
    an unrecognised file is left alone.
  - Every `.spare.tmp` is gone after the pass, and the allocator seed is above its id.
  - A segment of version 2, or of a version above the `format` file's, fails the open.
  - A context cancelled before any phase returns the context's error with nothing changed but the
    `.spare.tmp` unlinks.
- **Tests:** the rows of R68 (`TestRFC28C_ForeignSegment`, `TestRFC28C_ForeignHeaderIsForeignNotDamaged`, `TestDamage_UnnamedFileLeftAlone`, `TestRFC28C_UnidentifiedDirectory`, `TestRFC28C_JournalGone`, `TestRFC28C_InterruptedInit`, `TestRFC28C_FormatFile`, `TestRFC28C_KindValuesRefused`, and the first half of the "`format` rebuilt" row); a cancel injected at each phase boundary (catches a phase that
  ignores the context); a `.spare.tmp` with the highest id in the directory, then a spare made
  after open (catches an allocator seeded only from `.seg` names, R6); a file named
  `00000000000000000042.seg.bak` and one named `notes.txt` left in place (catches a classifier
  that deletes what it does not know); the unreadable `format` and the header read failing then
  succeeding (catches an I/O error read as a checksum failure, or a fourth attempt, R46).

### T7.2 Rebuild by scan

**Subtask T7.2 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T7.1; E3's slot scanner (3.6); E4-11.
- **Covers:** R7, R12 (the feed), R14 (scan alone), R19, R20, R21, R22 (records), R38, R46 (the
  segments), R66 (the count), R68 (kind values).
- **Deliverables:** `internal/recovery/scan.go`, reading through T7.1's helper (R46): a segment reader that
  runs E3's scanner over one segment and turns each verifying record into E4's `Record` (the
  content and removal kinds, offloaded and unmark) or hands it to T7.5 (loss, forget, hold,
  unhold, stamp), with its synced-through offset kept for T7.4; at most `GOMAXPROCS` readers feed
  the builder concurrently (D8, with its `ponytail:` marker). A verifying header of a kind the
  segment's version does not define, a non-zero epoch, or a version counter or sequence number at
  the largest 64-bit value fails the open (R21, R22); a verifying `reconcile` or `reconciled`
  record fails it with `ErrNotImplemented` naming the kind and segment (D14, with its `decision:`
  marker). The scanner's resync count is summed into `scan_resyncs` (R38, R66). The scan reads
  headers only: a payload is read where a later subtask needs it (T7.3's header-only payloads,
  T7.9's unproven tail, T7.11's copies), never for a record a later one wholly covers (R20).
- **Acceptance criteria:**
  - A journal written, closed and rebuilt by scan alone holds exactly the extents the reference
    model holds, with the same versions and records.
  - Feeding the segments in shuffled orders and with one to `GOMAXPROCS` readers gives an
    identical index each time.
  - A verifying header of kind 17 or 128, or a version with epoch 1, fails the open; neither is
    skipped.
  - A verifying `reconcile` or `reconciled` record fails the open naming it, with nothing written.
  - A segment read that fails with `EIO` twice and then succeeds rebuilds as if nothing failed;
    one that fails every attempt fails the open with `*UnreadableRangesError` naming that range,
    and no segment changes.
  - A torn slot before a verifying record in a stream's newest segment is stepped over by
    resynchronising, and `scan_resyncs` counts one.
  - A rebuild of segments that are not the newest of a stream reads no payload byte (the seam
    counts reads).
- **Tests:** scan-alone rebuild against the model over a seeded workload; shuffled-order and
  reader-count variants under `-race` (catches an order dependence or a race in the feed, R19);
  the kind-values and epoch rows (catches a skip by length, R21); the reconcile kinds (catches a
  kind decoded and ignored, D14); the `EIO` schedule on a segment read (catches a failed read
  classed as zeros or as a bad record, R46); the torn slot (catches a resync not counted, R66); a seam read counter during rebuild (catches a
  payload read, R20).

### T7.3 Rebuild by catalog

**Subtask T7.3 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T7.2; E3's catalog codec; E5's seal writer (T5.7), for the
  catalogs under test.
- **Covers:** R13, R14 (catalog), R15, R16, R17, R18, R39, R69.
- **Deliverables:** `internal/recovery/catalog.go`: for each sealed segment, reads the last 32
  bytes, checks the trailer, the seal marker at `footerOffset` − 32 for this segment and offset,
  the `entryCount` fit and the footer checksum, and feeds the entries; any failure or an
  unrecognised layout version demotes the segment to T7.2's scan (R16). The newest segment of
  every stream, and every segment no successor names, is scanned whatever its trailer says (R17).
  A `loss` or `clone target` entry is read at its `recordOffset`, verified, and its header checked
  against the entry; a mismatch is corruption confined to that record (R18, R39), handed to T7.4.
  Nothing is written (D3, with its `ponytail:` marker). A test-only switch makes every segment
  scan, so the two sources can be compared.
- **Acceptance criteria:**
  - The index built from catalogs equals the one built by scan, field for field, over a workload
    with every record kind E8 and E6 append.
  - A truncated footer, a flipped entry byte, a trailer of layout version 2 and a trailer with no
    seal marker before its footer each demote the segment to a scan with the same index.
  - A recycled segment whose old trailer survived is scanned and none of its old records attached.
  - A corrupt record in a catalogued segment leaves every neighbour readable.
- **Tests:** the rows of R69 (`TestRFC28C_SourcesAgree`, `TestRFC28C_TrailerTorn`, `TestRFC28C_UnknownVersion`, `TestRFC28C_NeighboursSurvive`, `TestRFC28A_FooterIsNotAuthoritative`); the both-ways equality over the model workload (catches a source
  that drops a kind, R15); a recycled segment with its zeroing dropped at the seam (catches a
  trailer accepted without its seal marker, R17); an entry whose header disagrees (catches an
  entry trusted over its record, R13).

### T7.4 Torn against corrupt

**Subtask T7.4 estimate: 5 man-days.**

- **Track:** A. **Depends on:** T7.2, T7.3.
- **Covers:** R35, R36, R37, R40, R41, R42, R44 (the scan's ranges), R45 (marking), R70.
- **Deliverables:** `internal/recovery/tail.go`: the stream topology (each segment's predecessor,
  the segments a successor names, the newest segment of each stream, and every segment that
  could have been open at a crash, a segment whose seal marker no successor names included); for
  every bad slot the scanner or the catalog reports, the decision torn, torn slot or corrupt by
  R35 to R42, using the highest synced-through offset a later verifying record names and whether
  a verifying seal marker follows (T7.15 adds the marks' `Through` to both); the zero-slot rule
  (R37); the seal marker rule (R40); the fate of each segment for T7.9 and T7.10: *intact*,
  *sealed*, *seal to prove* (a marker no successor names, for T7.9), *torn tail to cut at offset
  o*, or *ended at offset o*; the list of records dropped as corrupt with the synced flag
  recovery proved (D9). Every range that names no record the scan finds — a refused range the
  torn-tail rule makes corruption in a segment whose catalog does not verify, or a named
  predecessor's missing end, each with RFC 28 §3.3's bounds — fails the open with
  `ErrNotImplemented` naming the segment and the range (R44, D14, with its `decision:` marker);
  a refused range made up wholly of records whose headers verify is not one, and goes to the
  list. Damage is marked on the segment for `DamagedSegments`. RFC 28 §3.3's two `ponytail:`
  notes (the failed slot no record proves; the forward search's cost) go at the forward search.
  `FuzzOpen` (rule 2).
- **Acceptance criteria:**
  - In the newest segment, A and B synced, C written, A's payload corrupted: A is corruption, B is
    held, C is cut as a torn tail.
  - A zero slot with a verifying record after it is stepped over and reported as nothing; the
    same zeros below a later record's synced-through offset are corruption.
  - A segment named as a predecessor and truncated mid-record fails the open naming its missing
    end; the newest segment of a stream truncated the same way is a torn tail.
  - A record's header rotted in a sealed segment whose catalog does not verify fails the open
    naming the range, and nothing is served; with the catalog verifying, the record is dropped
    and the journal opens.
  - A seal marker that does not verify is a torn tail when nothing names its segment and damage
    when a successor does.
  - A segment header that does not verify is damage when a seal marker follows it and torn
    otherwise; its records are attached either way, and a torn one ends its segment.
  - `DamagedSegments` is never raised by a torn header, tail or slot.
- **Tests:** the rows of R70 (`TestRFC28C_SyncedDamageIsNotATornTail`, the newest-segment half of `TestRFC28C_TruncatedSealedSegment`, `TestRFC28C_SealMarkerCopiedIntoAPayload`, `TestRFC28A_AZeroSlotIsNotTheEnd`); one test per acceptance bullet, each with the seam writing the
  exact bytes (catches a rule applied to the wrong segment class); the rotted header with and
  without a catalog (catches a range dropped by a header it cannot trust, which would serve the
  older version, R44); `FuzzOpen` (catches a panic or an extent the bytes do not hold).

### T7.5 Loss and forget records, copies, and the records a read-only journal failed

**Subtask T7.5 estimate: 4 man-days.**

- **Track:** A. **Depends on:** T7.2, T7.4; E6's T6.2 (the ring's `Place(found at open)` and the
  per-directory failed-sequence list).
- **Covers:** R12 (synced losses unsettled), R20 (named and forgotten records), R24–R28, R71.
- **Deliverables:** `internal/recovery/kinds.go`: the kinds E4's builder leaves to recovery (its
  `decision:` on `RecordKind`). A loss record's payload is read (from the scan, or at its catalog
  entry), and the sequence number it names is withheld from the feed, original and every copy,
  over the loss's own extent only for a `superseded` one, whose record keeps the rest (R20, R24);
  a synced one is also fed as a release-like cut of its extent at its pair, registered as an
  unsettled marker so `Since` yields it until `Settle` (R12, R24). Every loss record is placed in
  the ring through `Place(found at open)` (R25). A forget record withholds every record of its
  tag below its sequence number (R26). Records with equal numbers are collapsed to one verifying
  copy, preferring one in a segment the open neither ends nor saw fail; a non-verifying copy
  while another verifies is no loss and is damage only where proven synced (R28). Each sequence
  number on E6's per-directory failed list found on disk is withheld and noted for an unsynced
  loss record with no event and no raise (R27), which T7.10 appends first. A hold, unhold or stamp
  record that verifies and is not covered refuses the open (D6, with its `decision:` marker).
- **Acceptance criteria:**
  - A record a loss record names is not held after open even where its bytes were repaired on
    disk; an unsynced loss record re-holds what the dropped record superseded, and a synced one
    keeps it covered.
  - A loss found at `ReadAt` and synced before a clean exit is yielded by `Since` after the
    reopen, until `Settle` covers it, and not after.
  - A `superseded` loss record over half of R's extent leaves the other half held; with R's
    payload corrupted, that half is dropped with its own loss record and reads as lost.
  - Every loss record on disk is in `Losses`, marked found at open; a second reopen adds none.
  - After `Forget(T)`, a crash and a reopen hold no extent or marker of `T`, and `Since` yields
    nothing of it; a write under `T` after the forget is held.
  - A re-appended copy and its corrupted original on a failed segment: the copy reads back, no
    loss record names it, and nothing is reported damaged.
  - A record failed by a read-only journal and found whole at the next open in the process is not
    held, gets an unsynced loss record, and neither `LossGeneration` nor `Losses` moves.
- **Tests:** the rows of R71 (`TestRFC1C_LossSurvivesRestart`, `TestRFC1C_ForgetLeavesNothingToSettle`, `TestRFC28C_BadCopyDoesNotMaskAGoodOne`, `TestRFC1C_HeaderOnlyRecordsReappended`, and the reopen halves of "removals survive to `Since`" and "a partly superseded record is still checked"); the synced loss at `Since` across a clean exit (catches a loss rebuilt as settled, so the engine never commits the lost write's existence, R12); the repaired-bytes case (catches a loss applied by position rather
  than by sequence number); the synced-versus-unsynced pair (catches a loss keyed on "at or
  below", RFC 1 §3.8); the `superseded` half (catches a loss applied over the whole record, R24);
  the read-only drop with the registry's list (catches a reopen that serves a write its `Sync`
  was told failed).

### T7.6 Counters, tags, derived state and assembly

**Subtask T7.6 estimate: 3.25 man-days.**

- **Track:** A. **Depends on:** T7.5; E4-11 (`Finish(floor)`); E5's segment set and ledger
  (T5.1), pool (T5.5); E8's `New` (D2 of E8), conflict flag (T8.4), dirty-file list (T8.9) and
  `Stats` aggregation (T8.13); E9a's liveness tracker (T9.1).
- **Covers:** R7 (no outside input), R12 (markers and buckets), R22 (the floor), R23, R29–R33,
  R72.
- **Deliverables:** `internal/recovery/assemble.go`: calls the builder's `Finish(floor)`, which
  refuses a floor with a non-zero epoch and sets the sequence and version counters above every
  number fed and the floor (R22, R30; T7.15 raises the sequence counter above the marks), and the
  sequence counter above the largest number on E6's failed list for the directory (R30); turns
  the builder's rivalled files into E8's conflict flag (R29); rebuilds the dirty-file list, the
  files with an unsettled marker or synced loss, the per-share used, held, dirty and unreclaimed
  bytes for every tag that holds a record, and E5's ledger and per-segment counters from the
  attached segments (R31); registers every attached header-only record with E9a's liveness
  tracker through its registration hook, a forget record with the set of segments holding
  records of its tag below its sequence number (which the feed collected per tag and segment), an
  unsynced loss record with the segments holding copies of the record it names, and offloaded and
  unmark records under the tracker's always-live switch (R32); hands the result to `datapath.New`
  (D1). Offloaded and unmark records reach the builder as fed (D5). An index over its bound is
  accepted (R23).
- **Acceptance criteria:**
  - After a crash, the next sequence number and the next content version exceed every one on
    disk; opened with a floor above them, the next version exceeds the floor.
  - A read-only journal fails B, C and D before any reaches the device; the reopen in place gives
    the next stable write a number above D's, and a second reopen in the process holds that write.
  - A record of F planted under tag B beside A's: `TagConflicts` names F with both tags and every
    call on F through either is refused.
  - `Files`, `DirtyFiles`, `Stats().Shares` and `UsedBytes` after a reopen equal what the running
    journal reported before the crash, for every share, attached or not.
  - An offloaded record planted over a held extent, then a crash: after the reopen the extent is
    not in `DirtyFiles` and `DirtyBytes` excludes it; planted below the extent's version, or with
    an unmark above it, the extent stays dirty.
  - A journal reopened under a lower `ExtentLimit` opens, and `WriteAt` is refused naming the
    index.
  - A `CloneTarget` marker survives a crash with its spec, and `Since` yields it.
  - After `Forget(T)` and a reopen, the forget record stays live until the last segment that held
    a record of `T` below it is retired, and not after; a segment holding only settled, outranked
    removals reports not held without a read.
- **Tests:** the rows of R72 (`TestRFC1C_Monotonicity`, `TestRFC1C_EpochHalfIsZero`, `TestRFC1C_CloneTargetSurvivesCrash`, `TestRFC1C_UnclaimedTagReported`, `TestRFC1C_UnclaimedTagsAreTheCallersToName`, `TestRFC1C_FileIsItsSharesShare`, `TestRFC28C_VersionFloor`, `TestRFC28A_WriteCarriesItsMtime`, `TestRFC1C_CancelledCallChangesNothingOrCompletes`); the before-and-after comparison of `Stats`, `Files` and
  `DirtyFiles` (catches a derived value not rebuilt, R31, tested at its consumer); RFC 1 §9.4's
  "a failed number is never issued again" (catches a number taken from the disk alone, which gives
  a new write a failed number and a later reopen drops it after its `Sync` succeeded, R30); the floor case
  (catches a counter taken from disk alone); the planted offloaded record (catches a bit dropped
  at rebuild or inferred from a sealed segment, R33, tested at `DirtyFiles`); the forget record's
  liveness across a reopen, checked through E9a's `Held` (catches a forget registered with a
  count, which a repack of one of its segments and a later write under the tag would break).

### T7.7 Spare re-adoption

**Subtask T7.7 estimate: 3.5 man-days.**

- **Track:** A. **Depends on:** T7.1, T7.4; E5's pool `Adopt` (T5.5), retirement (T5.9) and
  making a spare (T5.4a).
- **Covers:** R8 (never zeroed), R46 (never a spare), R55–R59, R73.
- **Deliverables:** `internal/recovery/spares.go`: the record-less test of R55 over the segments
  T7.1 sorted and T7.4 read (identity first, R56), including a recycle a crash interrupted after
  the rename; a segment any read of which failed is never a candidate (R46); the size check and,
  on Linux, the `FIEMAP` allocation query in `platform/alloc_linux.go` (D12, with its `ponytail:`
  marker); a spare that fails is unlinked and left for the pool to make again (R57); the one
  directory sync before any re-adopted spare is taken, failing the open with its error and
  taking and unlinking nothing if it fails (R59); the zeroing and sync of a dirty header or
  trailer slot (R58); then `Adopt` into the pool's classes. T7.15 withholds a mark's segment from
  the test (R55).
- **Acceptance criteria:**
  - Crashed with every spare untaken, and after a stream took one but before its first sync, the
    reopen re-adopts every record-less segment, the headroom and the repack reserve are whole with
    no new allocation, and nothing is reported as an orphan or as damage.
  - A recycle crashed between its rename and its zeroing sync is re-adopted with both slots zeroed
    before any stream takes it, and no record of its old life is attached.
  - A foreign segment with no record of this journal is never re-adopted and keeps its bytes.
  - A sealed segment holding dirty records whose every read fails, header slot included, is not
    re-adopted, no slot of it is zeroed, and the open fails naming it.
  - A spare short of the cap is unlinked and made again.
  - With the open's directory sync failing at the seam, the open fails, no spare is taken or
    unlinked, and a later open with the fault cleared succeeds with its reserves whole.
- **Tests:** the rows of R73 (`TestRFC28C_SparesAreReAdopted`, `TestRFC28A_DirectoryEntrySurvives`, `TestRFC28C_SpareIsWholeOrAbsent`, `TestRFC28C_RetiredSegmentsAreRecycled`, and the first half of "unreadable is not empty"); the foreign record-less segment (catches the record-less test run
  before the identity check, R56); the unreadable segment (catches an I/O error read as zeros,
  which would zero acknowledged writes, R46); the failing directory sync (catches a spare taken
  whose name is not durable); a punched hole in a spare on Linux (catches a size-only check where
  the platform reports allocation).

### T7.8 Reads past the page cache

**Subtask T7.8 estimate: 2.5 man-days.**

- **Track:** A. **Depends on:** E3's platform layer (3.7).
- **Covers:** R47 (the read).
- **Deliverables:** `ReadAtDirect` in `internal/platform`: `O_DIRECT` with aligned buffers on
  Linux, `F_NOCACHE` on macOS, `FILE_FLAG_NO_BUFFERING` with sector-aligned buffers on Windows;
  E3's `ponytail:` on `ReadAtDirect` is removed. A refusal by the filesystem is returned as itself
  and fails the open (D11, with its `decision:` marker). The seam's `ReadAtDirect` returns the
  bytes the device holds, not those the simulated cache holds.
- **Acceptance criteria:**
  - On Linux, a direct read of a range whose page cache was changed under the file (through a
    second descriptor written and not synced, then the device copy checked) returns the device's
    bytes.
  - An unaligned record is read through an aligned window and returned exactly.
  - On a filesystem that refuses `O_DIRECT`, the open fails naming it.
- **Tests:** the device-versus-cache read on Linux (catches a read that still goes through the
  cache); the alignment cases at the first and last byte of a segment (catches an off-by-one in
  the window); macOS and Windows builds compile and run the alignment test where a runner exists
  (E3's open point P22).

### T7.9 The unproven tail and the unnamed seal marker

**Subtask T7.9 estimate: 3.5 man-days.**

- **Track:** A. **Depends on:** T7.4, T7.8.
- **Covers:** R47, R48, R74 (the in-place and cache-only-marker halves).
- **Deliverables:** `internal/recovery/prove.go`. For each segment T7.4 marked *seal to prove*:
  one sync of the segment, then the 32-byte marker re-read through `ReadAtDirect`; a marker that
  verifies there proves the seal; one that does not, or a segment whose sync fails, is treated as
  absent, the segment unsealed and its tail proven as below, the marker part of its torn tail
  (R48). For the newest segment of each stream that no seal marker proven on the device ends, the
  records past the highest synced-through offset any verifying record in it names: each read
  through the cache first, payload included, and checked against both checksums, one that fails
  there going back to T7.4's torn or torn-slot rule; then one sync of the segment; then each
  record that verified re-read through `ReadAtDirect` and checked again (R47). Each record ends
  as *on the device* (stays where it is), *lost by the device* (verified at the first read only:
  re-appended by T7.14 from that first copy, and its segment ended, R51), or *torn*; a tail
  whose sync fails hands every record of it to T7.14 as a failed window. `Journal.Close` seals
  nothing, so every reopen runs this phase over each stream's newest segment; its cost is one
  sync and one direct read of the writes past the last proven sync point per stream (RFC 28
  §3.1). Nothing is appended here.
- **Acceptance criteria:**
  - A and B written and not synced, the process killed with the seam keeping them as a dead
    process's cache would: the open syncs once, re-reads both past the cache, and both read back
    with their numbers, not re-appended.
  - The same with B's payload torn in the cache as on the device: B is torn, not reported, and A
    reads back.
  - The same with B whole in the cache and damaged on the device: B is handed to T7.14 from its
    first copy, never dropped, and its segment ended.
  - A seal marker in a segment no successor names, served from the cache and not the device, with
    the reopen's sync succeeding without writing it: the marker is read from the device, the
    segment is not taken as sealed, and its tail is proven.
  - A sealed segment whose marker a successor names is not re-read.
- **Tests:** the in-place half of R74's "unproven tail verified at open" (`TestRFC28A_UnprovenTailVerifiedAtOpen`) and the reopen-in-place half of "a seal marker only the cache holds" (added here, §9); the torn payload (catches a first read skipped, which would refuse the open on a torn write every retry meets); the device-only damage (catches an open that trusts what verified through the cache); the cached marker after a sync that wrote nothing (catches a marker proven by a cache read after the sync, R48); a named predecessor's marker (catches a re-read nothing requires).

### T7.10 Cutting, sealing and ending, and the open's loss records

**Subtask T7.10 estimate: 4.5 man-days.**

- **Track:** A. **Depends on:** T7.6, T7.7, T7.9, T7.14, T7.15; E5's seal writer (T5.7) and its
  end of a segment (T5.10); E6's `Lose` with `AtOpen` and `Deferred` (T6.2) and `WaitDurable`
  (T6.1); E8's open reserve class (T8.6).
- **Covers:** R27 (the reserve), R43, R50, R51, R52, R54 (loss records), R65, R75.
- **Deliverables:** `internal/recovery/seal.go`: for each *torn tail* segment, truncation at the
  start of the torn tail (never below a verifying record or the highest synced-through offset),
  a sync of the truncation, and a seal through T5.7's writer with the entries the scan gathered,
  counted in `torn_tails` (R50, R65); for each *ended* segment, the same truncation and no seal
  marker, the segment left as a failed one for T7.11 (R51); a sync that fails here leaves its
  segment failed and unsealed (R52). Every stream then continues in a new segment, naming no
  predecessor after an ended one. After `New` (D1), each record of R27 goes to `Lose` marked
  `AtOpen`, first and never deferred, while the open reserve lasts, the rest noted for T7.11's
  emptying; then each record dropped as corrupt that no loss record on disk names goes to `Lose`
  with its synced flag, marked `AtOpen` while the reserve lasts and `Deferred` past it; the open
  waits on `WaitDurable` for every appended one (D9, R43, R54). RFC 1 §6's `decision:` on the
  open reserve goes at the deferral. `Open` now returns the journal for every directory whose open
  ends no segment; one that would end a segment is refused with `ErrNotImplemented` naming the
  segment until T7.11. The conformance tests this turns green leave `known.yaml`.
- **Acceptance criteria:**
  - A torn last record is cut, the segment sealed with a verifying catalog, `torn_tails` is one,
    and a write after the reopen synced and crashed again reads back.
  - A truncation never goes below a record that verifies.
  - A recycled segment's old bytes past its new records are cut as a torn tail, not reported.
  - A corrupt dirty record found at open is in `Losses` as data loss, raises the generation, and
    its loss record is durable before the open returns; a second reopen reports nothing new.
  - At an exhausted headroom, a corrupt synced record in a sealed segment gets one synced loss
    record on the open reserve, the content it superseded is never served, and a third open
    appends no second loss record.
  - With more corrupt records than the open reserve holds, the open serves; each loss past the
    reserve is published marked deferred, its extent reads as lost, and a synced one is yielded
    by `Files` and `Since` until `Settle`.
  - The public `Open` of a journal that was closed cleanly, or killed with only torn tails,
    returns a journal that serves.
- **Tests:** the rows of R75 (`TestRFC28C_Rebuild`, `TestRFC28C_TornTailCutOff`, `TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead`, `TestRFC1D_MetricsAreReachable`, and the sealed-segment half of "an open at an exhausted headroom", added here, §9); the second crash after the cut (catches an append after torn bytes,
  RFC 28 §3.3); the loss-record durability before return (catches a loss lost again at the next
  crash, R43); the exhausted headroom (catches the open's loss records drawn on the headroom,
  which would refuse the open, R54); the deferral past the reserve (catches a deferred loss
  appended anyway, or one `Since` does not yield); the runner after the `known.yaml` change (rule 6).

### T7.11 Emptying the segments the open ends

**Subtask T7.11 estimate: 4.5 man-days.**

- **Track:** A. **Depends on:** T7.10, T7.14; E5's retirement (T5.9) and pool (T5.5); E9a's
  `Held` and `Droppable` (T9.1).
- **Covers:** R27 (past the reserve), R43 (deferred records written), R53, R54 (copies), R76.
- **Deliverables:** `internal/recovery/empty.go`: once T7.10's loss records are durable, retires
  every attached segment E9a's `Held` reports not held, never a spare, never one holding a
  deferred loss's dropped record, recycling or unlinking each as T5.9 decides, each retirement
  refilling the reserve before the next emptying (D7); then, one ended segment at a time — and
  each segment that still holds a record of R27 past the open reserve — hands T7.14's copier the
  records repack would carry forward by `Droppable`, each payload verified whole before it is
  copied (RFC 28 §2.3), leaving out the records of R27, skipping a record whose copy already
  verifies in a segment the open neither ends nor saw fail (which then stays held), and adding the
  loss record of a record that fails only on this read with no other verifying copy and of each
  deferred loss whose dropped record the segment holds; then unlinks the segment and syncs the
  directory. On a second refusal or no spare, the open fails with that error and leaves the
  segment. RFC 28 §3.3's `decision:` on emptying whole, and RFC 1 §6's `ponytail:` on a loss
  record's bytes in a copy, go here. The `ErrNotImplemented` of T7.10 is removed, and with it
  T8.15's marker (D1). Adds the conformance test of RFC 28 §4.2's row "an ended segment is
  emptied once" (§9).
- **Acceptance criteria:**
  - Every record an ended segment held reads back after the open, once, with its numbers; the
    ended segment is gone and the directory synced.
  - Every attached segment that is not held is retired before the first ended segment is
    emptied; a dead segment sealed beside an ended one is gone after the open.
  - Crashed at every step of the emptying with writes reordered, and reopened twice (process
    killed, then power lost after the second open's unlink), no record is held twice and each
    corrupt record's loss is named by exactly one loss record.
  - A deferred loss whose dropped record sits in an ended segment is written into the copy, and
    the next open finds it on disk and defers nothing.
  - More records failed by a read-only journal than the open reserve holds: the reopen in place
    serves, appends none of their loss records past the reserve, and their segments are gone with
    the records left out.
  - A target whose sync fails sends its records to another spare and no loss record names them;
    a second failure fails the open with the sync error, takes no third spare, and leaves the
    ended segment on disk.
  - A journal at its maximum whose open ends a segment still opens.
- **Tests:** the rows of R76 (`TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead`, the ended-segment half of "an open at an exhausted headroom", and "an ended segment is emptied once", added here); the crash at every step (catches a copy skipped only because the
  page cache held it, or an unlink before the loss records were durable); the dead segment beside
  an ended one (catches a superset retirement that leaves the emptying short of spares, D7); the
  deferred loss in a copy (catches a segment retired without its deferred record, which would
  bring the dropped content back at the next open); the read-only reopen past the reserve
  (catches a reopen refused by the headroom, R27); the at-maximum open (catches copies charged to
  a share, R54).

### T7.12 The open's statistics and span

**Subtask T7.12 estimate: 2 man-days.**

- **Track:** A. **Depends on:** T7.6; E8's `Stats` aggregation (T8.13).
- **Covers:** R45 (kept until close), R63, R64, R66, R67, R77.
- **Deliverables:** `internal/recovery/stats.go`: `Recovery` (segments by catalog, by scan, the
  open's duration on the injected clock), `RejectedSegments`, the `TornTails` and `ScanResyncs`
  counters, set once at open and read by `Stats` without a walk; `DamagedSegments` keeps a segment
  the open emptied or retired counted until close, and the segment is marked damaged and retired
  in E5's set so E9a's `Inspector.Segments` keeps its row under its old id until close. The
  `open` span's phase children carry the segment counts as attributes.
- **Acceptance criteria:**
  - After an open that read k segments by catalog and m by scan, `Recovery` says k and m and a
    duration that the test clock set.
  - A foreign segment and a torn tail each move their field in one `Stats` call.
  - A damaged segment the open emptied still counts in `DamagedSegments` until `Close`.
  - `Stats` after open allocates and takes no lock as it did before this epic.
- **Tests:** the row of R77 (`TestRFC1D_MetricsAreReachable`); the emptied damaged segment (catches a gauge lowered at unlink);
  E8's `Stats` cost check rerun with these fields (catches a walk, R67).

### T7.13 The crash and damage matrix, and the known-failure burn-down

**Subtask T7.13 estimate: 3.5 man-days.**

- **Track:** A. **Depends on:** T7.11, T7.12.
- **Covers:** R78.
- **Deliverables:** the RFC 28 §4.4 matrix run over the public `Open`: lost, torn and reordered
  writes at every sync point of `WriteAt`, a seal, each directory sync and `Open`'s own (the
  unnamed marker's and the tail's syncs, the tail's re-appends, the truncation and its seal, a
  re-append of the emptying, a spare's zeroing, the loss records' sync, the unlink), and a bit
  flipped in every region but a record header whose flip leaves a range that names no record,
  which waits for E7b (D14); each reopen compared with E2's reference model as §4.4 says; E2's
  model run with crashes from fixed seeds. Every conformance test this epic turns green leaves
  `known.yaml`; every entry it does not turn green and that waits only on E7b, E9a or E9b is
  re-pointed to that epic's issue (§9).
- **Acceptance criteria:**
  - Every case of the matrix ends with the reopened journal agreeing with the model, a fault
    excusing only what RFC 28 §4.4 lets it excuse.
  - A header flip the matrix leaves to E7b refuses the open naming the range and serves nothing.
  - The journal's own seam order holds whatever the faults: a successor taken only after its
    predecessor's seal sync, an ended segment unlinked only after its copies' sync, a segment
    retired at open only after the open's loss records' sync.
  - `known.yaml` names no test this epic turned green, and the runner passes.
- **Tests:** `TestDamage_LostWrites`, `TestDamage_TornWrites`, `TestDamage_ReorderedWrites`,
  `TestDamage_BitFlip` (its regions but the excepted header flips), `TestDamage_SegmentDeleted`
  and the model's crash seeds (each catches a recovery that comes back describing something other
  than what is on disk); the excepted header flips asserting the refusal (catches a range served
  before the reconcile exists); the seam-order assertions (catch an ordering bug a fault would
  excuse).

### T7.14 Re-appending what the device lost

**Subtask T7.14 estimate: 3.5 man-days.**

- **Track:** A. **Depends on:** T7.9; E5's pool (T5.5), seal writer (T5.7) and end of a segment
  (T5.10); E6's `WaitDurable` (T6.1) and failed segment (T6.3).
- **Covers:** R49, R51 (a re-read that fails), R54 (re-appends), R74 (the re-append halves).
- **Deliverables:** `internal/recovery/copy.go`: the copier T7.11 reuses (D4, with its
  `decision:` marker). It writes whole records, never merged, keeping their sequence numbers and
  versions, into a spare taken from the repack reserve, never a share's limit (R54), seals the
  spare and syncs it; on a sync that fails it marks the target a failed segment and writes the
  same records once more into another spare; a second refusal in a row, or no spare ready and none
  to be made, fails the open with that error and leaves the source in place. For the tail: each
  record T7.9 found *lost by the device* is re-appended from the copy its first read verified,
  and the segment it came from is ended (R51); a tail whose sync failed has every record
  re-appended the same way and its segment is a failed segment, never sealed (R49). The copies'
  own seal proves them synced to a later open.
- **Acceptance criteria:**
  - B whole in the cache and damaged on the device: after the open B reads back from its
    re-append, with its numbers, and its old segment is ended.
  - A tail whose sync fails at the seam: every record of it reads back from the re-appends, and the
    segment is never sealed.
  - A read-only journal reopened in place with failed records past the cache: the records it did
    not fail read back from the device or from a re-append, and the reopen serves rather than
    refusing.
  - A re-append target whose sync fails: the records go to a second spare and read back; a second
    failure fails the open and leaves the source segment on disk.
  - A journal at its maximum after the process was killed still opens.
- **Tests:** the re-append half of R74's "unproven tail verified at open" and of "a seal marker only the cache holds"; the device-only damage (catches a record re-read from the device instead of its first copy, which would drop an answered write); the failing tail sync (catches a failed window resolved by trusting the cache, RFC 1 §5.3); the reopen in place (catches the refusal every 30 s a re-append-less open would meet); the failing target sync and second refusal (catches a third spare taken, or a source unlinked before its copy); the at-maximum open (catches re-appends charged to a share, R54).

### T7.15 Sync marks at open

**Subtask T7.15 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T7.4, T7.6, T7.7, T7.14.
- **Covers:** R6 (above the marks), R30 (the marks), R34, R35 and R36 (below `Through`), R44 (a
  lost tail, a range below `Through`), R45 (with marks), R55 (a mark's segment), R60–R62, R79.
- **Deliverables:** `Open(ctx, dir, floor, expect, marks []SyncMark)` and `SyncMark` as RFC 1 §3
  writes them, each mark keyed by its `Stream` (a stream with no mark has nothing proven by one), passed to `recovery.Run`; `Stats.SyncMarks`. `internal/recovery/marks.go`: the
  marks' `Through` added to T7.4's proof, so a bad record or zeros below it in its segment are
  corruption, never a torn slot, and a refused range there in a segment whose catalog does not
  verify is a range that names no record (R35, R36, R44, D14); the next sequence number and the
  segment-id allocator raised above every mark's `Sequence` and `Segment` (R6, R30); a segment
  holding no verifying record whose id a mark names withheld from T7.7's spare test and ended for
  T7.11's emptying (R55, R61); then, after T7.14's re-appends, the lost-tail check of R62, a mark
  whose segment is not on disk read as retired, with RFC 28 §3.3's `ponytail:` on that reading;
  a lost tail fails the open with `ErrNotImplemented` naming the mark (D13, with its `decision:`
  marker); otherwise each stream's entry in `Stats.SyncMarks` is set to what the open recovered
  (R34). `DamagedSegments` counts a torn header or tail a mark proves synced, never a torn slot
  above every mark's `Through` (R45).
- **Acceptance criteria:**
  - A mark whose `Through` stops at A's start while C completed: A torn reads as a torn slot,
    nothing reports it and the content A would have overwritten reads; A's header torn counts no
    range.
  - A record rotted below a mark's `Through` in a segment with no verifying catalog fails the
    open naming the range.
  - C lost from the device after its `Sync` returned and its mark was recorded: the open fails
    naming the mark and appends nothing; the same with C re-appended by T7.14 from the cache does
    not.
  - A mark naming a segment id above every one on disk: the next segment made after the open takes
    a higher id, and the next sequence number exceeds the mark's.
  - A segment the mark names holding no verifying record is never re-adopted, and is gone after
    the open.
  - After the open, `Stats.SyncMarks` gives each stream's last verifying record, a re-append
    included.
- **Tests:** the torn-slot, torn-header and counter halves of R79 ("a mark proves only up to `Through`", added here, §9); the rot below `Through` (catches a mark ignored, which would cut a synced record as torn, R36); the lost tail with and without a cached copy (catches a lost tail read as torn, R62, or a re-append counted as lost); the mark's empty segment (catches it re-adopted as a spare, where a re-append would land and hide the loss, R55); the id and sequence above the marks (catches a drained directory reissuing them, R6, R30); `Stats.SyncMarks` after an open with a re-append (catches the mark of the old segment reported).

## 7. Order of work

```mermaid
flowchart TD
  S3(["S3 walking skeleton"]):::sync --> T7_1["T7.1 Phases and attach"]
  T7_1 --> T7_2["T7.2 Rebuild by scan"] --> T7_3["T7.3 Rebuild by catalog"]
  T7_2 --> T7_4["T7.4 Torn against corrupt"]
  T7_3 --> T7_4
  T7_4 --> T7_5["T7.5 Loss, forget, copies"] --> T7_6["T7.6 Counters, tags, assembly"]
  T9_1(["E9a T9.1 liveness tracker"]):::sync -.-> T7_6
  T7_1 --> T7_7["T7.7 Spare re-adoption"]
  T7_4 --> T7_7
  T7_8["T7.8 Direct reads"] --> T7_9["T7.9 Unproven tail, unnamed marker"]
  T7_4 --> T7_9
  T7_9 --> T7_14["T7.14 Re-append what the device lost"]
  T7_4 & T7_6 & T7_7 & T7_14 --> T7_15["T7.15 Sync marks"]
  T7_6 & T7_7 & T7_9 & T7_14 & T7_15 --> T7_10["T7.10 Cut, seal, end, loss records"]
  T7_10 --> C1(["after T7.10: Open reopens a journal that ends no segment"]):::sync
  C1 --> T7_11["T7.11 Emptying ended segments"]
  T7_14 --> T7_11
  T9_1 -.-> T7_11
  T7_6 --> T7_12["T7.12 Statistics and span"]
  T7_11 --> C2(["after T7.11: every crash reopens"]):::sync
  C2 & T7_12 --> T7_13["T7.13 Crash matrix and burn-down"]
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

- **Until the checkpoint after T7.10**, the public `Open` of a directory holding segments still
  returns T8.15's `ErrNotImplemented` after the attach pass, and T7.2 to T7.9, T7.14 and T7.15
  are tested through `recovery.Run`.
- **After T7.10 (C1)**, a journal closed cleanly or killed with only torn tails reopens; an open
  that would end a segment is refused naming it, which never serves wrong data.
- **After T7.11 (C2)**, every crash the seam can make reopens, except the ranges D13, D14 and
  D15 refuse, and the engine's start can rely on `Open`. E9a's crash tests wait for C2: a lost
  copy before a later record is a torn slot, which ends its segment, and the open before C2
  refuses that.
- T7.8 needs nothing of this epic and can start at once; T7.12 can run beside T7.7 to T7.11.
- T7.6's registration and T7.11's retirement wait for E9a's T9.1, which track B builds first
  after S3.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T7.1, T7.2, T7.3 | 10.5 | — | the attach pass, the open's reads and both sources of the index, read-only and behind `ErrNotImplemented` |
| 2 | T7.4, T7.5, T7.6 | 12.25 | PR 1; E9a's T9.1 | every decision the rebuild takes, ending in a state `New` assembles |
| 3 | T7.8 | 2.5 | — | the per-OS direct reads, open beside PR 1 and PR 2 |
| 4 | T7.7, T7.9, T7.14 | 10.5 | PR 2, PR 3 | spares, the tail's and the unnamed marker's proof, and the copier that re-appends what the device lost |
| 5 | T7.15, T7.10 | 7.5 | PR 4 | the marks, then cuts, seals and the open's loss records; `Open` turns on (C1) |
| 6 | T7.11, T7.12, T7.13 | 10 | PR 5; E9a's T9.1 | the emptying, the open's statistics, and the crash matrix that proves the whole (C2) |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The surface S1 froze lacks the open's names.** `Open` on the programme branch takes
  `(ctx, dir, floor, expect, opts...)`, and `SyncMark`, `SegmentRange`, `*UnreadableRangesError`,
  `ErrUnreadable` and `Stats.SyncMarks` are not declared. RFC 1 §3 declares all of them, so this
  epic adds them as written (rule 11): T7.1 the error types, T7.15 the `marks` argument and
  `SyncMark`, which changes T8.15's `Open` and E2's fixture. **Decides:** E1's and E8's owners,
  whether to add them ahead of this epic. **Blocks:** T7.15's signature change, nothing else.
- **Conformance rows with no test yet.** RFC 28 §4.2 lists "a seal marker only the cache holds",
  "a mark proves only up to `Through`", "an open at an exhausted headroom", "unreadable is not
  empty" and "an ended segment is emptied once", and `pkg/block/journal/conformance` has no test
  for any of them. T7.9, T7.15, T7.10, T7.7 and T7.11 add the halves this epic can pass; the
  halves that need held unread records or the reconcile wait in `known.yaml` for E7b.
  **Decides:** E2's owner, for the traceability matrix and `known.yaml` entries if they land
  before this epic. **Blocks:** nothing.
- **`known.yaml` attributions.** Most conformance tests that need a reopen are attributed to the
  issues of epic 8 or epic 9, and four to epic 7's. The entries whose last missing piece is this
  epic should name E7a's issue, and the rest E7b's, E9a's or E9b's. **Decides:** E7a's owner, at
  T7.13. **Blocks:** nothing.
- **Holds before E7b.** D6 refuses an open that finds a hold, unhold or stamp record. If E9b's
  holds ship before E7b, every journal that took a hold stops reopening. **Decides:** the owners
  of E7b and E9b, by ordering them. **Blocks:** nothing here.
- **The refusals E7b withdraws.** D13, D14 and D15 keep a journal closed where RFC 28 opens it
  and owes a reconcile or holds an unread record: a lost synced tail, a rotted header with no
  verifying catalog, a truncated named predecessor, a rot below a mark's `Through`, a read that
  keeps failing. None serves wrong data, and each is retried by the engine's start. **Decides:**
  the owners of E7b, how soon it follows. **Blocks:** nothing here.
- **The liveness hand-off with E9a.** E9a's D3 asks the rebuild for each forget record's set of
  segments, not a count; R32 and T7.6 do so, and also hand over unsynced loss records with the
  segments holding the record they name. T7.11 asks `Held` and `Droppable` before it retires or
  carries anything, so a deferred loss must keep its dropped record's segment held in E9a's
  tracker (RFC 1 §6). The hook's exact signature is T9.1's. **Decides:** E9a's owner, with this
  epic's. **Blocks:** T7.6's registration and T7.11, which land after T9.1.
- **A direct read the filesystem refuses** (E3's P12). D11 fails the open until RFC 28 says
  whether to refuse, fall back or count. **Decides:** RFC 28's owner. **Blocks:** nothing; D11
  stands meanwhile.
- **Runners for macOS and Windows** (E3's P22). T7.8's per-OS bodies compile everywhere and are
  exercised only on Linux in CI until a runner exists. **Decides:** CI owner. **Blocks:** nothing.
