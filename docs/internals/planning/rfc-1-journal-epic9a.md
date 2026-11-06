# Epic 9a: journal v2 reclamation and tags

**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §7.1 and §7.2 (whole), §7.3 (the copy's
accounting and the double retire, as far as this epic's two retirement paths meet), §3 (`Forget`,
`Strand`, the context rules for both, and a file's tag released by a forget), §3.7 and §3.9 (the
fields and counters of this area), §4.4 (the per-segment accounting), §5.1 (the rules on
durability before an unlink and before a removal record is dropped), §6 (the repack reserve and
what a capacity refusal is owed), §8.1 and §8.3–§8.7 (the parts on repack, reclamation and
`Forget`), §9.2 and §9.4 (the rows on repack, retirement and forget), and §10 questions 5 and 6
(the two rules `Repack` owes a capacity refusal); [RFC 28](../rfcs/rfc-28-journal-format.md) §2.2
(a failed segment's retirement, an unlink durable before a removal record is dropped), §2.3 (no
copy of a record that does not verify; the record kinds a copy keeps), §3.2 (which records decide
an offloaded bit after recovery) and §3.3 (a segment holding unreadable records, and copies with
equal numbers).
**Track:** B.
**Epic 9a estimate: 11 subtasks, 37.25 man-days.**
**Depends on:** sync point **S2, red suite in CI**; epic 4's per-segment held and retained bytes
(E4-9), its drop ranges and the "any segment holds a record of this file" answer (E4-10), the
retained-extent set (E4-6), removal markers and `Settle` (E4-5) and the rebuild builder (E4-11),
used to plant states in tests; epic 5's segment set with its record bytes per share tag and its
claims (T5.1), the per-segment reader drain (T5.2), the repack-reserve class of the roll (T5.6),
the seal (T5.7) and the retirement executor, its queue and its durable-unlink report (T5.9); epic
6's `Durability` (`WaitDurable`, `Durable`, T6.1), loss records (T6.2), the announcement that a
failed segment's window is resolved (T6.3, T6.4), the retained-extent lifting (T6.4), the
read-only check `Admit` (T6.6) and the corruption handler `Corrupt` (T6.7); epic 8's share
registry (T8.3), the tag check and claim (T8.4), the admission classes for header-only records
and repack copies (T8.6), the removals and `Settle` (T8.8, T8.9), the lock-order recorder
(T8.11), `Journal.Close` and its stop registry (T8.12) and `Stats` with `UnreclaimedBytes`
(T8.13); epic 7a's `Open` with rebuild, which applies forget records and selects copies by
precedence, for every test that reopens after a crash.
**Blocks:** epic 9b (offers, `report`, marks, `Release` and holds run on this epic's liveness
rule, selection and pass, and consult its stranded set); the engine's capacity loop and share
removal, which call `Repack`, `Forget` and `Strand` (E11); epic 12.

---

## 1. Goal

Epic 9a gives the journal the half of reclamation that needs nothing from the remote tier: a
segment that holds nothing live leaves the disk on its own, and `Repack` carries the live records
of the segments it selects forward and retires them. It also gives the tag lifecycle its two
calls, `Forget` and `Strand`. With it, a journal that is never offloaded still gets its space
back: content an overwrite superseded, and extents a truncate, a deallocate or a delete removed,
return in whole segments, and a share's removal drops the share's bytes. Offers, `report`,
`MarkOffloaded`, `Unmark`, `Release` and snapshot holds are epic 9b's, and build on what this
epic leaves.

**At the end of the epic:**

- a segment that backs no held or retained extent and holds no live header-only record is
  retired, unlinked or recycled, without a `Repack` call and without a read, once the records
  that left it so are durable;
- `Repack(ctx, budget, scope)` selects within its scope, without reading, the segments with the
  least live payload, verifies every source record whole, copies only its held and retained
  pieces, coalesced, carries the live header-only records with their numbers, decides its drops
  after its own copies, makes the copies durable, repoints the index and only then retires the
  sources, and returns `freed` once the retirements are durable;
- a crash at any step of a pass leaves every held and retained extent reachable after reopen;
- a pass runs at the journal's maximum and beside a share at its limit, drawing only on the
  repack reserve, and never selects a segment another pass holds, an active one, a failed one
  whose window is open, or a damaged one that still backs extents it cannot produce;
- a pass scoped to a share seals that share's active segments itself, and the per-segment
  accounting names each segment's unreclaimed bytes;
- `Forget(ctx, tag)` appends one durable forget record and drops everything of the tag, is refused
  while a handle of the tag is open, is idempotent, and is never overtaken by a repack copy;
- `Strand(tag)` keeps a tag's extents from every other handle, appending nothing;
- `repacked_bytes`, `freed_bytes` and the `repack_pass` span move, and every conformance test
  this epic turns green has left the known-failure list.

### 1.1 In scope

- The liveness rule: when a segment is held, per header-only record kind, and the removal-drop
  test per extent by version (T9.1).
- Retirement of segments that are not held, without `Repack`, once the records that left them
  so are durable (T9.2).
- `Repack`: selection, scope and claims (T9.3), content copies (T9.4), carried header-only
  records and drops after copies (T9.5), ordering, repoint, `freed` and progress (T9.6), the
  repack reserve and the copy's accounting (T9.7), sealing a share's active segments and the
  per-segment accounting with its unreclaimed bytes (T9.8).
- `Forget` (T9.9) and `Strand` (T9.10).
- The counted rows, the known-failure bookkeeping and the model run of this area (T9.11).

### 1.2 Out of scope

- `Offload`, `OffloadMany`, the frozen `Offered` reader, `limit`, `widen`, `report` and its
  marking, partial success, unoffered reports, a removal during an offer: epic 9b.
- Pins: 64 per offer, 256 journal-wide, `PinnedSegments`, and the offer's refusal to pin a segment
  under repack. This epic gives the predicate an offer asks ("is this segment selected by a
  pass?", T9.3) and treats a pin as an exclusion selection reads (R19); epic 9b sets and clears
  pins.
- `MarkOffloaded`, `Unmark`, the below-half rule for offloaded records, `Release`, release
  records and their all-or-nothing rule, and `released_bytes`: epic 9b. The liveness rule for a
  release record is this epic's (it is the removal's, R8), so epic 9b only produces them.
- Snapshot holds: `Hold`, `Unhold`, `Stamp`, `Held`, retained lifetimes under a hold,
  `Superseded`: epic 9b. Their records are carried forward by this epic as always live (D4), and
  epic 9b narrows that to the RFC's rule.
- `Forget`'s drop of hold marks: there are none until epic 9b; epic 9b's holds hook into the drop
  this epic writes (T9.9).
- Stranded extents kept from eviction, offers and release: epic 9b's calls consult the stranded
  set this epic keeps (T9.10).
- Salvage of a stranded tag, choosing which tags to strand and to forget, and the start order:
  the engine (E11), never the journal (RFC 1 §3).
- Applying forget records, rebuilding drop ranges and the per-segment counters at open, and
  emptying a segment recovery ended: epic 7a. Inspection's index read-out (`File`, `All`) and
  `Verify` with its footprint check: epic 7b. This epic implements only `Inspector.Segments`
  (T9.8).
- The retirement executor itself (close, unlink or recycle, the directory sync, the double-retire
  count), the descriptor drain, the seal and the reserve-class roll: epic 5. This epic decides
  *when* a segment is retired and hands it over.
- Repack's write amplification (J8) and reclamation under load (J5): epic 12.
- The cluster parts of RFC 1 §3 (per-shard forget records, replicas, takeover): outside the
  journal's single-node scope.

### 1.3 Names used here

- **E1**, epic 1, interfaces: the public surface with stub bodies, among them `Repack`,
  `RepackScope`, `Forget`, `Strand` and `Inspector`.
- **E2**, epic 2, red suite and test foundations: the test clock, the fault schedule, the
  crash-simulating seam, the reference model, the profiler, the fixture and the known-failure
  list.
- **E3**, epic 3, on-disk format and platform layer: the record codecs, CRC32C and the platform
  checks.
- **E4**, epic 4, placement index and versions: the index, its retained set, markers, per-segment
  counters, drop ranges and rebuild builder. Its subtasks are cited as E4-6, E4-9, E4-10, E4-11.
- **E5**, epic 5, segments, streams and accounting: the segment set, streams, spare pool, seal,
  retirement executor and descriptor cache.
- **E6**, epic 6, durability and loss: `Sync`, loss records, failed windows, read-only state and
  the corruption handler.
- **E7a**, epic 7a, the part of epic 7 (recovery and identity) that lets a journal reopen after a
  restart or a crash: rebuild by catalog and by scan, torn tails, counters, spare re-adoption,
  the offloaded bits rebuilt from offloaded and unmark records, forget records applied at open
  and tag conflicts.
- **E7b**, epic 7b, the rest of epic 7: `Restamp`, inspection's index read-out and verify, the
  recovery benchmarks, retained extents rebuilt from hold records, and the re-append of an
  unproven tail.
- **E8**, epic 8, data path and capacity: shares and handles, `WriteAt`, `ReadAt`, `Fill`, the
  removals, capacity and admission, lock domains, `Close` and `Stats`.
- **E9a**, this epic: the part of epic 9 (offload, reclamation, holds and tags) that needs no
  remote tier: retirement, `Repack`, `Forget` and `Strand`.
- **E9b**, epic 9b, the rest of epic 9: offers, `report`, pins, `MarkOffloaded`, `Unmark`,
  `Release` and snapshot holds.
- **E10**, epic 10, start and initialisation: the device configuration, the start check and the
  journal side of initialisation.
- **E11**, epic 11, the engine on the RFC 16 store: the caller of `Repack`, `Forget` and `Strand`.
- **E12**, epic 12, E2E green: the known-failure burn-down, soak and benchmarks.
- **S1**, interfaces frozen: E1's exported surface no longer changes.
- **S2**, red suite in CI: E2's conformance suite runs on every pull request, listed whole as
  known failures.
- **S3**, the walking skeleton: E5's streams, pool, seal and roll, E6's `Sync` and corruption
  rule, and E8's `WriteAt`, `ReadAt` and minimal `Open`.
- **S4**, core complete: E3 to E9 done; only system tests remain on the known-failure list.

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
| 1 Unit tests | all | every R-number has a test that can fail. Subtasks build on E4's index and builder, E5's set and executor, E6's `Durability` and E8's journal core as landed, and plant states (a settled marker, a retained extent, a record of a tag below a forget) through E4-11's builder rather than a fake |
| 2 Fuzz | T9.1, T9.6 | no parser of foreign bytes here: a source record is read through E3's codec and verified by its checksum, which E3 fuzzes. T9.1 fuzz-drives sequences of writes, overwrites, removals, settles and retirements against an oracle of liveness; T9.6 fuzz-drives operation sequences with repack passes against the reference model |
| 3 Metrics and tracing | T9.2, T9.6, T9.8, T9.9 | `repacked_bytes` (T9.6) and the per-segment unreclaimed bytes (T9.8) are maintained at the event; `freed_bytes` is E5's, raised by its executor, and `Repack`'s `freed` is summed from the same reports (T9.6); `UnreclaimedBytes` is E8's counter, which T9.2, T9.6 and T9.9 feed through E8's account function; `DropRanges` is E4's. One span, `repack_pass`, per pass (T9.6). No span for `Forget` or for a retirement, which rule 3 does not name |
| 4 Profiling | all | every test runs under E2's profiler. T9.2's worker and T9.6's pass are where goroutines could leak; T9.1 asserts zero allocations per liveness update outside `-race`, since it runs on every index change. No hot path of rule 4 is this epic's |
| 5 Interface and internals | all | tests go through `Repack`, `Forget`, `Strand`, `Inspector.Segments` and `Stats`, and through `ReadAt` after a pass or a reopen: the read is the consumer of a repoint, a drop and a forget. `-race` on every concurrent case. Reverting a fix fails on its assertion |
| 6 Known failures | T9.11 | the rows of §6's T9.11 leave `test/journal/known.yaml` in the change that turns them green; the rows that need epic 9b's calls are re-pointed to it there |
| 7 v2 ignores v1 | none | nothing here reads anything outside v2. The rule's path names v1's former package, which v2 now uses (see §9) |
| 8 Real storage | T9.2, T9.6, T9.9, T9.11 | crash cases (mid-pass, an unlink not yet durable, a forget not yet synced) run on real storage through E2's crash-simulating seam, never an in-memory filesystem. T9.1, T9.3 and T9.5's drop test are pure logic and may run in memory |
| 9 Markers | T9.1, T9.3, T9.4, T9.6, T9.9, T9.10 | the RFC's `ponytail:` on whole-segment freeing goes at the retirement hand-over (T9.2); §4 lists every marker this epic leaves |
| 10 A test can fail | all | each test names the bug it catches in §6 |
| 11 Exports | T9.8 | nothing new is exported: `Repack`, `RepackScope`, `Forget`, `Strand` and `Inspector.Segments` are E1's. The per-segment unreclaimed figure stays internal until RFC 1 §4.4 names it (D16) |
| 12 Creates only what it declares | all | `internal/reclaim` lands with T9.1, its first consumer being T9.2 in the same pull request; each later file lands with its subtask |

## 3. Requirements and invariants

**When a segment is held, and when it is retired**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A segment is held while it backs a held or retained extent or holds a live header-only record. A segment that is not held is retired, unlinked or recycled, by the journal on its own, without waiting for a repack to select it | MUST | RFC 1 §7.1, §7.2 | T9.1, T9.2 |
| R2 | Storage is freed in whole segments only: neither retirement nor repack frees, zeroes or deallocates any byte range inside a segment while it remains | MUST NOT | RFC 1 §7.1 | T9.2, T9.4 |
| R3 | A segment is handed to retirement only once the records that left it not held are durable, and a segment's own records are durable before reclamation unlinks or recycles it | MUST | RFC 1 §7.1, §5.1 | T9.2, T9.6 |
| R4 | A release record is durable before the segment holding the record it released is unlinked or recycled. (Epic 9b writes release records; the rule is applied by this epic's hand-over, R3) | MUST | RFC 1 §5.1 | T9.2 |
| R5 | A segment left by a failed sync, or ended by a failed append, is a repack candidate once its window, if any, is resolved, never before; it is unlinked, never recycled, only after repack carried forward the records synced into it before the failure. Whether it is resolved is re-read under the file index before its records are carried or it is handed over | MUST | RFC 1 §7.2, §5.3; RFC 28 §2.2 | T9.3, T9.6 |
| R6 | A segment in which every record has stopped holding anything becomes reclaimable without a read | test row | RFC 1 §9.2 | T9.1, T9.2 |
| R7 | One segment selected by two reclamation paths at once is retired by one of them; the accounted footprint falls by its size exactly once and never below zero | test row | RFC 1 §9.2, §7.3 | T9.2, T9.3 |

**Which header-only records are live**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R8 | A release record, or a synced loss record, is live while it may still outrank a record of its file in another segment; a synced loss record also while `Settle` has not covered it | invariant | RFC 1 §7.2 | T9.1 |
| R9 | An unsynced loss record is live while any segment holds a copy of the record it names | invariant | RFC 1 §7.2, §3.8 | T9.1 |
| R10 | A truncate, deallocate, delete or clone target record is live while it may still outrank a record in another segment or its removal marker is unsettled | invariant | RFC 1 §7.2, §3.6 | T9.1 |
| R11 | A forget record is live while any other segment holds a record of its tag below its sequence number | invariant | RFC 1 §7.2, §3 | T9.1, T9.9 |
| R12 | An offloaded or unmark record is live while it decides the bit of a held or retained extent; a hold, unhold or stamp record while the hold or stamp it records is. This epic carries every one of them forward as live (D4); epic 9b, which writes them, applies the narrower rule | invariant | RFC 1 §7.2; RFC 28 §3.2 | T9.1 |
| R13 | A removal, release or synced loss record whose marker, if it has one, is settled, and below whose (content version, sequence number) pair no other segment holds a record of the same file overlapping its extent, can outrank nothing and is dropped. The test is per extent and by that pair: never by sequence number alone, never against every record of the file whatever its extent, never against the lowest pair journal-wide | MUST | RFC 1 §7.2, §4.3 | T9.1, T9.5 |
| R14 | Such a record is dropped only once every unlink or recycle it counted on is durable by a directory sync | MUST NOT | RFC 1 §7.2, §5.1; RFC 28 §2.2 | T9.5, T9.6 |
| R15 | The test reads the at most eight ranges per (file, segment) the index keeps, each with the lowest pair inside it: it may keep a removal longer than it must, never drop one it needs | bound | RFC 1 §7.2 | T9.1 |

**Selection**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R16 | Candidacy is decided from held and retained bytes per segment against the storage the segment still allocates; a pass never reads a segment to decide whether to repack it | MUST NOT | RFC 1 §7.2 | T9.3 |
| R17 | A pass takes the segments with the least live payload first | SHOULD | RFC 1 §7.2, §7.1 | T9.3 |
| R18 | Only segments inside the scope are candidates: a share's scope selects the segments holding that share's records, a segment-set scope only the named segments, and a zero scope any segment | MUST | RFC 1 §7.2 | T9.3 |
| R19 | A segment pinned by a running offer is not a candidate, and a segment a pass has selected takes no new pin until the pass ends with it. (Pins are epic 9b's; this epic reads the pin and answers the "selected" predicate) | MUST | RFC 1 §7.2 | T9.3 |
| R20 | A pass never runs on a segment while it backs a held or retained extent from a record that cannot be read. Once those extents are dropped, or the failed window resolved, it carries the readable live records forward and retires the segment | MUST NOT | RFC 1 §7.2; RFC 28 §3.3 | T9.3, T9.6 |
| R21 | Two passes never select the same segment, and a segment is never its own target | MUST NOT | RFC 1 §7.2, §8 | T9.3 |
| R22 | A segment recovery ended is never a candidate: open empties and unlinks it (epic 7a) | invariant | RFC 1 §7.2; RFC 28 §3.3 | T9.3 |

**Copies of content**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R23 | Repack preserves the set of held extents and of retained extents, the bytes they produce, and their offloaded bits: it neither resets nor sets a bit | MUST | RFC 1 §7.2; RFC 28 §3.2 | T9.4, T9.6 |
| R24 | A copy of content gets a new sequence number and keeps its content version | invariant | RFC 1 §7.2, §4.3 | T9.4 |
| R26 | Before copying any part of a source record, repack reads its whole payload and verifies its payload checksum. A record that does not verify is dropped as corrupt and nothing of it is copied | MUST | RFC 1 §7.2; RFC 28 §2.3, §3.3 | T9.4 |
| R27 | Repack copies only the held and retained extents of a record, each as a new record with its own payload checksum, never the released part; the original stays whole on disk until its segment is unlinked | invariant | RFC 1 §7.2, §7.1 | T9.4 |
| R28 | Adjacent held extents of one file that share a content version and an offloaded bit are copied as one record | invariant | RFC 1 §7.2, §4.2 | T9.4 |
| R29 | A copy is appended under its file's serialised append, and only if its source is still held or retained at that moment. A source a later write or removal turned retained is still copied, as retained; one neither held nor retained is skipped. The check and the append are one step against `WriteAt`, the removals and `Release` on that file | MUST | RFC 1 §7.2, §8.4 | T9.4 |

**Carried header-only records**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R25 | A carried header-only record keeps both its sequence number and its content version | invariant | RFC 1 §7.2; RFC 28 §3.3 | T9.5 |
| R30 | A pass decides its drops once its own copies are appended, and counts them, its target segments included | invariant | RFC 1 §7.2 | T9.5 |

**Ordering, `freed` and readers**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R31 | Copy the records, make the copies durable, repoint the placement index, and only then retire the source. A crash before the retirement leaves both, and precedence selects the copy; a crash after it is a completed repack. At no point is an extent unreachable | MUST | RFC 1 §7.2 | T9.6 |
| R32 | Repack's repoint is atomic for readers of the affected files: a read sees the old location or the new one, never neither | MUST | RFC 1 §8.4 | T9.6 |
| R33 | `Repack` returns `freed`, the storage its retirements returned to the filesystem or to the spare pool, only once they are durable; the storage in use, `UsedBytes` − `Spares` × the segment cap, falls at that moment, never earlier | MUST | RFC 1 §7.2, §6 | T9.6 |
| R34 | `repacked_bytes` counts the bytes repack moved, and `freed_bytes` the storage retirements returned, both served from counters | MUST | RFC 1 §3.9 | T9.6 |
| R35 | A read either observes the location it resolved, intact, or learns to retry or to report the extent missing; it never returns bytes from storage freed or reused. Repack frees storage only through epic 5's executor, after its repoint | MUST | RFC 1 §8.5 | T9.6 |

**Capacity**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R36 | Copies reserve against the repack reserve, never a share's limit, and a pass copies at most what the reserve holds before the segments it emptied are retired and the reserve is whole again | MUST | RFC 1 §7.2, §6 | T9.7 |
| R37 | A journal at its maximum, and a share at its limit, can still run a repack | MUST | RFC 1 §7.2 | T9.7 |
| R38 | A copy is accounted to its record's share once the source segment is unlinked; until then the share's usage may stand above its limit by that copy | invariant | RFC 1 §6 | T9.7 |
| R39 | A pass scoped to a share seals each active segment that holds the scope's unreclaimed bytes, at most one per stream until repack retires it, so repack can reach those bytes; the seal marker draws on the reserved headroom, so a journal at its limit still takes it | MUST | RFC 1 §10 question 5 | T9.8 |
| R40 | The per-segment accounting gives, for each segment, its id, seal state, allocated storage, held bytes, whether it is damaged, and its unreclaimed bytes; `Stats` is the total | MUST | RFC 1 §4.4, §10 question 6 | T9.8 |

**Locks and progress**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R41 | A pass takes each file's append lock in turn and never holds two; it repoints under the file index alone, and takes the storage guard only to unlink, holding no other lock | MUST | RFC 1 §8.3 | T9.4, T9.6 |
| R42 | No I/O is done while the file index is held: a source is read with the index released | MUST NOT | RFC 1 §8.3 | T9.4 |
| R43 | A continuous stream of readers never keeps reclamation from a segment; reclamation holds a segment exclusively for no longer than one unit of work, so a large repack is divisible | MUST | RFC 1 §8.6 | T9.6 |
| R44 | A read of one file never waits on reclamation of a segment it does not read | invariant | RFC 1 §8.1 | T9.6 |
| R45 | On close, passes and this epic's retirement worker are stopped and joined before the segments they touch are closed | MUST | RFC 1 §8.7 | T9.2, T9.6 |
| R46 | `Repack` takes a context and checks it before any refusal and before changing state, returning its error having changed nothing; a unit whose copy has been appended completes | MUST | RFC 1 §3 | T9.6 |

**Tags**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R47 | `Forget` appends one header-only forget record naming the tag, durable before it returns and drawing on the reserved headroom, and drops from the index every held and retained extent, removal marker and hold mark of the tag's files | MUST | RFC 1 §3, §6 | T9.9 |
| R48 | After `Forget`, `Since` yields nothing of the tag's files and nothing is left to settle: `RemovalMarkers` no longer counts them | invariant | RFC 1 §3, §9.4 | T9.9 |
| R49 | `Forget` is refused while a handle of the tag is open | MUST | RFC 1 §3 | T9.9 |
| R50 | `Forget` is durable and idempotent: run again after a crash, or on a tag that holds nothing, it succeeds | invariant | RFC 1 §3 | T9.9 |
| R51 | A forget record and its drop are atomic against repack copies and fills of the tag: a copy whose held check ran before the forget is abandoned, never appended after the forget record | MUST | RFC 1 §8.4, §3 | T9.4, T9.9 |
| R52 | Once a tag is forgotten, the files it claimed are claimed by no tag, so a write through another handle claims them; a tag conflict naming it is cleared from `TagConflicts`; and `Stats().Shares` no longer lists it | MUST | RFC 1 §3, §3.7, §9.4 | T9.9 |
| R53 | The journal never forgets a tag on its own: a tag not attached is kept, served once its share attaches, and forgotten only when the caller names it | invariant | RFC 1 §3, §9.4 | T9.9 |
| R54 | `Forget` takes a context; with a context already done it returns the context's error and appends no record | MUST | RFC 1 §3, §9.4 | T9.9 |
| R55 | `Strand` is kept in memory and appends nothing. From the call on, no read through another tag's handle is served from the tag's extents, and none of them is evicted, offered or released (epic 9b's calls consult the set); a handle of the tag itself still reads them. It takes no context | MUST | RFC 1 §3 | T9.10 |

**Conformance rows this epic turns green**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R56 | Removal outlives its segment: a delete's segment, alone selected, and a settled delete whose counted unlinks are not yet durable, both leave the file absent after a crash | test row | RFC 1 §9.4 A | T9.5, T9.6, T9.11 |
| R57 | Crash mid-repack: between copy and unlink, recovery selects the copies, every held extent reads, and no extent is duplicated | test row | RFC 1 §9.4 A | T9.6, T9.11 |
| R58 | Copy loses to a newer write: synced, no copy is appended; unsynced, the copy is appended as retained and a failed window serves the old bytes, before and after reopen | test row | RFC 1 §9.4 A | T9.4, T9.11 |
| R59 | Repack under read: a read during a repack copy and unlink sees the old or the new location, never neither | test row | RFC 1 §9.4 A | T9.6, T9.11 |
| R60 | A recycled segment keeps its new records, and a failed append in a recycled segment makes it a candidate only once resolved, unlinked after its synced records are carried | test row | RFC 1 §9.4 A | T9.3, T9.6, T9.11 |
| R61 | Background joined: a store closed with a repack in flight stops it before any segment is closed | test row | RFC 1 §9.4 B | T9.6, T9.11 |
| R62 | Forget leaves nothing to settle, and a later attach of the tag holds new writes | test row | RFC 1 §9.4 C | T9.9, T9.11 |
| R63 | An unclaimed tag is reported in `Stats().Shares` with its held and dirty bytes, and no longer after `Forget` | test row | RFC 1 §9.4 C | T9.9, T9.11 |
| R64 | Segment reclaimable: once a corrupt record's extents are dropped, repack can select the segment and its storage is recovered | test row | RFC 28 §4.2 B | T9.3, T9.11 |

## 4. Decisions this epic takes

- **D1, one package for reclamation; the tag calls beside the tag check.** `internal/reclaim`
  holds the liveness tracker, the retirement candidacy and `Repack`. `Forget` and `Strand` live
  in `internal/datapath` beside epic 8's `tags.go` and share registry, since they change both.
  `Journal.Repack` delegates to `reclaim`, and `Forget` calls `reclaim` only to register its
  record's liveness (T9.1).
- **D2, liveness is a counter per segment, kept by events.** Each segment carries a count of the
  live header-only records it holds, beside E4-9's held and retained bytes; a segment is not held
  when all three are zero. The count is changed where a record's liveness can change, never by a
  sweep: a record becomes live when appended; a removal or synced loss record is re-tested for
  its file when that file's marker is settled, when a segment that held ranges of the file is
  durably retired (E5's executor reports the files through E4's `segmentGone`), and after a
  pass's copies (R30); an unsynced loss record when the last segment holding the record it names
  is retired; a forget record per D3. A `ponytail:` marker at the tracker names the ceiling: a
  file with many removal records re-tests them all at each such event, O(records of the file);
  upgrade to an index of removal records by extent when a profile shows the re-test.
- **D3, a forget record's liveness is a snapshot of segments.** At `Forget`, the segments whose
  record bytes for the tag are not zero (E5's per-tag bytes per segment) are the only ones that
  can hold a record of the tag below the forget's sequence number: every later record of the tag
  is above it. The record stays live while any of them remains in the set. A `decision:` marker
  says why no per-record scan is needed.
- **D4, records this epic does not write are carried as live.** Offloaded, unmark, hold, unhold
  and stamp records are written only by epic 9b's calls; until then none exists outside a planted
  test. Carrying every one forward keeps a pass correct with no rule for them, at no cost while
  none is written. A `ponytail:` marker at the liveness switch names the upgrade: apply RFC 1
  §7.2's rule for each kind once the calls that write them land, since carrying them all would
  drain the headroom under offload.
- **D5, the hand-over waits for a durability point.** When a change leaves a segment not held,
  the tracker records, per stream, the location of the newest record whose publish did it (the
  removal, the superseding write whose sync lifted a retained extent, the loss record, the
  forget record). The candidate is handed to E5's retirement queue once E6's `Durable` reports
  every such location covered. An ended segment additionally needs E6's announcement that its
  window is resolved, and the announcement is re-read under the file index before the hand-over
  (E6's D10: a read can mark it unresolved again when it starts serving a copy). The RFC's
  `ponytail:` on freeing only whole segments goes here.
- **D6, copies land on the file's own stream.** A copy is appended like any record of its file,
  on the stream E5 chose for it (E5's D7), under the file append domain, and rolls onto a
  repack-reserve spare when the active segment has no room (E5's T5.6). Only sealed or ended
  segments are candidates, so the segment a copy lands in is never one the pass selected (R21);
  the one exception, T9.8's share seal, seals the active segment before selecting it.
- **D7, a copy keeps its source's kind and modification time.** A piece of a `write` record is
  copied as a `write` carrying the source's modification time; a piece of a `fill` as a `fill`,
  so recovery sets its offloaded bit as for any fill (RFC 28 §3.2). Coalescing (R28) merges
  pieces of one kind only, and for writes only with one modification time; a `decision:` marker
  says why a coalesced copy never mixes the two kinds (a merged fill would lose its bit at the
  next crash).
- **D8, copies are repointed only once durable, and a copy whose sync fails is abandoned.** The
  pass waits for its copies through E6's `WaitDurable`; a copy is published nowhere before then,
  so a failed window that drops it removes nothing a reader sees. The pass then abandons every
  source unit with a copy in that window, retires none of them, and returns the error. Whether
  E6's failed-window resolution should name a never-published copy as a loss is an open point
  (§9).
- **D9, the unit of work is one source segment.** A pass selects, copies, waits, repoints and
  hands over one segment at a time, checking its context between units (R46); the storage guard
  is taken exclusively only by E5's executor for one unlink or recycle (R43). The pass registers
  a stop function with E8's `Close` (T8.12), which interrupts it between units and joins it.
- **D10, `Forget` closes the tag, passes a barrier, then appends.** `Forget` first marks the tag
  forgetting in E8's share registry, under the registry's mutex, refusing if a handle is open;
  while marked, `Share` of that tag is refused with `ErrHandleOpen`, wrapped to say a forget is in
  progress (a `decision:` marker: one error says the tag is busy, and no new exported error
  is needed). It then takes each of the tag's files' append domain in turn and releases it at
  once: any repack copy of the file that passed its check before the mark has appended by then,
  and every later copy reads the mark under the same domain and is abandoned (R51). Only then is
  the forget record appended, and its drop is made once it is durable, per file under the file
  append domain and the file index held for the write.
- **D11, the drop waits for the record to be durable.** Because nothing is dropped before the
  forget record is durable, a read-only journal or a failed window that fails the record has
  nothing to undo: `Forget` returns the error and the tag stays whole. This answers epic 6's open
  point on the lock a forget record publishes under: none, before it is durable, and each file's
  append domain after.
- **D12, a tag that holds nothing appends nothing.** `Forget` of a tag no segment holds a record
  of (by E5's per-tag bytes), with no entry in the registry, returns nil and appends no record:
  recovery has nothing to apply, and the start that calls `Forget` again for a tag already
  forgotten does no I/O.
- **D13, repack calls the corruption handler holding the file's append domain.** A source that
  does not verify is passed to E6's `Corrupt` with the segment reference released first and
  `Corruption.DomainHeld` set (E6's D9). This answers epic 6's open point.
- **D14, a carried copy is not registered as another copy for a read.** E6's `Corrupt` serves a
  verified copy only of a re-append. A copy a pass made is repointed to before its source is
  retired, so a read that finds the source bad never needs the copy. E6's `ponytail:` stays as it
  is; this answers its open point.
- **D15, `Strand` is a set in the registry.** `Strand(tag)` adds the tag to an immutable set the
  registry publishes like its handles (E8's D13), returns nil, and `ErrClosed` after close. The
  set is read by epic 9b's `Release`, `Offload` and `OffloadMany` and by `ReadAt`'s tag check;
  `Forget` removes the tag from it.
- **D16, the per-segment unreclaimed figure is kept now and exported when the RFC names it.**
  Selection and the share seal need it internally from T9.3. RFC 1 §4.4 does not yet list it
  (§10 question 6 owes it), so `SegmentInfo` gains the field only when the RFC does (rule 11);
  until then the engine reads the scope's figure through `Stats().Shares`. Open point in §9.
- **D17, this epic implements `Inspector.Segments`, and only it.** Selection by segment-set scope
  and the engine's capacity step need the per-segment rows while the journal runs; `File`, `All`
  and `Verify` stay epic 7b's.

## 5. Target tree

```text
pkg/block/journal/
  repack.go             T9.6   Repack delegates to internal/reclaim
  share.go              T9.9, T9.10   Forget and Strand delegate to internal/datapath
  inspect.go            T9.8   Inspector.Segments
  internal/
    reclaim/
      live.go           T9.1   the liveness tracker per segment (D2, D3, D4)
      droptest.go       T9.1   the removal-drop test over E4's ranges
      retire.go         T9.2   candidacy, the durability point, the hand-over to E5 (D5)
      select.go         T9.3   candidates, scope, order, the claim, the selected predicate
      copy.go           T9.4   verify whole, copy held and retained pieces, coalesce (D6, D7)
      carry.go          T9.5   carried header-only records, drops after copies
      pass.go           T9.6   the pass: units, durability, repoint, freed, stop (D8, D9)
      reserve.go        T9.7   the reserve bound and the copy's accounting transfer
      seal.go           T9.8   the share scope's seal
      *_test.go
    datapath/
      forget.go         T9.9   Forget (D10, D11, D12)
      strand.go         T9.10  Strand (D15)
      shares.go         T9.9   the registry's forgetting mark
    segment/
      inspect.go        T9.8   per-segment rows from the set and E4-9's counters
test/journal/known.yaml T9.11  rows turned green removed; rows needing epic 9b re-pointed
```

## 6. Subtasks

### T9.1 Liveness of segments and the removal-drop test

**Subtask T9.1 estimate: 4 man-days.**

- **Track:** B. **Depends on:** S3; E4-5, E4-9, E4-10; E5's segment set with per-tag bytes
  (T5.1); E6's loss records (T6.2).
- **Covers:** R1, R6, R8–R13, R15.
- **Deliverables:** `internal/reclaim/live.go`: per segment, a count of live header-only records
  beside E4-9's held and retained bytes, and `Held(seg) bool`. `Append` and publish hooks that
  register each header-only record a path appends (E8's removals, E6's loss records, T9.9's
  forget record), and the events of D2 that re-test them. `internal/reclaim/droptest.go`:
  `Droppable(rec) bool` for a removal, release or synced loss record, built on E4-10's ranges of
  the record's file in every other segment, comparing the (version, sequence) pair over the
  record's extent, and on the marker's settled state from E4-5; an unsynced loss record's test
  against the segments holding the record it names; the forget record's snapshot (D3); the
  always-live switch for offloaded, unmark, hold, unhold and stamp records (D4). A segment going
  from held to not held calls T9.2's candidacy.
- **Acceptance criteria:**
  - A settled truncate whose file has no record at a lower pair overlapping it in another
    segment is not live; one with such a record is; an unsettled one is live whatever the ranges.
  - A synced loss record is live until `Settle` covers it and nothing it can outrank remains; an
    unsynced one until the last segment holding the record it names is retired.
  - A removal over `[1 GiB, 2 GiB)` is not kept alive by an older record of the same file over
    `[0, 1 MiB)`, nor by an older record of another file.
  - A record compared by sequence number alone would be kept or dropped differently in the
    planted case of RFC 1 §7.2 (a retained copy above a truncate's sequence and below its
    version): the test keeps the truncate.
  - A segment whose held and retained bytes reach zero and which holds no live record reports
    not held without any segment read.
- **Tests:** a property test against an exact oracle (every record kept, compared pair by pair)
  over random writes, overwrites, removals, settles and retirements, asserting `Held` agrees
  after every step (catches a missing re-test event); a `testing.F` target over the same
  sequences; the three planted cases above built through E4-11 (catches a test by sequence, per
  file, or journal-wide); a segment of truncates only, all settled, reporting not held with the
  seam's call log free of reads (R6); zero allocations per liveness update outside `-race`.
- **Rules:** a `ponytail:` at the re-test (D2), a `ponytail:` at the always-live switch (D4) and a
  `decision:` at the forget snapshot (D3).

### T9.2 Retiring segments that are not held

**Subtask T9.2 estimate: 3.5 man-days.**

- **Track:** B. **Depends on:** T9.1; E5's executor and queue (T5.9); E6's `Durable` (T6.1),
  the resolved announcement (T6.3, T6.4) and `Admit` (T6.6); E8's account function and
  `UnreclaimedBytes` (T8.13), and its stop registry (T8.12).
- **Covers:** R1–R4, R6, R7, R45.
- **Deliverables:** `internal/reclaim/retire.go`: the candidacy D5 describes, a worker that hands
  a candidate to E5's queue with the reason `Sealed` or `Ended` once every durability point is
  covered and, for an ended segment, once its resolved state, re-read under the file index, still
  holds; a claim taken through E5's set when a candidate is queued, so a pass cannot select it
  (R7); E8's `UnreclaimedBytes` lowered when E5 reports the retirement durable. The worker stops
  through E8's stop registry. The RFC's `ponytail:` on whole-segment freeing goes at the
  hand-over.
- **Acceptance criteria:**
  - A sealed segment whose last held extent an overwrite superseded is retired, without a
    `Repack` call, only after the overwrite is synced: with the sync held at the seam, the
    executor is not called.
  - A segment emptied by a truncate is retired only once the truncate record is durable.
  - An ended segment is not handed over while its window is open, nor after a read has marked it
    unresolved again.
  - The storage in use falls by the segment's size once the unlink is durable, and not before.
  - A segment queued here and selected by a pass is retired once, and the footprint falls once.
  - Close with candidates queued joins the worker before any segment closes.
- **Tests:** the sync held at the seam with an overwrite that empties a segment, asserting the
  executor's call log (catches a hand-over before durability, R3); the same with a truncate
  (R4's rule as it applies to removals, since release records need epic 9b); an ended segment
  re-marked unresolved by a planted `UseCopy` between candidacy and hand-over (catches a check
  read once); a pass racing the worker on one segment under `-race`, with the footprint checked
  (R7); `TestEdge_FullyReleasedSegmentReclaimedWithoutRead`'s shape with truncates in place of
  releases, at the journal level (R6); a close during a queued hand-over (R45).

### T9.3 Selection, scope and claims

**Subtask T9.3 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T9.1; E5's claims (T5.1); E6's damage report (T6.7) and resolved
  announcement (T6.4).
- **Covers:** R5, R7, R16–R22, R60, R64.
- **Deliverables:** `internal/reclaim/select.go`: candidates from the set's sealed and ended
  segments, never active ones or spares; live payload per segment from E4-9's bytes and T9.1's
  live records; per-segment unreclaimed bytes (allocated less live payload) kept with them
  (D16); scope filtering by share (E5's per-tag bytes), by a segment set, or none; order by least
  live payload, ties by oldest; exclusions: a pin (epic 9b's, read as a flag that is never set
  until then), a claim by another pass or by T9.2, an ended segment whose window is open, a
  damaged segment that still backs held or retained extents from unreadable records, a segment
  recovery ended; a claim through E5's set taken when selected and released only by the
  retirement or the pass's end; `Selected(seg) bool` for epic 9b's offers.
- **Acceptance criteria:**
  - Selection performs no read: the seam's call log is unchanged across it.
  - A share's scope selects only segments holding that share's records, a segment-set scope only
    those named, even when sparser segments exist elsewhere.
  - Two passes started together never hold the same segment.
  - A failed segment becomes a candidate exactly when its window is announced resolved.
  - A damaged segment backing an unproducible extent is skipped until E6 drops the extent, then
    selected.
- **Tests:** the seam's log across selection (R16); three segments with 10 %, 50 % and 90 % live
  payload, asserting the order (R17); a two-share layout with A's records interleaved in
  B's sparse segments, asserting the share scope's choice and a segment-set scope (R18,
  `TestRFC1B_RepackAimedAtShare`'s selection half, whose conformance form needs `Release`); two
  passes racing under `-race` (R21); a failed append's segment before and after resolution (R5,
  R60); a planted corrupt record, then its drop, then selection (R20, R64); a planted pin flag
  (R19).

### T9.4 Copying content

**Subtask T9.4 estimate: 5 man-days.**

- **Track:** B. **Depends on:** T9.3; E8's file append domain and claim (T8.1, T8.4), the
  repack-copy admission class (T8.6); E5's reserve-class roll (T5.6) and positioned read (T5.4b);
  E6's `Corrupt` (T6.7).
- **Covers:** R2, R23, R24, R26–R29, R41, R42, R51, R58.
- **Deliverables:** `internal/reclaim/copy.go`: for each record of the selected segment that
  backs a held or retained piece, read its whole payload with no lock held and verify it against
  its checksum; a record that fails is passed to `Corrupt` with `DomainHeld` once the file's
  append domain is taken (D13) and nothing of it is copied. Then, under the file's append domain
  (one file at a time): read the index for the pieces of that record still held or retained, and
  the forgetting mark of the file's tag (D10); skip what is neither, abandon the file if its tag
  is being forgotten; otherwise append each run of adjacent pieces with one version, one bit,
  one kind and, for writes, one modification time as one record (D7), with a new sequence number
  and the source's version, admitted from the repack reserve. The copies' locations are kept for
  T9.6's repoint; nothing is published yet (D8). A segment's pieces whose source failed to verify
  are reported as dropped, which lets T9.3 treat the segment as producing everything it backs.
- **Acceptance criteria:**
  - A record whose middle was superseded is copied as its two live ends, each with its own
    checksum, and its superseded middle is never written.
  - A source with one corrupt byte anywhere in its payload, even outside the pieces held, has
    nothing copied and is dropped through `Corrupt`.
  - Two adjacent held pieces of one write split across two records at one version are copied as
    one record; a fill beside a write at the same version is not merged with it.
  - A copy of a fill is a fill, and a reopen sets its bit; a copy of a write keeps the source's
    modification time, which `Since` reads.
  - An overwrite between the read and the append, synced, means no copy; unsynced, the copy is
    appended and kept for repoint as retained.
  - A copy of a file whose tag is being forgotten is never appended after the mark.
  - The lock-order recorder sees one file append lock at a time, and no read with the file index
    held.
- **Tests:** a partly superseded record copied with the seam's write log (R27, catches a copy of
  the whole record); a corrupt byte in a superseded part (R26, `TestRFC1A_RepackVerifiesWholeSource`'s
  journal-level form); coalescing across a segment-boundary split and a fill beside a write (R28,
  D7); a reopen after a pass over filled content asserting the bit (R23); the hold between read
  and append of `TestRFC1A_CopyLosesToNewerWrite`, both halves, under the seam (R29, R58); a
  forget racing a pass, with the forget record's sequence number compared against every copy's
  (R51); the recorder over a pass with concurrent writers (R41, R42).

### T9.5 Carrying header-only records and deciding drops

**Subtask T9.5 estimate: 3.5 man-days.**

- **Track:** B. **Depends on:** T9.4; T9.1's test.
- **Covers:** R13, R14, R25, R30, R56.
- **Deliverables:** `internal/reclaim/carry.go`: for each header-only record of the selected
  segment, once the pass's content copies for the segment are appended: re-test its liveness with
  the copies counted (R30); carry each live one forward with its sequence number and version
  unchanged, its bytes from memory (a header and at most a 56-byte payload), under its file's
  append domain where it has a file; drop the rest, except a removal, release or synced loss
  record whose drop counts on a retirement whose unlink is not yet durable, which is carried
  (R14). Carried records are registered with T9.1 at their new location.
- **Acceptance criteria:**
  - A settled truncate over content a pass in the same segment carries forward as retained is
    carried with it, and a reopen reads the extent missing.
  - A carried removal keeps both numbers: a crash with the original and the copy on disk reads as
    one record.
  - A settled delete whose older records sit in a segment whose unlink's directory sync is held at
    the seam is carried, not dropped; after the sync, the next pass drops it.
  - An unsettled truncate is always carried, and `Since` still yields its marker after a reopen.
- **Tests:** the planted case of a settled truncate and a retained version in one segment
  (R30, `TestRFC1A_DropsDecidedAfterCopies`' journal-level form, whose conformance form needs a
  hold); the held directory sync of `TestRFC1A_RemovalOutlivesItsSegment`, both halves (R14,
  R56); a carried removal's numbers read back through E3's codec (R25); an unsettled truncate
  carried and reopened (`TestRFC1A_RemovalRecordsCarriedForward`'s second half).

### T9.6 The pass: durability, repoint, `freed` and progress

**Subtask T9.6 estimate: 4.5 man-days.**

- **Track:** B. **Depends on:** T9.5, T9.2; E6's `WaitDurable` (T6.1); E5's executor and its
  durable-unlink report (T5.9); E8's `Close` (T8.12).
- **Covers:** R3, R5, R14, R20, R23, R31–R35, R41, R43–R46, R56, R57, R59–R61.
- **Deliverables:** `internal/reclaim/pass.go` and `pkg/block/journal/repack.go`:
  `Repack(ctx, budget, scope)` checks the context first, then runs units (D9): select one
  segment, copy and carry, wait for the copies to be durable, repoint under each file's index
  (held and retained entries that still name the source piece now name the copy, as one step per
  file), re-read an ended segment's resolved state, and hand the segment to E5's executor; on a
  failed copy window the unit is abandoned (D8). The pass returns `freed`, the sum of the
  retirements E5 reports durable, once they are, and stops at the budget or when no candidate
  remains. `repacked_bytes` rises by the bytes copied; the `repack_pass` span wraps the pass with
  the segments, bytes and `freed` as attributes. The pass's stop function joins it at close.
- **Acceptance criteria:**
  - With the copies' sync held at the seam, no index entry names a copy and the source is not
    retired.
  - A read racing the repoint returns the pattern from the old or the new location, never
    missing.
  - With the unlink's directory sync held, `Repack` has not returned and the storage in use has
    not moved; once it completes, `freed` equals the storage returned.
  - A crash at every step of a unit, reopened, reads every held extent once (E7a's `Open`).
  - Under sustained reads on a selected segment, the unit completes within a bounded time.
  - `Repack` with a done context appends nothing; one cancelled between units returns after the
    unit in flight.
  - Close with a pass in flight stops it before any segment closes.
- **Tests:** the seam's sync held across a unit (R31); `TestRFC1A_RepackUnderRead` and a
  reader looping over the repointed extents under `-race` (R32, R35, R59); the held directory
  sync of `TestRFC1B_FreedMeansFreed`'s repack half (R33); E2's crash-simulating seam at each
  sync point of a unit, `TestRFC1A_CrashMidRepack` and `TestDamage_LostWrites/repack_copy` and
  `/repack_unlink` (R31, R57); a reader loop against a pass with a bounded deadline (R43,
  `TestRFC1B_ReclaimNotStarved`'s shape with overwrites); a read of another file never waiting
  on a held unlink (R44); a done and a cancelled context (R46); `TestRFC1B_BackgroundJoined`
  (R45, R61); `repacked_bytes` and the span through `Stats` and a recording tracer (R34); a
  recycled segment's failed append carried and retired (`TestRFC1A_RecycledSegmentKeepsNewRecords`,
  R60); a `testing.F` target interleaving passes with writes and removals against the reference
  model.
- **Rules:** a `decision:` at the abandon on a failed copy window (D8).

### T9.7 The repack reserve and the copy's accounting

**Subtask T9.7 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T9.6; E8's admission classes (T8.6) and per-share counters (T8.5,
  T8.13); E5's reserve spares (T5.5).
- **Covers:** R36–R38.
- **Deliverables:** `internal/reclaim/reserve.go`: the pass's copy budget is the smaller of
  `budget` and what the repack reserve holds now; a unit that would exceed it waits for the next
  pass, after the reserve is whole again once the emptied segments are retired. A copy's bytes
  are counted to the journal's reserve while its source remains, and moved to the record's share
  in the step that E5 reports the source's unlink, through E8's account function.
- **Acceptance criteria:**
  - At the journal's maximum, with nothing content-admissible, a pass runs and returns space.
  - Beside a share at its limit, copies of that share's records are made and are not refused.
  - A share's `UsedBytes` may exceed its limit by the copies until the source is unlinked, and
    then reflects the copies in place of the source.
  - A pass never copies more than the reserve holds before its sources are retired.
- **Tests:** `TestRFC1B_RepackAtCapacity`'s journal-level form with overwrites in place of
  releases (R37); a share at its limit with interleaved segments (R36, R38,
  `TestRFC1B_RepackBesideShareAtLimit`'s journal-level form); the reserve shrunk to one segment and
  a budget of three, asserting the units run one per reserve (R36).

### T9.8 Sealing a share's active segments, and the per-segment accounting

**Subtask T9.8 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T9.3, T9.7; E5's seal (T5.7) and per-tag bytes (T5.1); E8's
  headroom class (T8.6).
- **Covers:** R39, R40.
- **Deliverables:** `internal/reclaim/seal.go`: before a pass scoped to a share selects, each
  stream's active segment that holds unreclaimed bytes of the share is sealed through E5's seal,
  its marker admitted from the reserved headroom, at most one per stream until a pass has retired
  it; the segment then becomes an ordinary candidate. `internal/segment/inspect.go` and
  `Inspector.Segments()`: one `SegmentInfo` per segment from E5's set and E4-9's counters, with
  `Damaged` from E6, and a retired damaged segment's row kept, marked retired, until close; the
  unreclaimed figure per segment kept internally (D16). A `decision:` at the seal names the rule
  as RFC 1 §10 question 5 words it.
- **Acceptance criteria:**
  - A share-scoped pass at the journal's limit seals the share's active segment, selects it and
    returns its unreclaimed bytes; a second pass while the first's sealed segment is still being
    retired seals no other segment on that stream.
  - A zero-scope or segment-set pass seals nothing.
  - `Inspector.Segments` agrees with a walk of the set after every step of a random sequence, and
    its held bytes sum to `HeldBytes`.
- **Tests:** a share at its limit with its unreclaimed bytes in the active segment (R39, the
  journal half of the engine's capacity row); the same with the headroom at half (catches a seal
  admitted against the share's limit); the rows against a walk (R40, `TestRFC1D_SegmentHeldBytes`'
  shape with overwrites).

### T9.9 `Forget`

**Subtask T9.9 estimate: 4.5 man-days.**

- **Track:** B. **Depends on:** T9.1, T9.4; E8's registry, tag check and conflict flag (T8.3,
  T8.4), removals and markers (T8.8, T8.9) and headroom class (T8.6); E6's `WaitDurable` (T6.1)
  and `Admit` (T6.6); E7a's `Open` applying forget records, for the crash tests.
- **Covers:** R11, R47–R54, R62, R63.
- **Deliverables:** `internal/datapath/forget.go`: `Forget(ctx, tag)` as D10 to D12: the context
  check; the forgetting mark under the registry's mutex, refused with `ErrHandleOpen` while a
  handle of the tag is open; the barrier over the tag's files; the forget record, admitted from
  the reserved headroom (a `NoSpaceError` naming the headroom when it is exhausted), appended and
  awaited through `WaitDurable`; then, per file under its append domain and the file index held
  for the write: every held and retained extent and marker dropped through E8's account
  function, the file's tag cleared, its drop ranges for segments below the record's sequence
  number cleared (so E8's "a segment holds a record of the file" no longer counts them), and its
  conflict flag cleared; the tag removed from the stranded set and the registry; T9.1's snapshot
  registered. Epic 9b's hold marks join the per-file drop. `internal/datapath/shares.go` gains the
  mark.
- **Acceptance criteria:**
  - After `Forget(T)`, crash and reopen, no extent or marker of T is held, `Since` yields nothing
    for T's files, `RemovalMarkers` does not count them, and `Stats().Shares` does not list T.
  - With a handle of T open, `Forget(T)` is refused and changes nothing.
  - `Forget(T)` twice, and `Forget` of a tag that never held a record, succeed; the second and
    the third append nothing.
  - A write through another tag's handle to a file T claimed succeeds after `Forget(T)` and is
    refused before.
  - A conflict between T and U is cleared by `Forget(T)`, and the file is then U's.
  - With the forget record's sync failed, `Forget` returns the error and T is whole.
  - With a done context, nothing is appended.
  - A tag not attached at reopen is kept and served once attached.
- **Tests:** `TestRFC1C_ForgetLeavesNothingToSettle` (R47, R48, R49, R62);
  `TestRFC1C_UnclaimedTagReported` (R63); `TestRFC1C_FileIsItsSharesShare`'s forget steps (R52,
  owned with epic 8); the cancelled-call row's `Forget` part (R54); a second forget and an empty
  tag with the seam's write log (R50); a sync failure at the seam on the forget record (D11); a
  forget racing a pass and a `Fill` of the tag's files, comparing sequence numbers (R51); a tag
  left unattached across a reopen and attached later (R53); a segment holding only T's records
  below the forget retired once the record is durable, and the forget record itself dropped by
  a pass once none remains (R11).

### T9.10 `Strand`

**Subtask T9.10 estimate: 0.75 man-days.**

- **Track:** B. **Depends on:** T8.3, T8.4.
- **Covers:** R55.
- **Deliverables:** `internal/datapath/strand.go`: `Strand(tag)` as D15, and `Stranded(tag)` for
  epic 9b's calls. `ReadAt` through another tag's handle is already refused by the tag check;
  `Strand` adds nothing to that path, and a test pins it.
- **Acceptance criteria:**
  - After `Strand(T)`, a read through U of T's file is refused and through T's own handle served.
  - `Strand` appends nothing, and after a reopen T is not stranded.
  - `Forget(T)` removes T from the set.
- **Tests:** the three criteria, with the seam's write log for the second (catches a durable
  strand).

### T9.11 Conformance rows, known failures and the model

**Subtask T9.11 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T9.1 to T9.10; E7a's `Open`.
- **Covers:** R56–R64, and the rows below.
- **Deliverables:** the conformance tests this epic turns green, each leaving
  `test/journal/known.yaml` in its change: `TestRFC1A_CopyLosesToNewerWrite`,
  `TestRFC1A_CrashMidRepack`, `TestRFC1A_RemovalOutlivesItsSegment`, `TestRFC1A_RepackUnderRead`,
  `TestRFC1A_RecycledSegmentKeepsNewRecords`, `TestRFC1B_BackgroundJoined`,
  `TestRFC1C_ForgetLeavesNothingToSettle`, `TestRFC1C_UnclaimedTagReported`,
  `TestRFC28B_SegmentReclaimable`, `TestRFC28C_RetiredSegmentsAreRecycled` (with epic 5),
  `TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead` (with epic 7a),
  `TestDamage_LostWrites/repack_copy` and `/repack_unlink`. Every other row now listed under
  epic 9 whose body needs `Offload`, `report`, `MarkOffloaded`, `Release` or a hold has its
  `reason` rewritten to name the call it waits for and its `issue` pointed at epic 9b's. The
  model run with repack and forget enabled in its operation mix.
- **Acceptance criteria:**
  - Each listed row passes and has left the list; the runner fails if a listed row passes.
  - No row listed under epic 9 still names only this epic's calls.
  - The model run over many seeds finds no divergence with repack and forget in the mix.
- **Tests:** the rows themselves; a model run of a fixed seed count in CI.
- **Rules:** the size of this burn-down depends on how many rows are written against calls only
  epic 9b provides; open point in §9.

## 7. Order of work

```mermaid
flowchart TD
  S3(["S3 walking skeleton"]):::sync --> T1["T9.1 liveness and drop test"]
  T1 --> T2["T9.2 retire what is not held"]
  T1 --> T3["T9.3 selection and scope"]
  T3 --> T4["T9.4 content copies"]
  T4 --> T5["T9.5 carried records, drops"]
  T5 --> T6["T9.6 the pass"]
  T2 --> T6
  T6 --> T7["T9.7 reserve and accounting"]
  T3 & T7 --> T8["T9.8 share seal, per-segment rows"]
  T1 & T4 --> T9["T9.9 Forget"]
  T10["T9.10 Strand"]
  C1(["after T9.2: space returns without Repack"]):::sync
  C2(["after T9.6: Repack usable by the engine"]):::sync
  C3(["after T9.9: a share can be removed"]):::sync
  T2 --> C1
  T6 --> C2
  T9 --> C3
  T6 & T7 & T8 & T9 & T10 --> T11["T9.11 rows, known failures, model"]
  E7A(["E7a Open"]):::sync -. "crash tests" .-> T6 & T9 & T11
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

- **After T9.2:** a journal that is only written, overwritten and truncated returns its space in
  whole segments, with no `Repack` call.
- **After T9.6:** `Repack` is complete for a journal-only engine; T9.7 and T9.8 make it safe at
  the limit and aimable at a share.
- **After T9.9:** a share's removal can drop its tag. T9.9 and T9.10 can run beside T9.5 to
  T9.8 once T9.4 has landed.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T9.1, T9.2 | 7.5 | — | the liveness rule and its first consumer: space returns without `Repack` |
| 2 | T9.3, T9.4, T9.5 | 11.5 | PR 1 | what a pass selects, copies and carries, tested before it is wired |
| 3 | T9.6, T9.7, T9.8 | 10 | PR 2 | `Repack` end to end, at the limit and aimed at a share |
| 4 | T9.9, T9.10 | 5.25 | PR 2 | the tag calls; can be open beside PR 3 |
| 5 | T9.11 | 3 | PR 3, PR 4 | the rows turned green and the known-failure list re-pointed |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The two rules `Repack` owes a capacity refusal.** RFC 1 §10 questions 5 and 6 say the RFC owes
  a rule in `Repack`'s contract (a pass scoped to a share seals the scope's active segments, its
  marker drawing on the headroom) and unreclaimed bytes in §4.4's per-segment breakdown. T9.8
  builds both as §10 words them, with a `decision:` marker; RFC 1 §7.2 and §4.4 should state them,
  and `SegmentInfo` gains its exported field only then (D16). Until the RFC changes, R39 and R40
  rest on an open question rather than on a section's text.
- **A never-published copy in a failed window (D8).** A repack copy is published nowhere before
  its sync. If E6's resolution fails that window, it names the copy in a loss record and counts a
  loss event, and raises the loss generation, for a record no reader ever saw, while the source
  still holds the content. Confirm with epic 6's owner that the ledger can be told a record is an
  unpublished copy, so it is dropped silently or re-appended, never counted as a loss.
- **`Share` during a `Forget` (D10).** RFC 1 says `Forget` is refused while a handle is open, not
  what `Share` does while a forget runs. D10 refuses it with `ErrHandleOpen`. Confirm with the
  RFC's author, or add the case to RFC 1 §3.
- **A file's tag after a forget.** RFC 1 says a file keeps its tag while any segment holds a record
  of it; after `Forget`, the records below the forget are still on disk until repack, and recovery
  ignores them. T9.9 clears the file's ranges for them so the running journal answers as recovery
  does. RFC 1 §3 could say so in one sentence.
- **Conformance rows written against `Release`.** Most of RFC 1 §9.4's reclamation rows make
  unreclaimed bytes by offloading and releasing (repack aimed at a share, at capacity, beside a
  share at its limit, storage returned, freed means freed, removal records dropped per extent,
  reclaim not starved, coalescing, repack copies the live part, repack verifies the whole source).
  This epic tests the same properties at the journal level with overwrites and truncates, and the
  rows stay listed for epic 9b. If the team prefers conformance-level coverage before epic 9b,
  each row gains an overwrite variant; T9.11's estimate would grow by about a day per five rows.
- **Rule 7's path.** The v1 package it names has been deleted and v2 now lives under that path,
  so the rule's import test would forbid v2's own package. The rule is copied word for word here;
  the rule should name v1 by what remains of it, or drop the import half, wherever the general
  rules are kept.
- **The forget record's snapshot and epic 7a (D3).** At open, a forget record's liveness is
  rebuilt from the segments holding records of its tag below its sequence number, which epic 7a's
  rebuild sees directly. Confirm with epic 7a's owner that its rebuild feeds T9.1's tracker with
  that set rather than a count.
