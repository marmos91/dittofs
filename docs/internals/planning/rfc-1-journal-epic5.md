# Epic 5: journal v2 segments, streams and accounting

**Sources:** [RFC 28](../rfcs/rfc-28-journal-format.md) §2.2, §2.3 (the header's synced-through field), §2.4 and §2.5
(the writing side), §3.1 (idle seal) and Appendix A (allocation); [RFC 1](../rfcs/rfc-1-journal.md) §3.7, §3.9, §5.1,
§5.3, §6 (allocation, spares, refusals), §7.1, §7.3, §7.4 and §8.2–§8.7; the descriptor and
stream settings of [RFC 13](../rfcs/rfc-13-configuration.md) Appendix B.
**Track:** A.
**Epic 5 estimate: 11 subtasks, 33.25 man-days.**
**Depends on:** sync point **S2, red suite in CI**; epic 1's `core` types and settings, and its
storage seam (`storage.FS`, `storage.File`); epic 2's crash-simulating seam, fixture and
profiler; epic 3's segment header and names, record, seal marker, catalog entry and trailer
codecs, and the OS `Allocate`; epic 4's per-segment byte accounting.
**Blocks:** epic 6 (`Sync` works on this epic's streams), epic 7 (recovery reads the segments
this epic writes, and re-adopts its spares), epic 8 (the data path appends and reads through
this epic's streams; its integration tests wait for the minimal stream) and epic 9 (reclamation
retires segments through this epic's executor).

---

## 1. Goal

Epic 5 is the writing side of the journal's files: the segments, the append streams that fill
them, the spare pool that supplies them, the seal that closes them, the retirement that
returns them, and the accounting that keeps count of every byte they occupy. It does no
durability policy (epic 6), no recovery (epic 7) and no index (epic 4).

**At the end of the epic:**

- a stream appends records to a segment at positions reserved in order, splits a write at the
  segment cap, rolls to a spare and reads back what it wrote, on real storage;
- a spare is made whole or not at all, at every crash point of the crash-simulating seam;
- a seal writes its marker, footer and trailer in the order the format requires, in two
  syncs for a full seal and three for an early one;
- a retired segment is unlinked or recycled by the pool's level at that moment, never
  readable by a reader that still holds its old location, and leaves the accounted footprint
  exactly once;
- descriptors are bounded per process and reopened on a miss;
- `Segments`, `Sealed`, `UsedBytes`, `HeadroomBytes`, `RepackReserveBytes`, `OpenDescriptors`,
  `DescriptorLimit`, `spare_refusals`, `segment_reopens`, `freed_bytes` and the double-retire count are
  served from maintained counters.

### 1.1 In scope

- The segment set, ids and the footprint ledger, including per-share record bytes (T5.1).
- The descriptor cache, the per-process descriptor budget and the drain that makes
  reclamation wait for readers (T5.2).
- Making a segment, whole and durable, and the ids (T5.4a).
- Append streams: position reservation, the split at the cap, the synced-through value a
  record carries, the positioned read (T5.4b), the roll and several streams (T5.6).
- The spare pool, its classes and its refusals (T5.5, T5.6).
- The seal, the catalog and the free tail, and the idle seal (T5.7, T5.8).
- Retirement by unlink or recycle, and the executor behind it (T5.9).
- Ending a segment after a failed append (T5.10).
- Refilling the pool from retirements, and closing the set (T5.11).

### 1.2 Out of scope

- Byte layouts, checksums and the slot scanner: epic 3. This epic calls the encoders.
- `Sync`, group commit, the sync timer, failed-sync windows, loss records: epic 6. This epic
  provides the stream's synced-through value, the end of a segment, and the records a window
  holds; epic 6 decides what a window does.
- `Init`, `Open`, rebuild, torn tails, unlinking a stray `.spare.tmp`, re-adopting spares
  (this epic's pool accepts one, T5.5), orphans and the footer's use at recovery: epic 7. Sealing the newest segment of a stream at
  recovery (RFC 28 §2.5, §3.1) is epic 7's too: it calls T5.7's seal writer on the entries it
  scanned. The platform refusals of RFC 28 Appendix A (copy-on-write, subvolume, cache-flush
  policy) are epic 3's probes, applied by epic 7 at `Init` and open. This epic takes the
  largest id the directory names as an input at open, for its id allocator.
- The file append serialisation point, reservations and `Reserve` (this epic's pool lets a
  reservation hold spares, R80), the share limit, the index bound, the headroom half rule,
  the order in which refusals are named, never evicting to satisfy a reservation, `Stats`
  aggregation, publishing an extent only after its record is written and the other atomicity
  rules of RFC 1 §8.4, the order of the six lock domains and its recorder (this epic owns the
  segment append domain, and T5.2's reference drain stands for the storage guard), `Handle.Close` and the
  journal-level refusal of operations after close: epic 8.
- When a segment is not held and when its removal records may be dropped, `Repack`, pins,
  `Release`, and handing a segment over for retirement only once the records that left it not
  held (its release records included) are durable: epic 9. This epic provides what they call:
  the retirement executor, the claim and the durable-unlink signal.
- Wiring the pool target, the stream count and the descriptor limit to configuration: epic 11.
- A footer for a segment sealed by a previous process (a MAY): not taken (D4).
- Compressing the `FileID` column of the footer (a MAY): not taken, the layout defines none.

### 1.3 Names used here

- **E1**, epic 1, interfaces: the `core` types and settings, the storage seam.
- **E2**, epic 2, red e2e suite and test foundations: the crash-simulating seam, fixture,
  reference model and profiler.
- **E3**, epic 3, on-disk format and platform layer: the codecs, names and the OS storage.
- **E4**, epic 4, placement index and versions: the per-segment held and retained bytes.
- **E5**, this epic.
- **E6**, epic 6, durability and loss: `Sync`, failed windows.
- **E7**, epic 7, recovery and identity: `Init`, `Open`, rebuild, spare re-adoption.
- **E8**, epic 8, data path and capacity: `WriteAt`, `ReadAt`, reservations, `Stats`.
- **E9**, epic 9, offload, reclamation, holds and tags: `Release`, `Repack`, pins.
- **E10**, epic 10, start and initialisation: the start check and the device configuration.
- **E11**, epic 11, the engine on the RFC 16 store: the wiring of configuration.
- **S1**, interfaces frozen: E1's exported surface no longer changes.
- **S2**, red suite in CI: E2's suite runs in CI from every pull request.
- **S3**, walking skeleton: a minimal append stream from this epic, one stream with no pool
  and no seal, plus E8's `WriteAt` and `ReadAt`, so the first conformance tests turn green.
  **T5.1, T5.2, T5.4a and T5.4b are this epic's half, about 12.25 man-days.**
- **S4**, core complete: E3–E9 done.

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

### 2.1 How the rules apply

| Rule | Subtasks | How |
| --- | --- | --- |
| 1 Unit tests | all | every R-number has a test that can fail. Subtasks use E3's codecs, E4's per-segment accounting and E2's crash-simulating seam, and fake nothing of their own |
| 2 Fuzz | T5.1 | there is no parser of bytes here: the footer reader is E3's, and this epic only writes. T5.1 fuzz-drives sequences of create, shrink, claim and retire against an oracle of the footprint |
| 3 Metrics and tracing | T5.1, T5.2, T5.4a, T5.5, T5.6, T5.9 | `Segments`, `Sealed`, `UsedBytes`, `HeadroomBytes`, `RepackReserveBytes`, `OpenDescriptors`, `DescriptorLimit`, `segment_reopens`, `spare_refusals`, `freed_bytes`, the double-retire count and, once RFC 1's `Stats` gains it, the `Spares` gauge are maintained at the event that changes them and read without a walk. The double retire logs at `Warn` through the injected logger. **No span:** a refused reservation, the one span-worthy operation near this area, is traced by E8, which names the limit that refused. A seal, a recycle and a spare's making are not on the list |
| 4 Profiling | all | every test runs under E2's profiler. T5.2, T5.8 and T5.9 are where descriptors and goroutines could leak (the cache, the idle sealer, the retirement worker). T5.4b asserts zero allocations per position reservation, outside `-race` |
| 5 Public and internals, `-race` | T5.1, T5.2, T5.4a, T5.4b, T5.5, T5.6, T5.7, T5.9, T5.11 | internal API now: the public `WriteAt`, `ReadAt` and `Stats` behaviour is tested in E8, their consumer. Concurrency is exercised under `-race` where subtasks list it |
| 6 Known failures | none | every conformance test needs `Open`, which E7 and E8 wire. This epic turns none green and adds none |
| 7 v2 ignores v1 | all | enforced by the import test. Nothing of `pkg/block/journal` is read |
| 8 Real storage | T5.2, T5.4a–T5.11 | every test of a sync, a rename, an unlink or a crash runs on a real directory through E2's seam. T5.1 is pure logic and tests in memory |
| 9 Comment markers | T5.1, T5.2, T5.4a, T5.4b, T5.5, T5.6, T5.7, T5.8 | see §4: each decision names its marker and where it goes. The RFC's `ponytail:` on the constant segment size goes at the cap in T5.6, and its `decision:` on zero-filling goes where T5.4a calls `Allocate`. The `decision:` on the 30 s idle time and `journal.idle_seal_bytes` goes at the idle constants in T5.8 |
| 10 A test can fail | all | each test in §6 names the bug it catches |
| 11 Exports | all | `journalv2` gains no symbol: everything is in `internal/segment`, and the one exception, `core.DescriptorBudget`, is already declared by E1 |
| 12 Created with its consumer | all | `internal/segment` lands with T5.1, and each file lands with the subtask that uses it. Until E8, tests are the only consumer: the package is logic over the storage seam tested on its own (rule 1), not a placeholder |

## 3. Requirements and invariants

Every **MUST** and **MUST NOT** that RFC 1 and RFC 28 place on this epic, the SHOULDs it
adopts, and the bounds and test rows that name its area, numbered so that subtasks and tests
can cite them. Obligations that another epic carries out are not listed: §1.2 names the
epic. A mechanical search for the key words over the owned sections found every one of them
below or in §1.2.

**Segment layout and streams**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A sealed segment is never appended to again | MUST NOT | RFC 28 §2.2 | T5.7, T5.10 |
| R2 | A write that does not fit what remains of the current segment is split at the cap into records, one per segment, each carrying its own file offset and the write's version. No record spans two segments | invariant | RFC 28 §2.2 | T5.4b, T5.6 |
| R3 | A segment is created at its cap and allocated whole, by the platform's allocation call or by writing zeros. The file never grows | invariant | RFC 28 §2.2, Appendix A | T5.4a |
| R4 | Bytes of an allocated segment that no record has reached read as zeros | invariant | RFC 28 §2.2 | T5.4a |
| R5 | Concurrent appends to one stream write at positions reserved in order. A write that fails returns its error and is never acknowledged, and its slot is left as it is | invariant | RFC 28 §2.2 | T5.4b |
| R6 | Appends of different `FileID`s proceed concurrently, on one stream and across streams. The number of independent streams is the configured stream count | invariant | RFC 28 §2.2, RFC 13 App. B | T5.4b, T5.6 |
| R7 | A bounded number of segments per stream is open for append at one time. Every other segment is sealed | invariant | RFC 28 §2.2 | T5.6 |
| R8 | Every segment begins with a header, written once when a stream takes it, naming the journal identity, the segment's own id, its predecessor (or none) and the stream number. The stream's next sync makes it durable | MUST | RFC 28 §2.2 | T5.4b, T5.6 |
| R9 | Segment ids start at 1 and ascend across all streams in the order segments are created | invariant | RFC 28 §2.2 | T5.4a |
| R10 | The synced-through offset a record carries is the offset in its segment up to which a sync had completed when the record was written. It never advances past a slot whose write failed or has not completed | MUST | RFC 28 §2.3, §2.2 | T5.4b, T5.10 |
| R11 | A stream seals a segment while what remains still holds the seal marker, a footer entry for every record in it and the trailer | invariant | RFC 28 §2.5 | T5.4b, T5.7 |
| R12 | Of the lock domains only the file append domain is held across I/O. Reserving a position in a segment takes the segment append domain for the reservation alone, and the record write happens outside it | invariant | RFC 1 §8.2 | T5.4b |
| R87 | Records, once written, are immutable: a record is written once at its reserved slot and never rewritten, and a recycle's zeroing touches only a retired segment's header and trailer slots | invariant | RFC 28 §2.2 | T5.4b, T5.9 |

**The spare pool**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R13 | A spare is made as `<id>.spare.tmp`: created, allocated (zero-filled where Appendix A requires it), synced, renamed to `<id>.seg`, then the directory synced. A crash leaves no `<id>.seg` or a whole one | MUST | RFC 28 §2.2 | T5.4a |
| R14 | A spare's id is drawn when it is made, and its header slot is zeros until a stream takes it | invariant | RFC 28 §2.2 | T5.4a |
| R15 | Making a segment happens off the write path. A roll costs only the take | invariant | RFC 28 §2.2, RFC 1 §5.2 | T5.5, T5.6 |
| R16 | The id of a new spare or of a recycle exceeds the id of every `.seg` and `.spare.tmp` file in the directory, those found at open included | MUST | RFC 28 §2.2 | T5.4a, T5.9 |
| R17 | The spares that back the headroom and the repack reserve are not in the content pool. A content roll and a `Reserve` never take one, and one that finds only those is refused `spare` | invariant | RFC 1 §6 | T5.5, T5.6 |
| R18 | Creating a spare reserves its cap against the journal's maximum, atomically with every other reservation. The pool grows only at `Init`, when the maximum is raised, or when no full-size retired segment is to hand | MUST | RFC 1 §6, RFC 28 §2.2 | T5.4a, T5.11 |
| R19 | A directory sync follows the creation of a segment before any `Sync` covering its records returns | MUST | RFC 28 §2.2, RFC 1 §5.1 | T5.4a |
| R20 | A roll that finds no spare ready is refused at once, with no I/O and without waiting, as `ErrNoSpace` naming the limit `spare`, never the journal's maximum | MUST | RFC 28 §2.2, RFC 1 §6 | T5.6 |
| R21 | Once making a spare has failed with `ENOSPC` and no full-size retired segment is to hand, a roll that finds no spare is refused with `ENOSPC`, until a spare is made again | MUST | RFC 28 §2.2 | T5.6, T5.11 |
| R22 | `spare_refusals` counts each refusal naming `spare` | MUST | RFC 28 §2.2, RFC 1 §3.9 | T5.6 |
| R23 | Header-only records go on drawing on the allocated headroom when no spare is ready: the pool offers them the headroom's spares. A repack copy that needs a new segment draws on the repack reserve's spares, never a content spare | invariant | RFC 28 §2.2, RFC 1 §6 | T5.5, T5.6 |
| R80 | A reservation holds spares ahead of need: spares held for it are not available to unreserved rolls, a reserved roll takes one of them, and an unreserved roll that would leave an open reservation short of its spares is refused `spare`. So a reserved write never meets the no-spare refusal while the pool can be refilled | invariant | RFC 1 §6 | T5.5, T5.6 |
| R83 | A spare whose making fails at any step is never taken. Whatever the attempt left under either name is unlinked and stays accounted at its cap until the unlink has happened, and only then is its reservation returned. A leftover that cannot be unlinked stays in the set, marked ended, until a retirement unlinks it | invariant | RFC 28 §2.2, RFC 1 §3.7, §7.3 | T5.4a, T5.9 |
| R84 | A spare found at open is accepted into a class, accounted at its cap and counted toward the headroom, the repack reserve or a stream's next segment. (The scan that finds it and the decision that it is a spare are epic 7's) | MUST | RFC 28 §3.1 | T5.5 |

**Seal, catalog and free tail**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R24 | Sealing writes a seal marker at the slot just past the last record. It is durable before a successor naming the segment is taken | MUST | RFC 28 §2.2 | T5.6, T5.7 |
| R25 | Bytes after a seal marker belong to the footer, never to records | invariant | RFC 28 §2.2 | T5.7 |
| R26 | Seal order: every record durable first, then the marker, footer and trailer written and durable, then, for an early seal, the file truncated past the trailer and that made durable | MUST | RFC 28 §2.5 | T5.7 |
| R27 | A full seal needs as few as two syncs and an early seal three, and a step may be split into more. The RFC states a floor, not a count: D11 fixes what this epic issues | bound | RFC 28 §2.5 | T5.7 |
| R28 | A sealed segment carries a catalog footer: one entry per record in it | SHOULD, adopted | RFC 28 §2.4 | T5.7 |
| R29 | The footer is written and made durable after the records it describes | MUST | RFC 28 §2.4 | T5.7 |
| R30 | The footer carries its own checksum and identifies the extent of the segment it describes. Epic 3 owns the bytes, this epic supplies the entries and the offsets | MUST | RFC 28 §2.4, §2.5 | T5.7 |
| R31 | A segment that carries a footer ends with the fixed-size trailer in its last 32 bytes: at the cap's end for a full seal, just after the footer for an early one | MUST | RFC 28 §2.4, §2.5 | T5.7 |
| R32 | The footer is never appended twice, and a footer that already verifies is never rewritten or extended | MUST NOT | RFC 28 §2.4 | T5.7 |
| R33 | A failed sync in the first two seal steps leaves the segment never sealed, and its successor names none | MUST | RFC 28 §2.5, RFC 1 §5.3 | T5.6, T5.7 |
| R34 | A failed truncation sync does not end the segment. It is sealed, a successor may be taken, and it stays accounted at its cap | MUST | RFC 28 §2.5 | T5.6, T5.7 |
| R35 | A segment sealed full is never truncated and keeps its cap, so it can be recycled as a full-size spare. A segment sealed early always returns its free tail | MUST | RFC 28 §2.2, RFC 1 §7.3 | T5.7 |
| R36 | The catalog is not placed in a separate file, and no second durable log of it is kept. Checked in review, not by a test: nothing a test could assert here fails without the diff showing it (rule 10) | MUST NOT | RFC 28 §2.4 | T5.7 |
| R37 | An active segment that has taken no append for 30 s is sealed once the bytes it holds beyond its last catalogued point pass `journal.idle_seal_bytes`. Below the threshold it stays open | MUST | RFC 28 §3.1 | T5.8 |
| R38 | No byte range inside a segment is freed, zeroed or deallocated while the segment remains, except the free tail at seal. (The torn tail cut at recovery is epic 7's. A recycle's zeroing of two slots is RFC 28 §2.2's own step, and an open point in §9) | MUST NOT | RFC 1 §7.1 | T5.7, T5.9 |
| R85 | A seal closes the segment to new reservations, waits for every slot reserved before it and places the marker just past the last completed record. A concurrent append lands before the marker or rolls to the next segment, and none lands after it | invariant | RFC 28 §2.2, §2.5 | T5.7 |

**Retirement: unlink and recycle**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R39 | Before a segment is unlinked, every descriptor cached for it is closed. Where an open file cannot be unlinked, segments are opened with sharing that permits deletion | MUST | RFC 28 §2.2, RFC 1 §7.4 | T5.2, T5.9 |
| R40 | Recycle order: close the descriptors, rename to a fresh `<id>.seg`, sync the directory, then overwrite the header slot and the trailer slot with zeros and sync. Never zero first. No zero-fill | MUST | RFC 28 §2.2 | T5.9 |
| R41 | A recycle that fails at any step is an unlink, under whichever of its two names the journal finds. The segment is never a spare | MUST | RFC 28 §2.2 | T5.9 |
| R42 | Whether a full-size segment is recycled is decided at its retirement: recycled while the pool is below its target then, unlinked otherwise. Never decided at seal | MUST | RFC 28 §2.2, RFC 1 §7.3 | T5.9 |
| R43 | A segment sealed early is unlinked when retired, never recycled. A segment ended by a failed sync, by a failed append, or by recovery without a seal is unlinked, never recycled, and only once repack has carried its synced records forward | MUST | RFC 28 §2.2, RFC 1 §5.3 | T5.9, T5.10 |
| R44 | A segment's records are durable before it is unlinked or recycled: a sealed segment's are, by the seal's first step (R26), and an ended segment's window is resolved by the caller, who hands it over with the reason `Ended`. That a release record is durable before the segment holding the record it released is retired is the caller's: epic 9 hands a segment over only once the records that left it not held are durable | MUST | RFC 1 §5.1 | T5.9 |
| R45 | An unlink or a recycle is made durable by a directory sync, and the retirement reports when it is. A removal record whose retention counted the segment is dropped only after (epic 9 applies the rule) | MUST | RFC 28 §2.2, RFC 1 §5.1 | T5.9 |
| R46 | A segment that is not held is retired by the journal on its own, once the records that left it not held are durable, without waiting for a repack to select it. This epic provides the executor and its background worker. Epic 9 decides when a segment is not held | MUST | RFC 1 §7.1 | T5.9 |
| R47 | Unlink and recycle wait for the in-flight reads of that segment, and free storage as the last step, after no reader can hold the old resolution | MUST | RFC 1 §8.5 | T5.2, T5.9 |
| R82 | A segment is retired only once its stream has left it: sealed, or ended by a failure. An active segment and a spare in the pool are never retired | invariant | RFC 28 §2.2, RFC 1 §5.3, §7.1 | T5.9 |

**A failed append ends its segment**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R48 | A failed append ends its segment, fresh or recycled: no further append, not even a seal marker. The stream continues in a new segment naming no predecessor, and the segment is never sealed | MUST | RFC 28 §2.2, RFC 1 §5.1, §5.3 | T5.10 |
| R49 | The records written to the ended segment since its last successful sync are handed to the failed-window resolution (re-append or fail: epic 6). This epic hands them over and decides nothing about them | MUST | RFC 28 §2.2, RFC 1 §5.3 | T5.10 |
| R50 | An ended segment stays accounted at its cap to the journal until it is unlinked | MUST | RFC 1 §5.3, §7.3 | T5.1, T5.10 |

**Accounting**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R51 | Accounted footprint reflects the storage the journal's files allocate: a segment at its cap from creation to unlink, and a segment shortened by a torn-tail truncation or at seal at its size afterwards | MUST | RFC 1 §7.3 | T5.1 |
| R52 | `UsedBytes` is raised before an allocation happens and lowered only after an unlink has happened, and it moves only when a segment is created, unlinked or shortened | invariant | RFC 1 §3.7, §6 | T5.1, T5.4a, T5.9 |
| R53 | Every file the journal creates is accounted, each segment's footer included, and every such file is bounded by live state, not by history | MUST | RFC 1 §7.3, RFC 28 §2.4 | T5.1, T5.7 |
| R54 | The spares of the headroom and of the repack reserve are accounted to the journal, never to a share | MUST | RFC 1 §7.3, §6 | T5.1, T5.5 |
| R55 | A record's bytes (header, payload, padding) are accounted to the share whose tag the record carries when it is written, and released when the segment holding it is unlinked or recycled | MUST | RFC 1 §6 | T5.1 |
| R56 | An unlink is idempotent: the segment's bytes leave the footprint exactly once, in the step that removes it from the set of segments the journal owns, and only if that step found it there. An unlink that finds the file gone or the segment out of the set subtracts nothing. A recycle subtracts nothing at all (R78) | MUST | RFC 1 §7.3 | T5.1, T5.9 |
| R57 | A claim taken on a segment is never released after the segment is retired | MUST NOT | RFC 1 §7.3 | T5.1 |
| R58 | A retire that subtracts nothing is counted and logged at `Warn` | MUST | RFC 1 §7.3 | T5.1 |
| R59 | The accounted footprint is checkable against the sum of the segment set's sizes, and a mismatch is reported (the check is called by epic 7's verification) | SHOULD, adopted | RFC 1 §7.3 | T5.1 |
| R60 | The counters of this area are served from maintained state, never from a walk of the segment set | MUST | RFC 1 §3.7 | T5.1, T5.2, T5.5, T5.6 |
| R78 | A recycle does not return the segment's cap: the recycled segment stays accounted at its cap, now as a spare under its new id, and only an unlink lowers the footprint | MUST | RFC 1 §6, §7.3 | T5.1, T5.9 |
| R79 | `freed_bytes` counts storage actually returned by unlinking segments, raised only after the unlink has happened | invariant | RFC 1 §3.9, §6 | T5.9 |

**Descriptors**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R61 | Open segment descriptors are bounded by `DescriptorLimit`, independently of the number of segments. The bound is per process: every journal of the process draws on one budget | MUST | RFC 1 §7.4 | T5.2 |
| R62 | A read of a segment whose descriptor is not open reopens it | MUST | RFC 1 §7.4 | T5.2 |
| R63 | The count, the bound and the reopens are reported through `Stats` | MUST | RFC 1 §7.4, §3.9 | T5.2 |
| R64 | The default `DescriptorLimit` is a quarter of the process's open-file limit | bound | RFC 13 App. B | T5.2 |

**Reclamation against readers, lock order and shutdown**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R65 | A read either observes the location it resolved, intact, or discovers that it must retry or report the extent missing. It never returns bytes from storage that has been freed or reused | MUST | RFC 1 §8.5 | T5.2, T5.9 |
| R66 | Reclamation waits for the in-flight reads of the segment it frees, and only of that segment. The mechanism is a per-segment reference drain that rejects new readers once it starts, which RFC 1 §8.5 permits in place of a guard because freeing is still the last step | MUST | RFC 1 §8.5 | T5.2 |
| R67 | A continuous stream of readers never prevents reclamation from completing: once closing a segment starts, new readers of it fail, so the drain ends | MUST NOT | RFC 1 §8.6 | T5.2 |
| R68 | Reclamation never keeps a segment unreadable for longer than one unit of work: from the start of its drain to its unlink or rename | MUST NOT | RFC 1 §8.6 | T5.2, T5.9 |
| R69 | Nothing is acquired while a reader holds a segment reference except the read itself, and reclamation holds no lock while it waits for the drain. The cache's mutex is a leaf: never held across I/O or while another lock is held. No two locks of one domain are held at once. (The recorder that checks the order across the stack is epic 8's) | MUST NOT | RFC 1 §8.3 | T5.2, T5.4b, T5.9 |
| R70 | On close, background work (idle seal, the retirement worker) is stopped and joined before the segments it touches are closed, and no storage a reader may still hold is freed | MUST | RFC 1 §8.7 | T5.8, T5.11 |
| R81 | After close, an append or a read through the segment set fails with the closed error, and in-flight ones complete or fail cleanly. The journal-level close and its refusal of new operations are epic 8's | MUST | RFC 1 §8.7 | T5.11 |
| R86 | Every piece of mutable state this epic adds names the lock domain that guards it (D13) | MUST | RFC 1 §8.2 | T5.1, T5.2, T5.4a, T5.4b, T5.5 |

**Test rows of the RFCs for this area**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R71 | A spare is whole or absent: crash at every step of making one, dropping unsynced allocations and directory changes. Every `<id>.seg` is at its cap and fully allocated, and the next spare's id is above any `.spare.tmp` id. (The unlink of a stray `.spare.tmp` at open is epic 7's) | test row | RFC 28 §4.2 | T5.4a |
| R72 | Retired segments are recycled: the seam sees a rename, then a zeroed header slot and trailer slot per recycle, and no zero-fill. A recycle happens exactly when the pool is below its target at retirement. Every segment sealed early is truncated and none sealed full is. A segment whose sync or append failed is unlinked | test row | RFC 28 §4.2 | T5.7, T5.9, T5.10 |
| R73 | Descriptors: create more segments than the bound, reads still succeed and open descriptors stay bounded | test row | RFC 1 §9.4 | T5.2 |
| R74 | Two paths retire one segment, one after the other: accounted footprint falls by its size exactly once and never goes below zero | test row | RFC 1 §9.2 | T5.1, T5.9 |
| R75 | Accounting follows the cap: the journal's `UsedBytes` does not move when records are written into an allocated segment, and is never below what the segment files allocate, including on a filesystem that allocates speculatively past the end of a growing file. (The share's counters in the same row are epic 8's) | test row | RFC 1 §9.4 | T5.4a, T5.4b |
| R76 | Unlink under read: a read of a segment concurrent with its unlink returns the pattern it should, or retries, or reports the extent missing, never other bytes | test row | RFC 1 §9.4 | T5.2, T5.9 |
| R77 | A zero slot is not the end, the write half: a failed middle write returns an error, no append reserved after the failure lands in that segment, and the stream's next append goes to a new segment. (The reopen half is epic 7's) | test row | RFC 28 §4.2 | T5.10 |

## 4. Decisions this epic takes

- **D1, one package of concrete types.** `internal/segment` holds the set, the cache, the
  streams, the pool and the retirement as concrete types, with no interface between them
  (rule 12). The storage seam is the only interface they touch.
- **D2, the synced-through value comes from an ordered-completion tracker.** RFC 28 §2.2
  says only that it never passes a slot whose write failed or has not completed. The
  stream tracks, per segment, which reserved slots have completed and which sync has
  returned, and a record carries the highest offset below which every slot completed and a
  sync covered. A `decision:` marker at the tracker states why a plain "last synced offset"
  is wrong: a later slot can complete and sync while an earlier one is still in flight.
- **D3, three spare classes in one pool.** The pool holds spares of three classes: content,
  headroom and repack reserve. Only content spares serve a roll (R17). Sizes come from the
  settings (`SpareTarget` for content, the headroom and reserve sizes from the capacity
  rules of RFC 1 §6, given to the pool by the caller). A `ponytail:` marker at the target
  says the target is static: it names the ceiling (a burst longer than one segment's fill
  time refuses `spare`) and the upgrade (a target that follows the roll rate) once
  `spare_refusals` shows it.
- **D4, no footer for a segment sealed by a previous process.** RFC 28 §2.4 permits it
  (a MAY) and a segment sealed at a crash legitimately has none. This epic does not take
  it: recovery scans such a segment. Sealing the newest segment of a stream at recovery (RFC 28 §2.5) is a different case: epic 7 does it through this epic's seal writer (T5.7). A `decision:` marker at the seal says the exemption
  holds while a scan of the tail is cheap, and what overturns it: a time to first read
  dominated by scans of such segments.
- **D5, one retirement executor, two entry points.** `Retire` runs one retirement now.
  A queue and a worker run those that epic 9 hands over once their records are durable.
  Epic 9 decides that a segment is not held, so the executor never calls the index to
  decide it; it checks only what it owns (sealed or ended, nothing claimed).
- **D6, a spare's directory entry is synced when it is made.** So taking one costs no
  directory sync. This is the rule of an open RFC 28 change (#3500); the merged text says a
  failed directory sync of a new segment is a failed sync window. Under either text no record
  is written into a segment whose directory entry is not yet durable (D10 included), so there
  is no window to re-append. A spare whose making fails at any step, a failed directory sync
  included, is never taken (R83): the file may exist under `<id>.spare.tmp` or `<id>.seg`, so
  it is unlinked under either name and stays accounted until the unlink has happened. A file
  that cannot be unlinked stays in the set, marked ended, and the retirement worker retries it
  with the reason `Ended`.
- **D7, a file's appends go to one stream.** The stream is chosen by a hash of the
  `FileID`, so one file's records stay ordered on one stream and epic 8's file append
  serialisation needs no cross-stream order. A `ponytail:` marker names the ceiling: one hot
  file cannot use more than one stream.
- **D8, the drain of a segment's references is the RFC 1 §8.5 mechanism.** A read takes a
  reference on the segment's descriptor and holds nothing else. `Close(id)` marks the segment
  gone, waits for its references and closes its descriptors; a later acquire of that segment
  fails, and the read reports the extent missing. Segment ids are never reused, so the
  rejection is permanent. A storage guard on top would never wait: any reader holding it also
  holds a reference, and `Close(id)` has already drained them. RFC 1 §8.5 allows another
  mechanism with the same guarantee, provided freeing is still the last step. The cache opens
  a file on a miss outside its mutex, and never evicts a descriptor a reference holds. A
  `ponytail:` marker at the cache says it is one mutex around an LRU, and what would make it
  worth sharding: contention on that mutex in a profile of the mixed read soak.
- **D9, the segment cap in tests.** Tests shorten the cap through `core.SetSegmentCap`
  and never through a second constant, so every size check of this epic runs at the
  production code path.
- **D10, the skeleton makes its first segment on the write path.** T5.4b calls T5.4a's
  `Make`, which returns a segment whole and with its directory entry synced, so the minimal
  stream works before the pool exists. A `ponytail:` marker says so, and T5.6 replaces the
  call with the pool's take.
- **D11, a seal issues the minimum syncs.** RFC 28 §2.5 gives a floor (two for a full seal,
  three for an early one) and permits more. This epic issues exactly the floor, so a roll
  costs no extra flush. The tests pin the floor as this epic's choice, and a merged step is
  caught by the order assertions of T5.7, not by the count.
- **D12, a header-only roll takes a headroom spare, which then becomes an ordinary
  segment.** The stream finishes it with whatever records follow, content included, and the
  worker refills the headroom class to its size, so `HeadroomBytes` dips until the refill. A
  `ponytail:` marker at the roll names the ceiling: a burst of content landing in such a
  segment spends headroom space until the refill. The upgrade, keeping the rest of that
  segment for header-only records, waits for `headroom_draws` to show it. Open point in §9.
- **D13, which domain guards each piece of state.** RFC 1 §8.2 names six domains and asks
  that every piece of mutable state name one. The segment set, the footprint ledger, the id
  allocator and the pool's counters sit under the capacity domain (store-wide counters, no
  I/O under it). A segment's write position, its catalog entries and its completion tracker
  sit under that segment's append domain, held for the reservation only (R12). The storage
  guard domain is stood for by T5.2's reference drain (D8). The descriptor cache is none of
  the six: it takes a leaf mutex of its own, never held across I/O and never while another
  lock is held. A `decision:` marker at each type names its domain. Open point in §9.

## 5. Target tree

```text
pkg/block/journalv2/internal/
  segment/
    set.go          T5.1   segment set, footprint ledger, claims, per-(segment, tag) bytes
    descriptors.go  T5.2   descriptor cache over one per-process budget, reference drain
    make.go         T5.4a  making a segment, id allocator, repair of a failed make
    stream.go       T5.4b  append stream, position reservation, synced-through tracker, read
    pool.go         T5.5   spare classes, take, hold, adopt, refill worker
    roll.go         T5.6   take a spare, predecessor, several streams, refusals
    seal.go         T5.7   seal marker, footer, trailer, truncation
    idle.go         T5.8   idle seal worker
    retire.go       T5.9   unlink, recycle, executor and worker
    end.go          T5.10  ending a segment after a failed append
    close.go        T5.11  refill from the retirement queue, closing the set
    *_test.go
  core/
    config.go       T5.2   DescriptorBudget implemented (declared by E1)
  index/
    files.go        T5.1   the retire pair (segmentGone, retireSegment), exported for this package
```

## 6. Subtasks

### T5.1 Segment set and footprint ledger

**Subtask T5.1 estimate: 2.75 man-days.**

- **Track:** A. **Depends on:** S2; E4's per-segment held and retained bytes.
- **Covers:** R50–R60, R74, R78, R86.
- **Deliverables:** `internal/segment/set.go`: the set of segments the journal owns, each with
  an id, a state (spare, active, sealed, ended), the size it is accounted at and its record
  bytes per share tag; the footprint (`UsedBytes`), raised when a segment is created and
  lowered only after its unlink; `Retire(id)` that removes the segment from the set and
  subtracts only if it found it there (an unlink); `Recycle(old, new)` that moves a segment to
  its new id as a spare, footprint unchanged, releasing its per-tag record bytes once and
  raising the spare count; `Claim` and its release, a no-op after retirement; `Check()` that
  compares the footprint with the sum of the set's sizes; the `Segments`, `Sealed` and
  double-retire counters (and the `Spares` gauge once RFC 1's `Stats` has it); a `Warn` log on
  a retire that subtracts nothing. In `internal/index/files.go` the existing retire pair is
  exported for this package: `segmentGone` for each file that held a record in the segment,
  then `(*files).retireSegment`, which wraps `segments.retire`. Nothing else of the index
  changes. A `decision:` marker at each type names the lock domain that guards it (D13).
- **Acceptance criteria:**
  - Retiring a segment twice, or from two goroutines at once, lowers the footprint once and
    counts and logs the second.
  - A claim released after retirement changes nothing.
  - `Check()` reports a mismatch when the footprint and the sum differ.
  - A segment shortened at seal is accounted at its new size; a failed or ended one keeps its
    cap.
  - A recycle moves the segment to its new id with the footprint unchanged (tested at T5.9,
    its consumer).
  - Record bytes of one tag leave with the segment that held them.
- **Tests:** a `testing.F` target of random create, shrink, claim and retire against an oracle
  of the footprint, whose seed corpus runs as a test (catches a path that forgets to subtract,
  or subtracts twice); two goroutines retiring one segment under `-race` (catches a
  check-then-act retire); `Check()` after a deliberately skipped subtract (catches a check
  that compares the footprint with itself); a test that `UsedBytes` is raised before a
  create's allocation and lowered only after its unlink, against a hook between the two.

### T5.2 Descriptor cache, budget and the read drain

**Subtask T5.2 estimate: 3 man-days.**

- **Track:** A. **Depends on:** S2.
- **Covers:** R39, R47, R60–R69, R73, R76, R86.
- **Deliverables:** `core.DescriptorBudget` implemented: the limit (0 means a quarter of the
  process's open-file limit where the platform reports one, and a fixed default where it does
  not, behind a `ponytail:`), the shared count and an acquire that refuses past the limit.
  `internal/segment/descriptors.go`: a cache whose `Acquire(id)` returns a reference to the
  segment's descriptor, or a gone error; it opens on a miss outside its mutex and counts the
  reopen, and evicts the least recently used descriptor nothing references. `Close(id)` marks
  the segment gone, waits for its references to drop and closes its descriptors: this drain is
  the RFC 1 §8.5 mechanism (D8). It exports `OpenDescriptors`, `DescriptorLimit` and
  `segment_reopens`.
- **Acceptance criteria:**
  - Two journals of one process, each over its own directory, draw on one budget: the sum of
    their open descriptors never passes the limit.
  - Reading more segments than the limit succeeds, and the open count stays at or below it.
  - A read of a segment whose descriptor was evicted reopens it and counts one reopen.
  - After `Close(id)`, the storage seam shows no open descriptor of that segment, and a later
    acquire of it fails instead of reopening.
  - A reference a reader holds prevents eviction. `Close(id)` waits for it, and only for the
    references of its own segment.
  - With readers of one segment arriving continuously, `Close(id)` returns: once it starts,
    new acquires of that segment fail.
  - A reader holding a reference reads intact bytes until it drops the reference.
  - No seam call is made while the cache's mutex is held.
- **Tests:** two caches on one budget with a bound of 4 and 16 segments each (catches a
  per-journal bound); a reader holding a reference while 16 others are read (catches an
  eviction under a reader); the seam's open-file count after `Close` (catches a leaked
  descriptor that would keep an unlinked file allocated); a read loop at a bound of 1 counts
  one reopen per miss and no more; a reader loop on one segment with a `Close` that must
  return (catches a drain that new readers can extend); a `Close` of segment A while a reader
  holds a reference to B does not wait (catches a store-wide drain); a reader's reference held
  across `Close`, which returns only after the reference is dropped and the reader's pattern
  is intact (catches a `Close` that does not wait, the bug R65 and R76 name); a hook that
  fails the test when a seam call happens under the cache's mutex (catches I/O under the leaf
  lock, R69); `-race` with 32 readers and a `Close`.

### T5.4a Making a segment

**Subtask T5.4a estimate: 2.5 man-days.**

- **Track:** A. **Depends on:** T5.1; E3's `Allocate` and names.
- **Covers:** R3, R4, R9, R13, R14, R16, R18, R19, R52, R71, R75, R83, R86.
- **Deliverables:** `internal/segment/make.go`: `Make()` reserves the cap against the
  journal's maximum atomically through T5.1's ledger, then creates `<id>.spare.tmp`, allocates
  the cap, syncs, renames to `<id>.seg` and syncs the directory, and returns the segment only
  then. The id allocator is seeded by the largest id the directory names (supplied at open,
  zero for a new journal) and draws above it. A failure at any step unlinks whatever the
  attempt left under either name, keeps it accounted until the unlink has happened, and then
  returns the reservation; a leftover that cannot be unlinked stays in the set, marked ended
  (T5.9 retires it). The RFC's `decision:` on zero-filling goes where `Make` calls
  `Allocate`.
- **Acceptance criteria:**
  - After a crash at any step the durable directory holds no `<id>.seg`, or one at its cap and
    fully allocated, and a `.spare.tmp` at most.
  - The next id is above every id the directory held, a `.spare.tmp` included.
  - A fresh segment reads as zeros past its header slot, and its header slot is zeros.
  - Two concurrent `Make` calls near the maximum reserve atomically: together they never
    exceed it.
  - A failure at any step (create, allocate, sync, rename, directory sync, with `ENOSPC` and
    with `EIO`) leaves no usable segment: whatever it left is unlinked under either name,
    stays accounted until the unlink has happened, and the reservation is then returned. A
    file that cannot be unlinked stays in the set, marked ended.
  - `UsedBytes` is raised before the allocation, and the allocated size of the files never
    exceeds the accounted size, also on a filesystem that allocates speculatively.
- **Tests:** the crash seam at each of create, allocate, sync, rename and directory sync,
  listing the directory after each drop (the RFC 28 spare row, R71); an id allocator started
  from a directory holding a `.spare.tmp` with the highest id; concurrent `Make` calls at the
  maximum (catches a read-then-decide reservation); the footprint after a failed `Make` at
  each step with each error, the unlink succeeding and failing (catches a leaked reservation,
  and an uncounted file left at its cap); a hook between the reservation and the allocation
  (catches `UsedBytes` raised late); the files' allocated size against the accounted size on
  real storage (catches accounting that trails the filesystem, R75).

### T5.4b Minimal append stream and positioned read

**Subtask T5.4b estimate: 4 man-days.**

- **Track:** A. **Depends on:** T5.1, T5.2, T5.4a; E3's record codec and segment header.
- **Covers:** R2, R5, R6, R8, R10–R12, R69, R75, R86, R87.
- **Deliverables:** `internal/segment/stream.go`: `Stream` with one active segment, made by
  T5.4a's `Make` on the write path (D10), the header written once when the stream takes it,
  and `Append(record)` that reserves a position under the segment append lock without I/O
  (R12), writes the record outside it, and splits at the cap by reporting how many bytes fit
  so the caller continues in the next segment (T5.6). The ordered-completion tracker (D2) and
  the synced-through value a record carries. `ReadAt(location, p)`: a reference on T5.2's
  descriptor, the read, the reference dropped (D8). The stream keeps its write position,
  entries and tracker under that segment's append domain (D13).
- **Acceptance criteria:**
  - Sixteen concurrent appends of different files land at distinct, increasing,
    non-overlapping positions, each record reading back byte for byte.
  - A write that straddles the cap is cut into pieces that each carry their own file offset
    and the write's version.
  - A write that fails returns its error and is never acknowledged. Records reserved after it
    carry a synced-through value below its slot.
  - The reservation allocates nothing, outside `-race`.
  - A record that exactly fills what remains is one piece.
  - A record is written only into a segment `Make` has returned.
  - Each byte range of a segment is written once, and the files' allocated size and the
    accounted size agree after the appends (R75, R87).
  - `Append` returns a record's location only after its bytes are written, so a caller can
    publish it as its last step.
  - The stream stops short of the cap by the room the seal still needs (R11).
- **Tests:** the tracker with a later slot completing and syncing first while an earlier one
  is held (catches a "last synced offset" implementation, D2); the split at the cap with the
  pieces' offsets and versions asserted; a record that exactly fills what remains of the
  segment (the edge of RFC 1 §9.2: no empty piece, no early roll); a failed write at the seam
  followed by two appends, asserting both synced-through values; the allocation count of the
  reservation; a lock-held-during-I/O check on the segment append lock (catches R12); the
  seam's write log and the allocated size against the accounted one (catches a record written
  twice, R87, and accounting that trails the filesystem, R75); `-race` with 16 appenders and a
  reader.

### T5.5 The spare pool

**Subtask T5.5 estimate: 2.25 man-days.**

- **Track:** A. **Depends on:** T5.1, T5.4a.
- **Covers:** R15, R17, R23, R54, R60, R80, R84, R86.
- **Deliverables:** `internal/segment/pool.go`: the three classes (D3) with `Take(class)`
  over T5.4a's `Make`; a refill worker that makes spares off the write path when a class
  falls below its target, with a `Stop` that joins it; `Hold(n)` and its return, which keep
  spares for a reservation ahead of need (R80); `Adopt(id, class)`, which accepts a spare
  found at open into a class and the set at its cap (R84); the `HeadroomBytes` and
  `RepackReserveBytes` figures, each as allocated now, and the `Spares` gauge once RFC 1's
  `Stats` has it; the state "making a spare failed with `ENOSPC`" that T5.6 reads.
- **Acceptance criteria:**
  - A content `Take` never returns a headroom or reserve spare.
  - `Take` makes no seam call: only the refill worker makes spares (R15).
  - Spares held for a reservation are not taken by an unreserved `Take`.
  - A spare found at open and adopted counts toward its class and is accounted at its cap.
  - `HeadroomBytes` and `RepackReserveBytes` report the allocated size of their classes.
- **Tests:** `Take(content)` with only headroom spares ready (catches R17); a `Take` with the
  seam's call count unchanged (catches a make on the write path, R15); a `Hold` followed by
  an unreserved `Take` (catches R80); an `Adopt` followed by the class counts (catches R84);
  the two bytes figures after a `Make`; `-race` with takes and a refill.

### T5.6 Roll and several streams

**Subtask T5.6 estimate: 4.75 man-days.**

- **Track:** A. **Depends on:** T5.4b, T5.5, T5.7.
- **Covers:** R2, R6–R8, R15, R17, R20–R24, R33, R34, R60, R80.
- **Deliverables:** `internal/segment/roll.go`: when a record does not fit, seal the active
  segment (T5.7), take a content spare, write its header naming the predecessor it sealed,
  and continue the split write there; the configured number of streams, the stream of a file
  chosen by a hash of its `FileID` (D7); a content roll that finds no spare ready returns
  `ErrNoSpace` naming `spare` at once, or `ENOSPC` when making a spare has failed with it and
  no full-size retired segment is to hand (T5.11 supplies that fact; until then none is),
  with `spare_refusals` counted; header-only records are offered the headroom's spares (D12)
  and a repack copy the repack reserve's, each roll naming its class; an unreserved roll that
  would leave an open reservation short of its spares is refused `spare` in its place (R80).
  T5.4b's call of `Make` on the write path (D10) is replaced by the pool's take. A `ponytail:`
  marker at the cap states the RFC's note on the constant segment size.
- **Acceptance criteria:**
  - A write straddling the cap lands in two segments, the second naming the first as its
    predecessor, and the first's seal is durable before the second is taken.
  - A record that exactly fills what remains is written in place, and the next append rolls.
  - After an injected failed sync in a seal's first two steps the next segment names no
    predecessor. After a failed truncation sync it names the sealed segment, which stays
    accounted at its cap.
  - A repack copy rolls onto a repack-reserve spare and is never refused `spare` while one is
    ready.
  - A refusal performs no I/O: the seam sees no call.
  - With no content spare but a headroom spare, a content roll is refused `spare` and a
    header-only record proceeds.
  - Two files whose hashes differ use two streams and never block each other.
  - `spare_refusals` rises by one per refused roll.
  - With a reservation holding the last content spare, an unreserved roll is refused `spare`
    and the reserved write's roll succeeds.
- **Tests:** the straddling write across a roll, asserting the predecessor and the seal's
  order against the roll in the seam's call log (catches a successor taken before the seal is
  durable); a pool emptied, then the refusal with the seam's call count unchanged; the
  `ENOSPC` variant after a failed `Make`, with nothing queued for retirement (T5.11 adds the
  queue); `-race` with 16 appenders over 4 streams and a refill; the spare-refusal counter; a
  reserved and an unreserved roll with one spare left (catches R80); the failed seal syncs and
  the failed truncation sync, each followed by a roll (R33, R34); a repack-class roll with
  only reserve spares ready.

### T5.7 Seal, catalog and free tail

**Subtask T5.7 estimate: 4.25 man-days.**

- **Track:** A. **Depends on:** T5.4b; E3's seal marker, catalog entry and trailer codecs.
- **Covers:** R1, R11, R24–R36, R38, R53, R72, R85.
- **Deliverables:** `internal/segment/seal.go`: the stream keeps one catalog entry per record
  it appended; `Seal(early)`, which takes its entries as data so that epic 7 can seal a
  scanned segment through the same writer, makes every record durable, appends the marker, the
  footer and the trailer and makes them durable, then, for an early seal, truncates past the
  trailer and makes that durable; a full seal puts the trailer at the cap's last 32 bytes and
  returns no tail. A seal closes the segment to new reservations under its append domain,
  waits for every slot reserved before it and places the marker just past the last completed
  record (R85). A segment is sealed at most once and its footer written at most once. A
  `decision:` marker for D4 sits here. R36 is checked in review, not by a test.
- **Acceptance criteria:**
  - A full seal issues two syncs on the segment and an early seal three.
  - After a full seal the file is at its cap and its trailer is the last 32 bytes; after an
    early seal the file ends at its trailer and the ledger shows its new size.
  - A failed sync in the first two steps leaves the segment unsealed and ended (the state T5.1
    holds, so no append or seal follows; T5.10 adds the hand-off of its window). A failed
    truncation sync leaves it sealed and accounted at its cap. Which segment follows each is
    T5.6's.
  - A second `Seal` writes nothing.
  - An append racing a seal lands before the marker or is refused and rolls to the next
    segment. None lands after the marker.
  - E3's footer reader accepts the footer of a sealed segment, and reports "no footer" for the
    state after a crash that kept only part of the second step.
- **Tests:** the seam's sync count for both seals (pins the floor D11 chose, and catches an
  extra flush on the roll path; a merged step is caught by the order assertions that follow);
  a crash after each step and E3's reader on each durable state; an append racing a seal under
  `-race` (catches a record after the marker, R85); a torn second step; the failure injections
  of both steps and of the truncation; a second seal's call log.

### T5.8 Idle seal

**Subtask T5.8 estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** T5.6, T5.7.
- **Covers:** R37, R70.
- **Deliverables:** `internal/segment/idle.go`: one worker per journal that, on the injected
  clock, seals early a stream's active segment once it has taken no append for the idle time
  and holds more than the idle-seal bytes beyond its last catalogued point; the stream takes a
  spare only when its next append arrives. The worker has a `Stop` that joins it, and T5.11
  calls it before the segments close. A `decision:` marker at the idle time and the threshold
  says both are unmeasured constants, and what overturns them: a time to first read dominated
  by scans of active tails, or a trickle workload whose early seals show up in footer writes.
- **Acceptance criteria:**
  - After 30 s of virtual time with more than the threshold written, the segment is sealed
    early and its tail returned.
  - Below the threshold, the segment stays open for any idle time.
  - No spare is taken by an idle seal.
  - `Stop` returns with no worker goroutine left.
- **Tests:** the test clock advanced just below and just past the idle time, and just below
  and past the threshold (catches an off-by-one on either bound); a spare count unchanged
  across an idle seal.

### T5.9 Retirement: unlink and recycle

**Subtask T5.9 estimate: 4.75 man-days.**

- **Track:** A. **Depends on:** T5.1, T5.2, T5.5, T5.7.
- **Covers:** R16, R38–R47, R52, R56, R65, R68, R69, R72, R74, R76, R78, R79, R82, R83, R87.
- **Deliverables:** `internal/segment/retire.go`: `Retire(id, reason)`. It accepts only a
  segment its stream has left: sealed, or ended (R82). An active segment and a spare in the
  pool are refused, and so is a segment with a live claim. That its release records are
  durable is the caller's: epic 9 hands a segment over only once the records that left it not
  held are durable (R44). A segment ended by a failure, or left by a failed `Make`, is
  retirable with the reason `Ended`, once the caller says its window is resolved and repack
  has carried its synced records forward (a failed `Make` has neither). The order is:
  `Close(id)` on T5.2's cache, which drains the readers and closes the descriptors; then the
  one filesystem call, an unlink or the rename of a recycle; then the rest. The choice between
  the two is made now: a segment sealed full is recycled while the pool is below its target,
  and every other segment is unlinked. A recycle renames to a fresh `.seg` name, then syncs
  the directory, zeroes the header slot and the trailer slot through a fresh descriptor,
  syncs, and hands the segment to the pool as a spare under its new id; the ledger moves it
  without changing the footprint (T5.1, R78). Any failed step is an unlink under whichever of
  the two names exists, and the footprint is lowered then. A failed unlink leaves the segment
  in the set, accounted, and the worker retries it. After an unlink, or after a recycle's
  rename, the directory is synced and the retirement reports whether that is durable.
  `freed_bytes` is raised after an unlink has happened. A queue and a worker retire the
  segments epic 9 hands over (D5); the worker has a `Stop` that joins it, which T5.11 calls.
- **Acceptance criteria:**
  - The seam sees, per recycle, a rename, a directory sync, two zero writes and one sync, and
    no allocation and no zero-fill. `UsedBytes` does not move across a recycle.
  - A segment sealed early is unlinked and never recycled. An ended segment is retired only
    with `Ended`, is unlinked and never recycled.
  - An active segment, and a spare in the pool, is refused.
  - A failed spare's leftover, marked ended, is retired with `Ended` and unlinked.
  - A failure injected at each of the four recycle steps leaves the file gone under both
    names, the pool unchanged and the footprint lowered once.
  - A crash between the rename and the zeroing leaves a file under the new name that holds the
    old life's bytes, and the old name absent.
  - Eviction and repack retiring one segment lower the footprint once and never below zero.
  - A read racing a retirement returns its pattern or finds the segment gone.
  - The retirement reports not-durable when its directory sync fails.
  - A failed unlink leaves the segment in the set, accounted, and the worker retries it.
- **Tests:** the seam's call log for a recycle, and the footprint around it (R72, R78); the
  four failure injections; the pool target moved across the retirement (catches a decision
  made at seal); a reader holding a reference while a retirement runs, which must wait and
  then find the segment gone (R76; it fails if `Close(id)` stops waiting); two goroutines
  retiring one segment (R74); an ended segment retired with and without `Ended`; a failed
  spare's leftover retired (R83); `freed_bytes` after an unlink and unchanged after a recycle
  (R79); a crash at each durable state with the directory listing; a write log showing a
  recycle writes only the two slots (R87); an unlink that fails and is retried; an active
  segment and a pooled spare refused (R82).

### T5.10 Ending a segment after a failed append

**Subtask T5.10 estimate: 2.5 man-days.**

- **Track:** A. **Depends on:** T5.6.
- **Covers:** R1, R10, R43, R48–R50, R72, R77.
- **Deliverables:** `internal/segment/end.go`: `End` marks the active segment ended after a
  failed append (a short write, fewer bytes than asked with no error, is one), or at the
  request of the durability code after a failed sync: it accepts no further append and no
  seal, the stream's next roll takes a spare whose header names no predecessor, the
  synced-through value stays below the failed slot, and the records written since the last
  successful sync are handed to a registered consumer, which epic 6 fills in. The ledger keeps
  the segment at its cap, and T5.9 treats it as never recyclable. The ended state itself is
  T5.1's, and T5.7 already sets it on a failed seal sync: this subtask adds the
  append-failure trigger, the predecessor-free next segment and the hand-off.
- **Acceptance criteria:**
  - After a failed append the segment takes no further append and no seal marker.
  - A short write, with no error, ends the segment the same way.
  - The next append goes to a new segment whose predecessor is zero.
  - Every record reserved after the failed slot carries a synced-through value below it.
  - The unsynced records of the window reach the consumer exactly once, each with its
    sequence number, version and location.
  - An ended segment that was itself recycled from the pool behaves the same.
- **Tests:** three concurrent appends, the middle one failed at the seam and a third completed
  (the RFC 28 zero-slot row, write half, R77); the fourth append's segment and predecessor; a
  recycled segment's failed append; the window records handed over once; the footprint of the
  ended segment at its cap until its unlink.

### T5.11 Refill from retirements, and closing the set

**Subtask T5.11 estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** T5.5, T5.6, T5.8, T5.9.
- **Covers:** R18, R21, R70, R81.
- **Deliverables:** `internal/segment/close.go`: the pool's refill worker consults the
  retirement queue before it makes a spare, so the pool grows only when no full-size retired
  segment is to hand (R18). A roll's `ENOSPC` refusal asks the same question (R21).
  `Set.Close` stops accepting appends and reads (the closed error), lets in-flight ones
  finish, calls the `Stop` of the idle, refill and retirement workers, and only then closes
  the segments and their descriptors (R70, R81).
- **Acceptance criteria:**
  - With a full-size sealed segment queued for retirement and the pool below its target, the
    worker makes no new spare, and the segment is recycled into the pool instead.
  - A roll that finds no spare after a failed `Make` with `ENOSPC` is refused `spare` while a
    retirement is queued, and with `ENOSPC` once none is.
  - After `Set.Close`, an append and a read fail with the closed error, in-flight ones
    complete or fail cleanly, and no worker goroutine is left.
- **Tests:** a full-size sealed segment queued with the pool below target, counting `Make`
  calls at the seam (catches R18); the queued and unqueued `ENOSPC` pair (R21); an append and
  a read racing `Set.Close` under `-race`; a reader holding a reference at `Close`, asserting
  nothing is freed under it (R70).

## 7. Order of work

After T5.1 and T5.2, which can run side by side, `Make` (T5.4a) comes first, and the minimal
stream (T5.4b) needs all three. From there two lanes run side by side. The **stream lane**:
T5.7, T5.6, T5.8, T5.10. The **pool lane**: T5.5, then T5.9 once the seal exists. They meet at
T5.6, which needs the pool, and at T5.11, which needs the retirement and the idle worker. The
S3 skeleton needs T5.1, T5.2, T5.4a and T5.4b, about 12.25 man-days.

```mermaid
flowchart TD
  S2(["S2 red suite in CI"]):::sync --> T1["T5.1 Segment set and ledger"]
  S2 --> T2["T5.2 Descriptor cache and drain"]
  E3a(["E3 Allocate"]):::sync --> T4a
  E3r(["E3 record codec"]):::sync --> T4b
  E3s(["E3 seal and catalog codecs"]):::sync --> T7
  T1 --> T4a["T5.4a Make"]
  T1 --> T4b["T5.4b Minimal stream"]
  T2 --> T4b
  T4a --> T4b
  T1 --> T5["T5.5 Spare pool"]
  T4a --> T5
  T4b --> C1(["after T5.4b: S3 minimal stream usable by E8"]):::sync
  T4b --> T7["T5.7 Seal"]
  T7 --> T8["T5.8 Idle seal"]
  T6 --> T8
  T4b --> T6["T5.6 Roll and streams"]
  T5 --> T6
  T7 --> T6
  T6 --> T10["T5.10 Ended segment"]
  T1 --> T9["T5.9 Retirement"]
  T2 --> T9
  T5 --> T9
  T7 --> T9
  T5 --> T11
  T6 --> T11["T5.11 Refill and close"]
  T8 --> T11
  T9 --> T11
  T6 --> C2(["after T5.6: streams roll, refusals named"]):::sync
  T11 --> C3(["after T5.11: the pool refills itself"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

Checkpoints:

- After T5.4b: the minimal stream exists, and E8 can start its integration tests against
  `Append` and `ReadAt`.
- After T5.6: a stream rolls across segments and refuses a roll by name.
- After T5.11: retired segments return to the pool and the set closes cleanly, which closes
  the capacity loop that E9 and the engine drive.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T5.1, T5.2 | 5.75 | | the ledger and the descriptor cache with its drain: logic over the storage seam that every later subtask uses, with no format dependency |
| 2 | T5.4a, T5.4b | 6.5 | PR 1, E3 `Allocate`, E3 records | making a segment and the minimal stream on it: the S3 half this epic owes |
| 3 | T5.5 | 2.25 | PR 2 | the spare pool, which can be open beside PR 4 |
| 4 | T5.7 | 4.25 | PR 2, E3 seal and catalog | the seal and its crash matrix |
| 5 | T5.6, T5.8, T5.10 | 8.5 | PR 3, PR 4 | the roll, the idle seal and the end of a segment, which share the stream's next-segment path |
| 6 | T5.9, T5.11 | 6 | PR 1, PR 3, PR 4, PR 5 | retirement, the refill from it and the close: the loop that returns space |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The pool target has no value in any RFC.** RFC 28 §2.2 says a retired segment becomes a
  spare "while the pool is below its target" and names no number. D3 takes `SpareTarget` for
  the content pool, whose default is one spare per stream. Confirm with the RFC's author, and
  put the value in the RFC.
- **T5.3, the storage guard, is withdrawn.** With D8's drain, any reader holding a guard also
  holds a reference that `Close(id)` has already waited for, so a guard would never wait. Its
  work is T5.2's (R66–R68) and its test is T5.9's (R76). The id stays unused so that comments
  citing it still resolve to this note.
- **Open RFC changes that touch this epic.** None is merged, so nothing here follows them yet
  except where D6 says so.
  - #3500: no `Sync` waits on a directory sync, a spare whose directory sync fails is not
    taken, and open syncs the directory once before a re-adopted spare is taken. D6 follows
    it. T5.5's `Adopt` (R84) would then have to wait for open's sync. If it does not merge, a
    segment whose directory sync fails after records were written needs the failed-window
    path, and T5.10's hand-off would take it.
  - #3502: a descriptor that has written is never closed until a sync covers its writes, and
    `DescriptorLimit` counts it. T5.2 would add a dirty bit, set by a write and cleared by the
    sync that covers it, and the limit would have to exceed the dirty descriptors of every
    stream, seal and spare fill at once.
  - #3512: the first `EROFS` turns the journal read-only instead of ending a segment per
    failed append, and spares are retried on an interval. That changes T5.10 and the refill
    worker.
  - #3511: an `EIO` read is retried a bounded number of times, then fails and counts the
    segment suspect. That changes T5.4b's `ReadAt`.
  - #3507: a bad segment header or seal marker is reported as damaged, and a failed append's
    segment is emptied at the next open. R43's ended segments may then be fewer at
    retirement.
  - #3509: a record header is bound to its own offset, which changes the record codec T5.4b
    calls.
  - #3525: `Stats` gains a `Spares` gauge, which T5.1 and T5.5 then maintain.
  - #3520: the OS storage and its `Allocate` stub are not on the integration branch yet, and
    T5.4a cannot finish without them. E3's record, seal marker, catalog entry and trailer
    codecs and the share-delete segment open for Windows are also still to land.
- **A headroom spare becomes an ordinary segment (D12).** RFC 1 §6 keeps the headroom outside
  every limit, but a stream that rolls onto a headroom spare may then fill it with content.
  Confirm that the refill of the class is the intended answer, or that the rest of that
  segment is kept for header-only records.
- **Lock domains of the cache, the set and the pool (D13).** RFC 1 §8.2 lists six domains
  and none names a descriptor cache or a reference drain. This epic puts the set, the ledger,
  the allocator and the pool under the capacity domain and gives the cache a leaf mutex.
  Confirm with epic 8's owner, whose lock recorder will see them.
- **A recycle zeroes two slots of a segment that remains.** RFC 1 §7.1 allows exactly two
  exceptions to "no byte range inside a segment is freed, zeroed or deallocated" (the torn
  tail and the free tail at seal), and RFC 28 §2.2 requires a recycle to overwrite the header
  slot and the trailer slot with zeros. This epic reads the recycle as retiring one segment
  and creating a spare, so the rule is not broken (R38). Confirm that reading with the RFC's
  author, and say so in RFC 1 §7.1.
- **A recycle keeps the cap accounted.** RFC 1 §6 returns a segment's cap only at its unlink,
  and RFC 28 §2.2 turns a recycled segment into a spare. R78 follows both: the footprint does
  not move across a recycle, and the spare count rises. Confirm that the RFC says so in one
  place.
- **Who triggers the automatic retirement.** This epic provides the executor and the worker
  (D5); epic 9 decides that a segment is not held and that its release records are durable.
  Confirm with epic 9's owner that it hands segments over, rather than the executor polling
  the index.
- **The retire pair of `internal/index` is not exported.** T5.1 exports `segmentGone` and
  `(*files).retireSegment`. If epic 4's owner would rather expose a different shape, T5.1
  follows.
- **Recovery's seal is epic 7's, through this epic's writer.** `Seal` takes its entries as
  data (T5.7), so epic 7 can seal a scanned segment. Confirm with epic 7's owner that it will
  call it rather than write its own.
- **The default descriptor limit on Windows.** RFC 13 gives "a quarter of the process's
  open-file limit", and Windows has no such limit to read. T5.2 uses a fixed default there
  behind a `ponytail:`; confirm the value with the platform owner.
