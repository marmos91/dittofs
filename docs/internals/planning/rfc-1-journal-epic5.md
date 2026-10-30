# Epic 5: journal v2 segments, streams and accounting

**Sources:** [RFC 28](../rfcs/rfc-28-journal-format.md) §2.2, §2.4 and §2.5 (the writing side),
§3.1 (idle seal) and Appendix A (allocation); [RFC 1](../rfcs/rfc-1-journal.md) §3.7, §3.9, §5.1,
§5.3, §6 (allocation, spares, refusals), §7.1, §7.3, §7.4 and §8.2–§8.7; the descriptor and
stream settings of [RFC 13](../rfcs/rfc-13-configuration.md) Appendix B.
**Track:** A.
**Epic 5 estimate: 10 subtasks, 30 man-days.**
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
  `DescriptorLimit`, `spare_refusals`, `segment_reopens` and the double-retire count are
  served from maintained counters.

### 1.1 In scope

- The segment set, ids and the footprint ledger, including per-share record bytes (T5.1).
- The descriptor cache and the per-process descriptor budget (T5.2).
- The per-segment storage guard (T5.3).
- Append streams: position reservation, the split at the cap, the synced-through value a
  record carries, the positioned read (T5.4), the roll and several streams (T5.6).
- The spare pool, its classes and its refusals (T5.5, T5.6).
- The seal, the catalog and the free tail, and the idle seal (T5.7, T5.8).
- Retirement by unlink or recycle, and the executor behind it (T5.9).
- Ending a segment after a failed append (T5.10).

### 1.2 Out of scope

- Byte layouts, checksums and the slot scanner: epic 3. This epic calls the encoders.
- `Sync`, group commit, the sync timer, failed-sync windows, loss records: epic 6. This epic
  provides the stream's synced-through value, the end of a segment, and the records a window
  holds; epic 6 decides what a window does.
- `Init`, `Open`, rebuild, torn tails, unlinking a stray `.spare.tmp`, re-adopting spares,
  orphans and the footer's use at recovery: epic 7. This epic exposes how large the biggest
  id in the directory is as an input it is given at open.
- The file append serialisation point, reservations and `Reserve`, the share limit, the index
  bound, the headroom half rule, the order in which refusals are named, `Stats` aggregation,
  the five lock domains and their recorder, `Handle.Close`: epic 8.
- When a segment is not held and when its removal records may be dropped, `Repack`, pins,
  `Release`: epic 9. This epic provides what they call: the retirement executor, the claim and
  the durable-unlink signal.
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
  **T5.4 is this epic's half.**
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

_In progress: written with the subtasks._

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
| R2 | A write that does not fit what remains of the current segment is split at the cap into records, one per segment, each carrying its own file offset and the write's version. No record spans two segments | MUST | RFC 28 §2.2 | T5.4, T5.6 |
| R3 | A segment is created at its cap and allocated whole, by the platform's allocation call or by writing zeros. The file never grows | MUST | RFC 28 §2.2, Appendix A | T5.5 |
| R4 | Bytes of an allocated segment that no record has reached read as zeros | invariant | RFC 28 §2.2 | T5.5 |
| R5 | Concurrent appends to one stream write at positions reserved in order. A write that fails returns its error and is never acknowledged, and its slot is left as it is | invariant | RFC 28 §2.2 | T5.4 |
| R6 | Appends of different `FileID`s proceed concurrently, on one stream and across streams. The number of independent streams is the configured stream count | MAY, adopted | RFC 28 §2.2, RFC 13 App. B | T5.4, T5.6 |
| R7 | A bounded number of segments per stream is open for append at one time. Every other segment is sealed | invariant | RFC 28 §2.2 | T5.6 |
| R8 | Every segment begins with a header, written once when a stream takes it, naming the journal identity, the segment's own id, its predecessor (or none) and the stream number. The stream's next sync makes it durable | MUST | RFC 28 §2.2 | T5.4, T5.6 |
| R9 | Segment ids start at 1 and ascend across all streams in the order segments are created | invariant | RFC 28 §2.2 | T5.5 |
| R10 | The synced-through offset a record carries is the offset in its segment up to which a sync had completed when the record was written. It never advances past a slot whose write failed or has not completed | MUST | RFC 28 §2.3, §2.2 | T5.4, T5.10 |
| R11 | A stream seals a segment while what remains still holds the seal marker, a footer entry for every record in it and the trailer | invariant | RFC 28 §2.5 | T5.4, T5.7 |
| R12 | Of the lock domains only the file append domain is held across I/O. Reserving a position in a segment takes the segment append domain for the reservation alone, and the record write happens outside it | invariant | RFC 1 §8.2 | T5.4 |

**The spare pool**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R13 | A spare is made as `<id>.spare.tmp`: created, allocated (zero-filled where Appendix A requires it), synced, renamed to `<id>.seg`, then the directory synced. A crash leaves no `<id>.seg` or a whole one | MUST | RFC 28 §2.2 | T5.5 |
| R14 | A spare's id is drawn when it is made, and its header slot is zeros until a stream takes it | invariant | RFC 28 §2.2 | T5.5 |
| R15 | Making a segment happens off the write path. A roll costs only the take | invariant | RFC 28 §2.2, RFC 1 §5.2 | T5.5, T5.6 |
| R16 | The id of a new spare or of a recycle exceeds the id of every `.seg` and `.spare.tmp` file in the directory, those found at open included | MUST | RFC 28 §2.2 | T5.5, T5.9 |
| R17 | The spares that back the headroom and the repack reserve are not in the content pool. A content roll and a `Reserve` never take one, and one that finds only those is refused `spare` | MUST NOT | RFC 1 §6 | T5.5, T5.6 |
| R18 | Creating a spare reserves its cap against the journal's maximum, atomically with every other reservation. The pool grows only at `Init`, when the maximum is raised, or when no full-size retired segment is to hand | MUST | RFC 1 §6, RFC 28 §2.2 | T5.5 |
| R19 | A directory sync follows the creation of a segment before any `Sync` covering its records returns. A spare's entry is synced when it is made, so taking one needs none, and a failed directory sync means the spare does not exist | MUST | RFC 28 §2.2, RFC 1 §5.1 | T5.5 |
| R20 | A roll that finds no spare ready is refused at once, with no I/O and without waiting, as `ErrNoSpace` naming the limit `spare`, never the journal's maximum | MUST | RFC 28 §2.2, RFC 1 §6 | T5.6 |
| R21 | Once making a spare has failed with `ENOSPC` and no full-size retired segment is to hand, a roll that finds no spare is refused with `ENOSPC`, until a spare is made again | MUST | RFC 28 §2.2 | T5.6 |
| R22 | `spare_refusals` counts each refusal naming `spare` | MUST | RFC 28 §2.2, RFC 1 §3.9 | T5.6 |
| R23 | Header-only records go on drawing on the allocated headroom when no spare is ready. The pool offers them the headroom's spares | invariant | RFC 28 §2.2, RFC 1 §6 | T5.5, T5.6 |

**Seal, catalog and free tail**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R24 | Sealing writes a seal marker at the slot just past the last record. It is durable before a successor naming the segment is taken | MUST | RFC 28 §2.2 | T5.6, T5.7 |
| R25 | Bytes after a seal marker belong to the footer, never to records | invariant | RFC 28 §2.2 | T5.7 |
| R26 | Seal order: every record durable first, then the marker, footer and trailer written and durable, then, for an early seal, the file truncated past the trailer and that made durable | MUST | RFC 28 §2.5 | T5.7 |
| R27 | A full seal costs two syncs and an early seal three. A step may be split into more | bound | RFC 28 §2.5 | T5.7 |
| R28 | A sealed segment carries a catalog footer: one entry per record in it | SHOULD, adopted | RFC 28 §2.4 | T5.7 |
| R29 | The footer is written and made durable after the records it describes | MUST | RFC 28 §2.4 | T5.7 |
| R30 | The footer carries its own checksum and identifies the extent of the segment it describes. Epic 3 owns the bytes, this epic supplies the entries and the offsets | MUST | RFC 28 §2.4, §2.5 | T5.7 |
| R31 | A segment that carries a footer ends with the fixed-size trailer in its last 32 bytes: at the cap's end for a full seal, just after the footer for an early one | MUST | RFC 28 §2.4, §2.5 | T5.7 |
| R32 | The footer is appended at most once. A footer that already verifies is never rewritten or extended | MUST, MUST NOT | RFC 28 §2.4 | T5.7 |
| R33 | A failed sync in the first two seal steps leaves the segment never sealed, and its successor names none | MUST | RFC 28 §2.5, RFC 1 §5.3 | T5.7 |
| R34 | A failed truncation sync does not end the segment. It is sealed, a successor may be taken, and it stays accounted at its cap | MUST | RFC 28 §2.5, RFC 1 §7.3 | T5.7 |
| R35 | A segment sealed full is never truncated and keeps its cap, so it can be recycled as a full-size spare. A segment sealed early always returns its free tail | MUST | RFC 28 §2.2, RFC 1 §7.3 | T5.7 |
| R36 | The catalog is not placed in a separate file, and no second durable log of it is kept | MUST NOT | RFC 28 §2.4 | T5.7 |
| R37 | An active segment that has taken no append for 30 s is sealed once the bytes it holds beyond its last catalogued point pass `journal.idle_seal_bytes`. Below the threshold it stays open | MUST | RFC 28 §3.1 | T5.8 |
| R38 | No byte range inside a segment is freed, zeroed or deallocated while the segment remains, except the free tail at seal. (The torn tail cut at recovery is epic 7's.) A recycle's slot zeroing is part of retiring the whole segment, not of a segment that remains | MUST NOT | RFC 1 §7.1 | T5.7, T5.9 |

**Retirement: unlink and recycle**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R39 | Before a segment is unlinked, every descriptor cached for it is closed. Where an open file cannot be unlinked, segments are opened with sharing that permits deletion | MUST | RFC 28 §2.2, RFC 1 §7.4 | T5.2, T5.9 |
| R40 | Recycle order: close the descriptors, rename to a fresh `<id>.seg`, sync the directory, then overwrite the header slot and the trailer slot with zeros and sync. Never zero first. No zero-fill | MUST | RFC 28 §2.2 | T5.9 |
| R41 | A recycle that fails at any step is an unlink, under whichever of its two names the journal finds. The segment is never a spare | MUST | RFC 28 §2.2 | T5.9 |
| R42 | Whether a full-size segment is recycled is decided at its retirement: recycled while the pool is below its target then, unlinked otherwise. Never decided at seal | MUST | RFC 28 §2.2, RFC 1 §7.3 | T5.9 |
| R43 | A segment sealed early is unlinked when retired, never recycled. A segment ended by a failed sync, by a failed append, or by recovery without a seal is unlinked, never recycled, and only once repack has carried its synced records forward | MUST | RFC 28 §2.2, RFC 1 §5.3 | T5.9, T5.10 |
| R44 | A segment's records are durable before it is unlinked or recycled. A release record is durable before the segment holding the record it released is retired | MUST | RFC 1 §5.1 | T5.9 |
| R45 | An unlink is made durable by a directory sync, and the retirement reports when it is. A removal record whose retention counted the segment is dropped only after (epic 9 applies the rule) | MUST | RFC 28 §2.2, RFC 1 §5.1 | T5.9 |
| R46 | A segment that is not held is retired by the journal on its own, once the records that left it not held are durable, without waiting for a repack to select it. This epic provides the executor and its background worker. Epic 9 decides when a segment is not held | MUST | RFC 1 §7.1 | T5.9 |
| R47 | Unlink and recycle take the storage guard exclusively, and freeing storage is the last step, after no reader can hold the old resolution | MUST | RFC 1 §8.5 | T5.3, T5.9 |

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
| R52 | `UsedBytes` is raised before an allocation happens and lowered only after an unlink has happened, and it moves only when a segment is created, unlinked or shortened | invariant | RFC 1 §3.7, §6 | T5.1, T5.5, T5.9 |
| R53 | Every file the journal creates is accounted, each segment's footer included, and every such file is bounded by live state, not by history | MUST | RFC 1 §7.3, RFC 28 §2.4 | T5.1, T5.7 |
| R54 | The spares of the headroom and of the repack reserve are accounted to the journal, never to a share | MUST | RFC 1 §7.3, §6 | T5.1, T5.5 |
| R55 | A record's bytes (header, payload, padding) are accounted to the share whose tag the record carries when it is written, and released when the segment holding it is unlinked or recycled | MUST | RFC 1 §6 | T5.1 |
| R56 | Retiring a segment is idempotent: its bytes leave the footprint exactly once, in the step that removes it from the set of segments the journal owns, and only if that step found it there. A retire that finds the file gone or the segment out of the set subtracts nothing | MUST | RFC 1 §7.3 | T5.1, T5.9 |
| R57 | A claim taken on a segment is never released after the segment is retired | MUST NOT | RFC 1 §7.3 | T5.1 |
| R58 | A retire that subtracts nothing is counted and logged at `Warn` | MUST | RFC 1 §7.3 | T5.1 |
| R59 | The accounted footprint is checkable against the sum of the segment set's sizes, and a mismatch is reported (the check is called by epic 7's verification) | SHOULD, adopted | RFC 1 §7.3 | T5.1 |
| R60 | The counters of this area are served from maintained state, never from a walk of the segment set | MUST | RFC 1 §3.7 | T5.1, T5.2, T5.5, T5.6 |

**Descriptors**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R61 | Open segment descriptors are bounded by `DescriptorLimit`, independently of the number of segments. The bound is per process: every journal of the process draws on one budget | MUST | RFC 1 §7.4 | T5.2 |
| R62 | A read of a segment whose descriptor is not open reopens it | MUST | RFC 1 §7.4 | T5.2 |
| R63 | The count, the bound and the reopens are reported through `Stats` | MUST | RFC 1 §7.4, §3.9 | T5.2 |
| R64 | The default `DescriptorLimit` is a quarter of the process's open-file limit | bound | RFC 13 App. B | T5.2 |

**Storage guard, lock order and shutdown**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R65 | A read either observes the location it resolved, intact, or discovers that it must retry or report the extent missing. It never returns bytes from storage that has been freed or reused | MUST | RFC 1 §8.5 | T5.3, T5.9 |
| R66 | The mechanism is a shared/exclusive guard per segment: readers hold it shared, an unlink or recycle takes it exclusively and waits for in-flight reads of that segment only | MUST | RFC 1 §8.5 | T5.3 |
| R67 | A continuous stream of readers never prevents reclamation from acquiring a segment exclusively: the guard is not reader-preferring | MUST NOT | RFC 1 §8.6 | T5.3 |
| R68 | Reclamation never holds a segment exclusively for longer than one unit of work | MUST NOT | RFC 1 §8.6 | T5.3, T5.9 |
| R69 | The storage guard is the last lock in the order and nothing is acquired while it is held. Reclamation takes it only to unlink or recycle, holding no other lock. No two locks of one domain are held at once | MUST, MUST NOT | RFC 1 §8.3 | T5.3, T5.4 |
| R70 | On close, background work (idle seal, the retirement worker) is stopped and joined before the segments it touches are closed, and no storage a reader may still hold is freed | MUST | RFC 1 §8.7 | T5.8, T5.9 |

**Test rows of the RFCs for this area**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R71 | A spare is whole or absent: crash at every step of making one, dropping unsynced allocations and directory changes. Every `<id>.seg` is at its cap and fully allocated, and the next spare's id is above any `.spare.tmp` id. (The unlink of a stray `.spare.tmp` at open is epic 7's) | test row | RFC 28 §4.2 | T5.5 |
| R72 | Retired segments are recycled: the seam sees a rename, then a zeroed header slot and trailer slot per recycle, and no zero-fill. A recycle happens exactly when the pool is below its target at retirement. Every segment sealed early is truncated and none sealed full is. A segment whose sync or append failed is unlinked | test row | RFC 28 §4.2 | T5.7, T5.9, T5.10 |
| R73 | Descriptors: create more segments than the bound, reads still succeed and open descriptors stay bounded | test row | RFC 1 §9.4 | T5.2 |
| R74 | Two paths retire one segment, one after the other: accounted footprint falls by its size exactly once and never goes below zero | test row | RFC 1 §9.2 | T5.1, T5.9 |
| R75 | Accounting follows the cap, including on a filesystem that allocates speculatively past the end of a growing file, and a record write moves the share's `UsedBytes` and not the journal's | test row | RFC 1 §9.4 | T5.1, T5.5 |
| R76 | Unlink under read: a read of a segment concurrent with its unlink returns the pattern it should, or retries, or reports the extent missing, never other bytes | test row | RFC 1 §9.4 | T5.3, T5.9 |
| R77 | A zero slot is not the end, the write half: a failed middle write returns an error, no append reserved after the failure lands in that segment, and the stream's next append goes to a new segment. (The reopen half is epic 7's) | test row | RFC 28 §4.2 | T5.10 |

## 4. Decisions this epic takes

_In progress._

## 5. Target tree

_In progress._

## 6. Subtasks

_In progress._

## 7. Order of work

_In progress._

## 8. Suggested pull requests

_In progress._

## 9. Open points

_In progress._
