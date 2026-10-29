# Epic 4: journal v2 placement index and versions

**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §2, §3.2–§3.6, §3.10, §3.11, §4, §7.2, §7.3,
§8.2, §9.2, §9.4 and §9.7; [RFC 28](../rfcs/rfc-28-journal-format.md) §3.1 for rebuild
semantics only; [the plan](rfc-1-journal-plan.md) §1.5 and §5.
**Owner:** track B, model and operations (@didimelli).
**Estimate:** 12 subtasks.
**Depends on:** sync point **S2, red suite in CI**; epic 2's reference model and profiler.
**Blocks:** epics 7, 8 and 9.

---

## Ground rules

- **Sources are the RFCs and the planning doc only.** No existing code is read, reused or
  depended on: not v1 (`pkg/block/journal`), and not what is already in
  `pkg/block/journalv2`. Everything below is derived from RFC 1 and RFC 28.
- **TDD.** Each ticket starts with failing tests for the requirements it lists (R-numbers
  below). Every requirement has at least one test, and the test must be able to fail
  (plan rule 10).
- Package: `pkg/block/journalv2/internal/index`. It is pure in-memory and does no I/O.

## Requirements and invariants

Every **MUST** and **MUST NOT** RFC 1 and RFC 28 place on the index, numbered so that tickets
and tests can cite them.

**Structure**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R1 | A file's held extents are disjoint. A superseding write splits or narrows, and never leaves two extents covering one offset | RFC 1 §4 | 2 |
| R2 | A lookup is unambiguous without consulting sequence numbers | §4 | 2 |
| R3 | Held content need not be contiguous or start at offset 0 | §2 | 2 |
| R4 | One entry per contiguous held piece of one record. Never several entries for one piece, and two records never share an entry | §4.2 | 2 |
| R5 | A held extent carries its record location, content version and offloaded bit | §2, §4 | 2 |
| R6 | The offloaded bit is never consulted to answer a read | §2 | 2 |
| R7 | Retained extents are a separate set that may overlap held extents and each other, and are never served | §4, §3.11 | 6 |
| R8 | A removal marker has an extent and a version and no location. Inside its extent, content below its version is not held and content at or above it is. A removal drops what it covers whatever its offloaded bit, and is never gated on it | §3.6 | 5 |
| R9 | A marker lives until `Settle(v)` with v ≥ its version. Settling is not persisted, so rebuilt markers start unsettled | §3.6, §3.10, RFC 28 §3.1 | 5, 11 |
| R10 | `Truncate` drops every extent at or beyond `size` and narrows the one straddling it. A zero-length `Deallocate` or `CloneTarget` is refused. A clone target's marker keeps its `CloneSpec` | §3.6 | 5 |
| R11 | The index side of `Since(from)` yields held extents and unsettled markers whose version is strictly above `from`, in version order | §3.10 | 5 |
| R12 | Each `FileID` keeps its owning tag while any segment holds a record of the file, even after its change-version entry (R35) is dropped | §3, §4 | 3, 10 |
| R47 | A release stops holding its extents, splitting or narrowing them, and leaves **no** marker. It raises no change version (R33). It is refused, with no partial progress, when any part is not offloaded: the index offers the all-offloaded predicate and the all-or-nothing removal, and E9 owns the hold and successor rules | §3.5 | 2 |
| R49 | The index side of `Since` reads a consistent view of the file and holds the file index lock no longer than a read does. The version-order sort is O(n log n) per call over the file's yielded items | §3.10 | 3, 5 |

**Queries, cost and concurrency**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R13 | Covering extent of an offset, or its absence, in `O(log n)` | §4.1 | 1, 2 |
| R14 | Held extents and **exact** gaps over a span, in offset order, in `O(log n + k)` | §4.1, §3.2 | 2 |
| R15 | Insert, split, narrow and remove in `O(log n)` amortised. Every extent of one file in `O(n)` | §4.1 | 1, 2 |
| R16 | A point lookup never walks the extents linearly | §4.1 | 2 |
| R17 | Reads of one file proceed while another file is written. The file index domain is per file, and no lock spans the whole store | §4.1, §8.2 | 3 |
| R18 | Held and retained bytes per segment in `O(1)` | §4.1 | 9 |
| R19 | Per-segment bytes and drop ranges are maintained incrementally on every append, unlink and recycle, and never derived by a walk | §4.1 | 9, 10 |

**Extent bound**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R20 | `ExtentCount` counts held entries, retained extents and removal markers | §4.2 | 8 |
| R21 | A `WriteAt` or `Fill` that would raise the count above `ExtentLimit` is refused at once, before any I/O, as the named index refusal, and counted in `index_refusals` | §4.2 | 8 |
| R22 | Records without bytes are not refused by the bound, except a release or deallocate that splits a held extent once the count is at or above limit + limit/8. Truncate, delete, a non-splitting removal and `Forget` are never refused | §4.2, §3.6 | 8 |
| R23 | An index over its bound at open succeeds. Content inserts stay refused until the count drops below the bound | §4.2 | 8, 11 |

**Versions and sequence numbers**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R24 | A version is 128 bits, epoch then counter, compared as one unsigned number. The epoch half is zero in this format, and a non-zero epoch in a record or the floor is refused | §4.3 | 4 |
| R25 | One version counter and one sequence counter per journal, never per file or per segment | §4.3 | 4 |
| R26 | No number of either kind is issued twice. Carried-forward header-only records and re-appends keep their original numbers. A repack copy of content gets a new sequence number and keeps its version. A fill gets a new sequence number and the ref's version. A release gets a new sequence number and the released version | §4.3 table | 4, 11 |
| R27 | Every assigned version exceeds every version on disk and the open floor. After recovery the next of each exceeds the maximum observed during reconstruction, not a persisted counter. An attached share raises the next version above its own floor | §4.3, RFC 28 §3.1 | 4, 11 |
| R28 | Precedence: where two records overlap, the greater (version, seq) pair wins, version compared first | §4.3 | 4, 11 |
| R29 | Nothing is issued before the first `SetVersionLimit`, and nothing at or above the limit. A `WriteAt`, `Draw` or version-drawing removal at the limit fails with `ErrNoSpace` naming the limit | §4.3 | 4 |
| R30 | `Draw` issues the next version and appends nothing | §4.3 | 4 |
| R31 | Expose the next version and the versions left below the limit | §4.3 | 4 |
| R32 | Rebuild accepts records in any segment order, concurrently, and never depends on ascending order. E4-11 either takes concurrent feeders (tested under `-race`) or merges per-segment partial results fed in parallel, recorded as a `decision:` | §4.3 | 11 |

**Change version and floor buckets**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R33 | A file's change version is the highest version of any `WriteAt`, `Truncate`, `Deallocate`, `Delete` or `CloneTarget` on it. `Release` does not raise it. It is returned as `asOf` | §3.2, §3.4 | 7 |
| R34 | A `Fill` whose `asOf` is below the change version (or below the bucket's version, for a file with no entry) writes nothing and returns a distinct error | §3.4 | 7 |
| R35 | 4,096 buckets indexed by a hash of `FileID`. An entry is dropped when the file holds no extent and no marker (whether a retained extent counts as an extent here is not stated by the RFC: E4-7 decides it and records a `decision:`), and the drop raises its bucket to the entry's version. At open every bucket starts at max(highest version on disk, floor) | §3.4, RFC 28 §3.1 | 7, 11 |
| R36 | A `Fill` inserts only absent sub-extents and sets their offloaded bit. The index offers the absence query and the insert; atomicity with `WriteAt` is E8's, under the file append lock | §3.4 | 2, 7 |

**Drop ranges**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R37 | At most eight ranges per (file, segment), each with the lowest (version, seq) of the records inside it. A ninth merges the two ranges with the smallest gap and keeps the lower pair | §7.2 | 10 |
| R38 | A range may over-cover, so a removal is kept longer, but never under-cover, so a needed removal is never dropped | §7.2 | 10 |
| R39 | The drop query compares only against records of the same file, in other segments, overlapping the removal's extent, in `O(s)` | §4.1, §7.2 | 10 |
| R40 | `DropRanges` is counted. Ranges are rebuilt at open | §7.2 | 10, 11 |

**Accounting, inspection, rebuild, CI counts**

| # | Requirement | Source | Task |
| --- | --- | --- | --- |
| R41 | Per-segment held bytes equal a recomputed walk at every step. Retained bytes count toward their segment | §9.4 D, §3.11 | 9 |
| R42 | The index can be read out per file and in bulk, without mutating it or stopping it | §4.4 | 12 |
| R43 | In E4: a shuffled-order rebuild and an incremental build of the same records give an identical index. Catalog-vs-scan equality needs segments on disk and is E7's | RFC 28 §3.1, §9.4 C | 11 (E7) |
| R44 | Comparisons per lookup and per insert at most double from 10³ to 10⁶ extents | §9.7 | 1 |
| R45 | ≤ 56 bytes per entry at 10⁶ entries. ≤ 8 drop ranges per (file, segment) after 10⁶ random 4 KiB overwrites | §9.7 | 12, 10 |
| R46 | Allocations per op on index lookup and insert are asserted, outside `-race` | plan rule 4 | 1, 2 |
| R48 | Setting the offloaded bit over a span (what `report` does, block by block) costs O(k log n), and comparisons per marked extent at most double from 10³ to 10⁶ extents ("report cost"). The `report` logic itself is E9's | §3.3, §9.7 | 2 |

## Tasks

**E4-1 — Ordered B-tree container.** A packed-node B-tree keyed on start offset, with
insert, delete, predecessor and ascend-from, and a built-in comparison counter. It is
hand-written, because the import rule allows stdlib only. *Covers* R13, R15, R44, R46.
*Tests:* a property test against a sorted-slice oracle, and fuzzing. *Deps:* none.

**E4-2 — Held-extent set for one file.** Disjoint entries, each with an extent, a location,
a version and an offloaded bit. Insert splits and narrows (the three-way split, and
covering several extents plus parts of two), and there are point and span queries with
exact gaps. Release (stop holding, no marker, all-or-nothing on the offloaded predicate)
and setting the offloaded bit over a span. *Covers* R1–R6, R13–R16, R36, R46–R48.
*Tests:* the §9.4 "bounds" row at 10⁶ extents, the §9.2 extent-shape and release edge
cases (middle, one end, every part in separate calls, partly offloaded), the report-cost
count, and regression tests that a re-append moves its entry and a carried header-only
record leaves held extents untouched. *Deps:* E4-1.

**E4-3 — File index map and lock domain.** `FileID` → file entry (tag, held set, markers,
retained set, change version, segment ranges), with a per-file lock. Two lifetimes: the
change-version entry drops when nothing is held and no marker remains (R35), and the tag
lasts while E4-10 reports a segment holding a record of the file (R12). A `decision:`
marker states that the copy-on-write tree is not used. *Covers* R12, R17, R49. *Tests:*
`-race` with readers and writers on different files and on the same file. The tag outlives
the change-version entry. *Deps:* E4-2.

**E4-4 — Precedence and counters.** The (version, seq) comparator. Version and sequence
counters, the floor and share-floor raise, the version limit, `Draw`, and the next version
and versions left. *Covers* R24–R31. *Tests:* the §9.4 "no version repeats" row at the
index level, concurrent `Draw`, and the cases just below and at the limit. *Deps:* none
(can run in parallel with E4-1).

**E4-5 — Removal markers, `Since` and `Settle`.** Truncate, deallocate, delete and clone
target markers, `Settle(v)`, and the index side of `Since`. *Covers* R8–R11, R49. *Tests:* a
removal over non-offloaded content still drops it, and the
§9.4 "no resurrection" and "removals survive to `Since`" rows without the crash, the clone
spec round trip, and the §9.2 truncate and deallocate cases. *Deps:* E4-3, E4-4.

**E4-6 — Retained-extent set.** Supersede-with-retain, drop retained, re-hold after a failed
window, and per-file and per-segment iteration. E6 and E9 decide *when* content is
retained. *Covers* R7. *Tests:* a read never returns a retained extent, and re-hold
restores the exact held state. *Deps:* E4-3.

**E4-7 — Change version and floor buckets.** The per-file change version, the 4,096
buckets, and the `Fill` staleness predicate. The RFC's `decision:` marker goes at the
bucket table, beside the `decision:` on whether retained extents keep the entry (R35).
*Covers* R33–R36. *Tests:* the §9.4 "floor buckets" and "stale Fill" rows at
the index level. *Deps:* E4-3, E4-4.

**E4-8 — Extent bound.** `ExtentCount`, `ExtentLimit`, a pure "entries this op would add"
pre-check, the ⅛ allowance, and `index_refusals`. *Covers* R20–R23. *Tests:* the §9.4
"index bound refuses" row at the index level. *Deps:* E4-2, E4-5, E4-6.

**E4-9 — Per-segment held and retained bytes.** `O(1)` counters maintained on every
mutation and cleared on unlink or recycle. *Covers* R18, R19, R41. *Tests:* counter equals
a walk after every step of a random sequence, and never goes negative. *Deps:* E4-2, E4-6.

**E4-10 — Drop ranges.** Eight ranges per (file, segment) with the lowest pair, merge on the
ninth, the `O(s)` drop query, and "any segment holds a record of this file" for the tag
lifetime. The RFC's `ponytail:` marker goes here. *Covers* R12, R19, R37–R40, R45 (ranges).
*Tests:* a property test against an exact oracle, and the 10⁶-overwrite range count.
*Deps:* E4-3, E4-4.

**E4-11 — Order-independent rebuild builder.** Records in any order, each with its own
numbers, produce the index precedence gives. Markers start unsettled, counters and buckets
are set from the maxima, segment counters and drop ranges are rebuilt, and an over-bound
result is allowed. The ticket chooses between sort-then-apply and a precedence-aware insert
with transient release shadows, and how concurrent feeding works (R32). *Covers* R9,
R23, R26–R28, R32, R35, R40, R43. *Tests:* RFC 1 §4.5 as a golden case, shuffled order
gives an identical index, an incremental build equals a rebuild, and a concurrent feed
under `-race` (or the merge, if that is the decision). *Deps:* E4-2 through E4-10.

**E4-12 — Inspection read-out, memory check, model fuzz.** A stable per-file and bulk
read-out, the bytes-per-entry check (non-race job), and `testing.F` op sequences against
epic 2's reference model. *Covers* R42, R45 (entry size). *Tests:* the memory check fails
when one field is added, and the fuzz finds a planted split bug. *Deps:* E4-11, plus
epic 2's reference model and profiler.

### Dependency order

| Task | Needs |
| --- | --- |
| E4-1, E4-4 | nothing (start in parallel) |
| E4-2 | E4-1 |
| E4-3 | E4-2 |
| E4-5, E4-7, E4-10 | E4-3, E4-4 |
| E4-6 | E4-3 |
| E4-8 | E4-2, E4-5, E4-6 |
| E4-9 | E4-2, E4-6 |
| E4-11 | E4-2 … E4-10 |
| E4-12 | E4-11, epic 2's reference model and profiler |

Checkpoints: after E4-3 and E4-4, E8's storage-independent work can start against the
index API. After E4-11, the builder goes to E7.

### Plan rules (§1.5)

| Rule | Applies | How |
| --- | --- | --- |
| 1 Unit tests | all | every R-number has a test |
| 2 Fuzz | E4-1, E4-2, E4-12 | B-tree ops, extent ops, op sequences vs the model. There is no byte parser here |
| 3 Metrics/tracing | E4-4, 8, 9, 10 | maintained counters (R31, R20, R21, R40, R18) feed `Stats` in E8. **No span**: none of the span-worthy ops belongs to the index |
| 4 Profiling | E4-1, 2, 12 | R44–R46 and R48, non-race job |
| 5 Public/internal, `-race` | E4-3, E4-4 | internal API now. Public behaviour is tested in E8, its consumer |
| 6 Known failures | n/a | conformance drives the public API, which E8 wires. E4 turns nothing green |
| 7 No v1 | all | enforced by the import test. Also the ground rule: no existing code at all |
| 8 Real storage | n/a | pure logic, so in-memory is allowed |
| 9 Comment markers | E4-2, 3, 7, 10, 11 | `decision:` (no copy-on-write tree, floor buckets, retained extents and the bucket entry, rebuild feeding) and `ponytail:` (eight drop ranges, one entry per record piece) at the code site |
| 10 Tests can fail | all | each test names the bug it catches |
| 11 Exports | all | the public `journalv2` package gains no symbols. The index API lives in `internal/index` |
| 12 Created with consumer | all | `internal/index` lands with E4-1, and each API lands with its test. Until E8, tests are the only consumer: the package is pure logic tested on its own (rule 1), not a placeholder |
