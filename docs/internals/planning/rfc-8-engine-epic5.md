# Epic 5: engine v2 removals

**Sources:** [RFC 8](../rfcs/rfc-8-engine.md) §8 (a release is the journal's `Delete`, never
its `Release`), §8.1, §8.2, §2.5 step 3 (the abort a release reports), §6.4 (the guard, as
far as a removal holds it), §12.1 (`Truncate`, `Deallocate`, `Delete`), §13 (E6, E17, E40),
§14 (this epic's series, its span and `removal_stalled`) and §15's rows for those sections;
[RFC 1](../rfcs/rfc-1-journal.md) §3.6 and §3.10, for the journal's removals, `Settle` and
the marker; [RFC 6](../rfcs/rfc-6-block-metadata.md) §3.5 and §6.2, for phase 1, the masks
and phase 2; [RFC 7](../rfcs/rfc-7-namespace-metadata.md) §4.5 and
[RFC 14](../rfcs/rfc-14-open-state.md) §9.1, for the holder re-check and `Releasing`;
[RFC 17](../rfcs/rfc-17-vfs.md) §5.3, for the release's retry channel as the service waits
on it.
**Track:** B.
**Package paths:** `feat/storage-v2`'s, after #3645 renamed `enginev2` to `engine` and
`journalv2` to `journal`.
**Epic 5 estimate: 5 subtasks, 15.25 man-days.**
The plan had 12.5: T5.1 gains the guard's
capture skip, the in-batch wait and the claim below *v* (+1.5), T5.4 lands `Delete`'s retry
channel and `ErrFileOpen`, which the plan took as on the base (+1), and T5.5 the held-batch
case (+0.25).
**Depends on:** sync point **S2, red suite in CI**: E1's surface (`Truncate`, `Deallocate`,
`Delete`, `Deps.Existence.Remove`, `Deps.Content.Resume` and `PruneRemovals`) and E2's fake
journal; epic 3, built: T3.1's per-file entries, whose pending-operation kinds for truncate
and deallocate T3.4 to T3.6 already replay; epic 4's T4.3 (the capture this epic's guard
makes skip a file, the in-batch mark phase 1 waits out, and `dropShare`), T4.5 (the one sync
function phase 1 syncs through) and T4.7 (the claim phase 1 takes a file's pending writes
with, and the reservation counter), and T4.4 (the retrying-owner mark T5.3's loop sets); epic 8's T8.1a (admission and registrations), built, whose
registrations T5.1's entry-lock section checks; epic 8's T8.1b (the removal checks) lands
with T5.1. Neither `Delete`'s retry channel nor
`vfserr.ErrFileOpen` is on `feat/storage-v2` or #3609: T5.4 lands them, from the code on
`feat/enginev2-delete-retry`. Open state's `Releasing`, merged with the stage 1 follow-up,
for one test of T5.4, through E2's fixture.
**Blocks:** epic 6, whose start step 3 reuses T5.4's abort routine, whose step 4 hands
unfinished removals to T5.3's loop, and whose `Close` and `Drop` end the loop's retries with
`ErrUnavailable`; epic 7, whose self-fence watches the loop's transactions and whose `Stats`
exposes `removal_stalled` and this epic's counters; epic 8, whose clone takes the guard and
phase 1's order from T5.1, runs its phase 2 as a clone item of T5.3's loop, and builds the
`DelayClone` branch of T5.4's channel.

---

## 1. Goal

Epic 5 gives the engine its removals. `Truncate`, `Deallocate` and `Delete` hold the file's
guard across the journal's removal and one metadata transaction that commits the file's
pending existence with it, then settle the journal's marker; one loop per journal runs each
removal's phase 2 and its prune; and a release that cannot finish inside its call is
answered pending, with a channel that yields its outcome once.

**At the end of the epic:**

- a truncate, a deallocate and a release each answer once their phase 1 has committed, and a
  read in between never reads the removed extent as **Lost**;
- a deallocate records the extent's merged hole in phase 1 and stages no zeros;
- phase 2 drops the masked refs in bounded batches, a removal whose batches keep failing
  raises `removal_stalled` and wedges no other file, and a done removal is pruned;
- a release aborted by a holder leaves the file openable and its loss reported; one stalled
  at its deadline keeps the file marked until its phase 1 commits, and its channel says so.

### 1.1 In scope

The per-file guard and the group commit's capture skipping a guarded file; the in-flight
removal mark and the registration check in one entry-lock section; the three removals
through phase 1, `Settle` and the post; truncate up as a pending grow; the removal loop per
journal with its phase 2, its prune, its backoff and `removal_stalled`; the release's
contract with the service, its retry channel and the abort routine; the series
`engine_removals_pending`, `engine_removals_oldest_seconds` and
`engine_removal_batches_total{result}`; a removal batch's root span.

### 1.2 Out of scope

- The cut gate a removal's phase 1 passes (`Deps.Gate`, RFC 12): the second part.
- A pass in flight under a removal (RFC 8 §8.3, E16), and the offload guard's other holder,
  the offer's capture: the second part. With no pass in flight, a prune's in-flight floor is
  the removal's own version.
- The shared work scheduler and its `offload.retry_min`/`retry_max`: the second part. This
  epic's loop is a FIFO with a fixed backoff (D5).
- A journal turned read-only that undoes a `Delete` (RFC 8 §8.1 step 1's exception, RFC 1
  §5.4): `journal` has no read-only state (§9, point 2).
- Rebuilding a removal from the journal's marker or the store's unfinished removals at
  start, and the abort found at start: epic 6, which reuses T5.3's loop and T5.4's routine.
- Testing `Close` and `Drop` ending a pending release with `ErrUnavailable`: built here
  (T5.3's join at `Close` and its part of `dropShare`), tested in epic 6's T6.8 and T6.9.
- A release into an extent a clone registered, and the `DelayClone` outcome of its channel:
  epic 8's T8.2, over this epic's channel. A clone's phase 2 items and its `Holds.Hold`
  retries: epic 8's T8.3, over this epic's loop.
- The service's `Releasing` mark and its `done(...)`: the filesystem service's (RFC 17 §5.3).
  The engine reads and writes no open state for a release.

### 1.3 Names used here

- **E1**, epic 1, interfaces: `pkg/block/engine`'s exported surface, `Deps` and its
  interfaces, frozen at S1.
- **E2**, epic 2, red e2e suite and test foundations: `engine-conformance` with its
  `known.yaml`, the fixture over the real journal and `kvstore`, the fault-injecting
  `kv.KV`, the fake journal and the profiler wiring.
- **S1**, interfaces frozen and `Content` agreed with the filesystem service.
- **S2**, red suite in CI: E2's suite runs on every pull request, listed whole as known
  failures; epics 3, 4 and 6 start here.
- **S3**, engine complete over the fake journal: epics 3 to 8 done, every row whose journal
  call exists green.
- **E6, E17, E19, E40**: RFC 8 §13's invariants of those numbers, not epics. E6: a removal
  holds the guard across its journal step and its first metadata transaction; E17:
  correctness never depends on the per-file guard; E19: every existence commit syncs first;
  E40: no transaction past the key budget `K`.
- **Phase 1, phase 2**: a removal's first metadata transaction, `Existence.Remove`, and its
  later batches, `Content.Resume` (RFC 8 §8.1, RFC 6 §6.2).
- **The integration epic**, epic 9: the filesystem service composed over the engine at
  RFC 17 §8's integration tier.

## 2. General rules for each epic

1. **Independent unit tests.** Each epic carries its own unit tests. It may use another
   epic's pure in-memory implementation, the implementation of an epic it depends on once
   that has landed, and E2's test components: above all the fake journal, which stands in
   for the journal wherever the logic under test is the engine's (the resolution table, the
   group commit's split, the refusal mapping). E1 declares no internal interface for it to
   fake (rule 12).
2. **Fuzz tests: none.** The engine parses no bytes of its own: it reads the journal's
   answers and the store's records through typed calls. RFC 8 §15 checks its pure parts,
   read resolution and the group commit's split, by its tables, not by fuzz, and says so in a
   `ponytail:` marker, so no fuzz target is built for them.
3. **Observability follows RFC 25, through what exists.** `Deps` carries a `*slog.Logger`
   narrowed with `With("component", "engine")` and a `Tracer`, a function type the engine
   declares over standard-library types only. RFC 25's metrics registry, watchdog and
   health source exist on neither branch, so the engine exposes its counters and
   conditions as `Stats` fields, and the root that composes it adapts them. Counters are
   named for RFC 8 §14's metrics, under the `engine_` prefix (RFC 25 §2.1), never
   `enginev2_`, with no share label on a per-operation series. Spans are RFC 8 §14's
   proposals, `engine.sync` and `engine.existence_commit`, and a removal batch opens a root
   span. Each epic names the counters, conditions and spans it owns; the units, labels and
   logging rules are RFC 25's, applied as written.
4. **Profiling.** Tests and benchmarks run under E2's profiling harness and fail on
   goroutine, descriptor or memory leaks and on anomalous resource growth. No
   allocations-per-op assertion: RFC 8 §15.4 defers its engine-cost benchmark until a
   profile shows engine time on the request path.
5. **Public interface and internals.** Tests cover both the internal package API and the
   public behaviour the epic enables, including concurrency under `-race`. Test at the
   layer that **consumes** a value (see `CLAUDE.md`, "Verifying a change"): a field the engine
   passes to the journal or the store is asserted where they read it, over the real journal
   and `kvstore`, not where the engine builds it.
6. **Known failures shrink.** An e2e test that turns green leaves the suite's `known.yaml`
   (`test/engine/known.yaml`) in the same change: `dt` fails the run if a listed test
   passes, so the change that turns it green removes it, a journal pull request included. A
   test over the real journal that starts red sits in that suite behind its build tag, never
   in `unit`, which keeps no known list.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on v1, enforced by
   the engine's one import test, a walk over every package of `pkg/block/engine` with
   one allowlist, which also refuses `pkg/vfs`: no package imports `pkg/block/syncer`,
   `pkg/block/carver`, `pkg/block/remote` or the v1 metadata service `pkg/metadata`; v1's
   engine and journal are gone on `feat/storage-v2` since #3645. The walk checks every package on its own, so its
   allowlist names each package an exception applies to, test packages included. v1 is not
   touched: it is deleted at the cutover.
8. **Real dependencies for properties of those dependencies.** No fake stands in for a
   check about what the real thing does: the real journal on a real directory, with its crash
   seam, for durability, recovery and loss; the real `kvstore` for what a transaction
   commits and conflicts on. The fake journal is for the engine's pure logic only, and no
   conformance row runs over it.
9. **Code comment conventions** (`CLAUDE.md`, "Code comments"). A source comment describes
   the code's behaviour and references nothing outside it: no issue or pull request
   numbers, no CI, runner or OS names, no epic, subtask, plan or decision ids. A
   `ponytail:` marker records a knowingly simple implementation, naming its ceiling and the
   upgrade path. A `decision:` marker records deliberate behaviour, naming the rule, why it
   holds and the evidence that would overturn it. Where the RFC carries such a note, its
   marker goes at the code site that implements it: RFC 8's copy-on-write share table, the
   per-file state bound, the loss-sequence floor, the version-limit headroom and its burst,
   the lazy existence of appends, and the stable-overwrite cost.
10. **A test must be able to fail.** Keep a test only if a plausible change would make it
    fail and nothing else would catch that change: not the compiler, not the diff, not a
    later epic's tests. A review rule that no test can check, such as "stubs never panic",
    stays a review rule.
11. **A symbol is exported only if the RFC's interface uses it.** Everything exported is
    frozen at S1. Helpers the engine's own files need stay unexported or move to an
    `internal/` package of `engine`.
12. **Each subtask creates only what it declares.** No empty package and no stub without a
    caller. A package, an option or a test helper lands with its first consumer.


### 2.1 How the rules apply

| Rule | Subtasks | How |
| --- | --- | --- |
| 1 Unit tests | all | every subtask tests over E2's fake journal and the real `kvstore` on an in-memory `kv.KV`, through epic 3's unit composition; the wedge's unit test puts a failing `Content` wrapper in through `newUnit`'s options, since E2's fault-injecting `kv.KV` (T2.3) is not built |
| 2 Fuzz | none | no parser: the engine hands typed removals to the journal and the store |
| 3 Observability | T5.1, T5.3 | `engine_removals_pending` and `engine_removals_oldest_seconds`, per share under RFC 25 §2.5's worst-n rule, and `engine_removal_batches_total{result}` with `ok`, `conflict` or `error`, in unexported fields epic 7's `Stats` exposes; `removal_stalled` naming the file and the removal's kind, logged at `Warn` on entering and leaving; each batch a root span through `Deps.Tracer` (T5.3). A wait on a file's guard or its in-batch mark is a span event (T5.1) |
| 4 Profiling | all | the unit composition calls `prof.Watch`; the loop's goroutine per journal is joined at `Close`, and a pending release's channel is buffered by one, so no sender is left blocked on it |
| 5 Public and internal | all | the loop's stall count tested internally (T5.3); the removals through `*Engine`; `-race` in T5.1, T5.4 and T5.5. What phase 1 writes (the size, the merged hole, the removal record) is asserted over the real `kvstore` |
| 6 Known failures | T5.2, T5.3, T5.5 | their rows over the real journal, §8.2 deallocate (T5.2), §8.1 removal wedge (T5.3), and §5.1 sync before existence's truncate half with the truncate-up crash (T5.5), enter `test/engine/known.yaml` naming this epic's issue, red until the journal's `Truncate`, `Deallocate`, `Delete`, `Settle` and crash seam exist |
| 7 v2 ignores v1 | none | no new import: T5.4's test over the real open-state table runs through E2's fixture, which composes it |
| 8 Real dependencies | T5.1–T5.5 | every conformance row runs over the real journal and `kvstore`; the fake journal is for the unit tests, the interleavings and the hooks, none of which stands in for a row |
| 9 Comments | all | the markers of §4 at their code sites, the removal loop's `ponytail:` among them |
| 10 Tests can fail | all | each test below names the change it catches |
| 11 Exports | T5.1, T5.4 | `Deps.Existence` gains `Size` (D4), and `Delete`'s frozen signature becomes `(retry <-chan error, err error)` with `vfserr.ErrFileOpen` beside it (T5.4), the two changes to E1's surface (§9, points 1 and 4); the hooks live in `export_test.go` |
| 12 Creates only what it declares | all | the guard lands in T5.1 with its first holder; the loop lands in T5.3 with its first item, phase 2; the channel's `DelayClone` branch lands with epic 8, its consumer |

## 3. Requirements and invariants

Every MUST and MUST NOT that RFC 8 and the journal and metadata RFCs place on this epic,
numbered so that subtasks and tests can cite them.

**The removal and its guard**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A file's release is the engine's `Delete`, which calls the journal's `Delete`, never its `Release` | invariant | RFC 8 §8 | T5.4 |
| R2 | A removal holds the file's guard across its journal step and its phase 1, and returns once phase 1 commits; the guard is per file and held across no wait on another file | MUST | §8.1, §6.4, E6 | T5.1 |
| R3 | A group commit's capture skips a file whose guard is held, without waiting on the removal's journal call, and takes it in a later batch, so no other file's commit waits on a removal | invariant | E17, §5.2 | T5.1 |
| R4 | Phase 1 commits with it the file's pending existence below the removal's version *v*, the file synced first, and calls `Done` for every write it records; a write staged above *v* between the journal step and phase 1 stays pending, for the next group commit | MUST | §8.1, §5.1, E19, RFC 6 §3.4 | T5.1 |
| R5 | The journal removes the extent and assigns the removal its version *v*; phase 1 records the size change and `Removal(file, v)` in one transaction, writes no ref and drops none | MUST | §8.1 steps 2–3, RFC 6 §6.2 | T5.1 |
| R6 | After phase 1 commits, the engine releases the guard, calls `Settle(id, v)` and posts the removal to its journal's loop, in that order; a crash before `Settle` leaves the marker for recovery | MUST | §8.1 step 4, RFC 1 §3.10 | T5.1 |
| R7 | A truncate holds the file's entry lock across the journal's `Truncate` and the insert of its pending operation, so a `ReadAt` landing between them never reads the extent **Lost** | invariant | §7.1, RFC 1 §3.6 | T5.1, T5.5 |
| R8 | The registration check and the in-flight removal mark sit in one entry-lock section, and the mark lasts until phase 1 ends: a truncate or deallocate into a registered extent answers `DelayClone` at once, and a clone admitted meanwhile waits the removal out | invariant | §6.4, §9.1, E6 | T5.1 |
| R9 | A truncate to a size at or above the file's replayed size is a pending grow: the journal's `Truncate` assigns it its version, the entry records it, and the group commit commits it, with no phase 1 | invariant | §8, RFC 1 §3.6, RFC 6 §6.2 | T5.1 |
| R10 | Phase 1 waits out a batch in flight on its file before it commits, so its removal never lands above operations the batch captured and has yet to commit | invariant | §5.1 step 4, RFC 6 §3.4 | T5.1 |

**Deallocate**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R11 | A deallocate's phase 1 writes the extent's merged hole at *v*, under the merged hole's start key, at once rather than in phase 2's last batch | MUST | §8.2, §8.1 step 3 | T5.2 |
| R12 | A deallocate never stages zeros through the write path | MUST NOT | §8.2 | T5.2 |
| R13 | §8.2 deallocate: an extent larger than free journal capacity deallocates, reads as zeros, and consumes no journal capacity | test row | §15.1 | T5.2 |

**Phase 2 and the loop**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R14 | The refs a removal drops are dropped later, in batches within the key budget `K` from its cursor, by version, until done; until then the removal masks them from every read | MUST | §8.1 step 5, E40 | T5.3 |
| R15 | A done removal is pruned by the file's primary once at or below the in-flight floor, which with no pass in flight is its own version | MUST | §8.1, RFC 6 §6.2 | T5.3 |
| R16 | A failed phase-2 batch or prune is retried with a backoff for as long as the binding is served; after five consecutive failures the removal is `removal_stalled`, naming the file and the removal's kind, cleared by its next batch that commits; the count is kept per loop item, which is per file while the guard admits one removal of a file at a time | MUST | §8.1 | T5.3 |
| R17 | A removal that keeps failing wedges no other: the loop moves past a backed-off item, and other files' removals complete | invariant | §8.1, §15.2 | T5.3 |
| R18 | §8.1 removal wedge (adapted: a removal whose batches keep failing raises `removal_stalled` and wedges no other file; the undecodable ref is the second part's) | test row | §15.2 | T5.3 |

**The release**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R19 | A release reads and writes no open state in the engine: the service has marked the file releasing before it calls `Delete` | invariant | §8.1 step 1 | T5.4 |
| R20 | Phase 1 of a release re-checks the file's holders and aborts if one has arrived; the abort is settled without its removal and reported as data loss naming the file, and the pending writes below its *v* it dropped get `Done` with nothing charged; writes staged above *v* stay pending | MUST | §8.1 step 3, §2.5 step 3 | T5.4 |
| R21 | A release whose phase 1 has not committed at the call's deadline, the journal's `Delete` done, is pending, not failed: `Delete` answers a channel and a nil error, and retries phase 1 from the loop with no goroutine of its own, keeping the guard and the in-flight mark until it commits or ends | MUST | §8.1 | T5.4 |
| R22 | The channel yields the outcome once: nil once phase 1 commits, the abort's `ErrFileOpen`, or `ErrUnavailable` when the loop ends first | MUST | §8.1, RFC 17 §5.3 | T5.4 |
| R23 | A pending release that keeps failing raises `removal_stalled` for the file after five failures, and its commit or abort clears it | MUST | §8.1 | T5.4 |
| R24 | A pending release, once its retried phase 1 commits, runs step 4: the guard released, `Settle`, the post | MUST | §8.1 step 4 | T5.4 |
| R25 | `ErrFileOpen` passes only between the engine and the service: `vfs.Errors()`, which RFC 20 §7.3 checks each adapter's table against, omits it | invariant | RFC 17 §4.3, RFC 20 §7.3 | T5.4 |

**Interleavings**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R26 | §5.1 sync before existence, the truncate half: every `size` phase 1 commits is backed by synced records across a host crash right after it | test row | §15.1 | T5.5 |
| R27 | Removals interleaved with writes and group commits leave the file reading what the version order says: a write after a removal reads back, one before it inside the extent reads as removed | invariant | §8.1, RFC 6 §3.4 | T5.5 |

## 4. Decisions this epic takes

- **D1, the guard.** One atomic flag in the file's entry, set under the entry's lock before
  the journal step and cleared under it after phase 1, with waiters on the entry's
  condition: a second removal of the file waits for it, under its own context. The group
  commit's capture reads the flag without taking the entry's lock and skips a guarded file
  (R3), so it never waits behind a truncate that holds that lock across its journal call
  (D2). The entry holding the guard stays alive as one holding operations does (epic 3's
  bound). A wait on it is a span event. A `decision:` marker at the capture's skip says a
  removal never waits on another file's commit and the skipped file commits in a later
  batch, under E17.
- **D2, the truncate's lock.** The entry's lock is held across the journal's `Truncate` and
  the insert of its pending operation, unlike a write (epic 3's D3): the journal stops
  holding the extent at once, and a `ReadAt` between the two would find it missing with no
  pending truncate yet and read **Lost**. Reads of the file wait out one journal removal. A
  `decision:` marker at the lock names it, withdrawn if the journal can hand back the
  removal's version before it stops holding the extent. `Deallocate` holds it the same way
  (T5.2), under the same marker. Before deciding grow or removal and before the journal
  call, the same lock waits out writes already admitted (epic 8's T8.1a builds the
  admitted-write count on the entry; the removal waits on its condition with the context's
  `AfterFunc`, as a registration waits): a write in the journal but not yet in the entry
  would otherwise sit below *v* unseen. A `Commit` on a guarded file waits out the guard, or
  every batch would skip the file and the `Commit` would spin. Epic 4's `commitThrough` (D10)
  has no such wait, so T5.1 adds it: on a guarded file it waits for the guard's release,
  then finds its target covered once phase 1 has removed the operations it committed, and
  wakes no group commit.
- **D3, the in-flight mark.** The same entry-lock section checks the file's registrations
  (epic 8's T8.1b), answering `DelayClone` for a truncate or deallocate overlapping a source
  registration's extent and for any truncate or deallocate of a file holding a destination
  registration, and marks the removal in flight in the entry. The mark is cleared when phase
  1 commits or fails, or, for a pending release, when its retried phase 1 commits or ends
  (R21). A clone registering the extent waits on it under its own context. A `Delete` into
  a registration is not answered `DelayClone`: it is answered pending, before its journal
  step, by epic 8's T8.2. A `decision:` marker at the section names both halves and why they
  share it: a removal that checked before a clone registered could still drop the clone's
  source bytes.
- **D4, truncate up.** The engine keeps no committed size, so `Truncate` reads it with one
  store read, `Existence.Size`, which T5.1 adds to `kvstore` and to `Deps.Existence`, and
  replays the entry's pending operations over it to decide. Either way the journal's
  `Truncate` runs and assigns the version: at or above the replayed size the entry records a
  pending grow (`kvstore.OpGrow`) at that version and the call answers, below it the call is
  a removal. A `decision:` marker says a grow needs no phase 1, since it masks nothing.
- **D5, the loop.** One goroutine per journal, a FIFO of items (removal phase 2, prune,
  pending release, and epic 8's clone items), each failure retried after a fixed 1 s. The
  marker, at the loop:
  ```go
  // ponytail: removal phase 2 and its prune run from one loop per journal, a
  // FIFO with a fixed backoff, not the shared work scheduler. Replace it with
  // the scheduler's removal source when offload adds the first work that needs
  // fairness between shares.
  ```
  An item answers done, retry (counted toward the five) or retry-uncounted, the last for
  epic 8's `Holds.Hold` answered `DelayFrozen` or `ErrStopped`.
- **D6, the abort's value.** `kvstore` answers a release a holder still holds with
  `kvstore.ErrHeld`; the engine answers it `vfserr.ErrFileOpen`, which T5.4 adds: the
  service tells it apart from `ErrUnavailable`, clearing the mark for one and keeping it for
  the other. The routine that settles the abort without its removal, reports the loss at
  `Error` naming the file and calls `Done` for the dropped writes is one function, which
  epic 6's start step 3 reuses. A `decision:` marker at the mapping names it.
  `kvstore` asks a release's holders in a read before `Remove` commits the release's pending
  writes, so an aborted release leaves them uncommitted (R20); `releasePhase1`'s own check
  stays the authority, and a `decision:` marker at `kvstore` names the window between the two.
  The abort drops only the pending operations below *v*.
- **D7, the pending release's deadline.** `Delete` runs phase 1 within the caller's
  context; once the journal's `Delete` has returned, a phase 1 cut off by the context's end
  or answering a retry-later store error hands the item to the loop and returns its
  channel. Before the journal's `Delete`, the context's end is a refusal that changed
  nothing. A `decision:` marker at the hand-off names the point past which a release cannot
  fail.
- **D8, what phase 1 takes.** Phase 1 claims only the file's pending operations below *v*,
  through epic 4's claim; one staged between the journal step and phase 1 is above *v* and
  stays in the entry, unclaimed. `kvstore`'s `Remove` skips a removal at or below `applied`,
  so a claim that took an operation above *v* would raise `applied` past *v* and lose the
  removal. Once phase 1 commits, the claimed operations and the removal's own pending
  operation leave the entry. A `decision:` marker at the claim names the bound.
- **D9, a phase 1 that fails after the journal step.** The extent is already gone, so the
  call cannot fail: the removal is handed to the loop, which keeps the guard and retries
  phase 1, and the call answers `ErrDelay` (`DelayConflict` for a conflict, `DelayCommit`
  otherwise). This holds for truncate and deallocate as for a release; RFC 8 §8.1 defines
  this for truncate and deallocate too since #3716. A client's retry is idempotent.
- **D10, terminal failures.** The file gone (`ErrStale`), or the removal refused as invalid
  (`kvstore.Refused`), ends phase 1 at once, in the call or in the loop: the removal's
  operation leaves the entry, the guard is released, and the journal's marker is settled
  without the removal, a loss logged at `Error` naming the file when it still exists (RFC 8
  §8.1 since #3716; a marker a crash left unsettled is settled at start step 3, epic 6). The
  loop ending or the share dropped ends a retried phase 1 the same way. Without the class, a
  deterministic refusal would hold the guard forever.
- **D11, truncate above the size in `kvstore`.** A truncate removal whose new size is above
  the File record's, as a failed window's dropped write can make it, sets the size with the
  gap a hole and records no removal, rather than refusing, as RFC 6 §6.2 says since #3716.

## 5. Target tree

```text
pkg/block/engine/
  remove.go         T5.1  the guard, the in-flight mark, Truncate, the shared phase 1 (D1–D4, D8–D10)
                    T5.2  Deallocate
                    T5.4  Delete, the retry channel, the abort routine (D6, D7)
  removals.go       T5.3  the removal loop per journal, its backoff, removal_stalled, its part of dropShare (D5)
  file.go           T5.1  the entry's guard and in-flight mark
  groupcommit.go    T5.1  the capture's skip of a guarded file, commitThrough's wait for its release
  deps.go           T5.1  Existence.Size
  ../../metadata/kvstore/  T5.1  Size, a truncate above the size as a hole (D11), Refused; T5.4  the holders read before Remove commits (D6)
  unbuilt.go        T5.1, T5.2, T5.4  each built method leaves it
  export_test.go    T5.1–T5.5  hooks inside the journal's removals and between them and phase 1
  conformance/
    rfc8_removal_test.go  T5.2, T5.3, T5.5  the rows of rule 6
pkg/vfs/vfserr/
  vfserr.go         T5.4  ErrFileOpen
pkg/vfs/
  errors.go         T5.4  Errors() omits ErrFileOpen
pkg/metadata/kvstore/
  existence.go      T5.1  Existence.Size (D4)
test/engine/known.yaml  T5.2, T5.3, T5.5  their entries
```

## 6. Subtasks

### T5.1 The removals through phase 1

**Subtask T5.1 estimate: 4 man-days.**

- **Track:** B. **Depends on:** epic 4's T4.3, T4.5 and T4.7; epic 8's T8.1a; T8.1b lands with it.
- **Covers:** R2–R10.
- **Deliverables:** `remove.go`: the guard (D1) and the capture's skip in `groupcommit.go`;
  the entry-lock section with the in-flight mark (D3), in which T8.1b puts the registration check;
  `Truncate`, up and down (D2, D4); the phase 1 shared by the three removals: the guard
  taken, epic 4's `waitBatch` (R10), the claim of the pending operations below *v* (D8),
  the sync through epic 4's sync function, `Existence.Remove` with the claimed operations,
  `Done` for each once it commits, the claim released on failure; then, under the entry's
  lock and before the guard is released, the claimed operations and the removal's own
  pending operation removed from the entry, which is memory only and leaves RFC 8 §8.1 step
  4's order standing; then the guard released, `Settle(id, v)` and the post to the loop. A phase 1
  that fails after the journal step cannot simply fail, since the extent is gone: it is handed
  to the loop, which keeps the guard, and the call answers `DelayConflict` or `DelayCommit`
  (D9), for truncate and deallocate as for a release. A terminal failure ends it instead
  (D10). `Existence.Size` in `kvstore` and `Deps`. The wait on T8.1a's admitted-write count
  before the grow-or-removal decision and the journal call (D2); in `groupcommit.go`,
  `commitThrough`'s wait for a guard's release (D2).
- **Acceptance criteria:**
  - a truncate answers once its phase 1 committed, and the File record's size is the new
    one; a truncate up records a grow at the journal's version and makes no phase 1;
  - the pending writes phase 1 records get their `Done` once, by the reservation counter,
    and a write staged above *v* meanwhile commits later, the truncate still holding;
  - a group commit while a removal's journal call is blocked commits the journal's other
    files and takes the guarded one later;
  - a truncate into a registered extent answers `DelayClone`; a clone registered while a
    truncate is in flight returns only after its phase 1;
  - after phase 1, the entry holds neither the truncate's pending operation nor the writes
    it committed;
  - a write admitted before a truncate is committed by its phase 1, and a `Commit` on a
    guarded file answers once phase 1 commits.
- **Tests:** over the fake and the real `kvstore`: a truncate's phase 1 writes the size and
  the removal record, then `Settle` once at *v* after the guard is released (catches a
  settle before the commit, or the guard held across it); the reservation counter at zero
  after a truncate over pending writes (catches phase 1 without `Done`); a hook between the
  journal's `Truncate` and phase 1 stages a write at *v*+1: after both, the size is the
  truncate's grown by the write, never the pre-truncate size (catches a claim of every
  pending operation, whose `applied` above *v* makes `Remove` a no-op, D8); a hook inside
  the fake journal's `Truncate` blocks it while a group commit runs: the journal's other
  files commit and the guarded one is skipped (catches a capture that takes the entry's
  lock, R3); a batch held between its capture and its `Existence.Commit` on the file: the
  truncate's phase 1 starts only after the batch returns, and the captured writes are
  committed, not skipped (catches R10); a hook between the journal's `Truncate` and phase 1
  registering a clone over the extent: the clone's registration returns only after phase 1
  (catches the mark released early, R8); a truncate up makes no `Existence.Remove` call and
  its grow commits through the group commit (catches a grow taken for a removal, R9); a
  group commit triggered by a hook just after the guard is released captures neither the
  truncate's own operation nor the writes phase 1 committed (catches them removed after the
  release, when a capture would hand `kvstore` a truncate down, which it refuses as a
  removal); a hook between a write's `WriteAt` and its insert while a truncate comes in: the
  truncate's journal call starts only after the insert, phase 1 claims the write and its
  `Done` runs once (catches a removal that does not wait out admitted writes, leaving one
  below *v* that no commit takes, R4); a `Commit` over a pending overwrite on a file whose
  phase 1 is held by a hook, the clock pinned so D5's `existence_age` never puts the file
  back on the due set, answers after phase 1 commits, with the group commit skipping the
  file at most once meanwhile (catches a `Commit` that spins on batches that skip a
  guarded file).

### T5.2 `Deallocate`

**Subtask T5.2 estimate: 1 man-days.**

- **Track:** B. **Depends on:** T5.1.
- **Covers:** R11–R13.
- **Deliverables:** `Deallocate` over T5.1's phase 1 with `kvstore.RemoveDeallocate`, the
  entry lock held across the journal's `Deallocate` and the insert, under D2's marker.
- **Acceptance criteria:**
  - after phase 1 and before phase 2, the store holds one hole at *v* covering the extent
    joined with the holes touching it;
  - the journal's allocated bytes do not grow.
- **Tests:** over the real `kvstore`: the merged hole present after phase 1 with phase 2
  held by the loop's hook (catches a hole left to phase 2, R11); a write into the extent
  between phase 1 and phase 2 reads back, not zeros (catches the same bug through its
  consumer); over the fake, no `WriteAt` during a deallocate (catches zeros staged, R12). A
  conformance row over the real journal for §8.2 deallocate, red until the journal's
  `Deallocate` exists.

### T5.3 The removal loop

**Subtask T5.3 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T5.1; epic 4's T4.4 (the retrying-owner mark).
- **Covers:** R14–R18.
- **Deliverables:** `removals.go`: the loop per journal (D5), joined at `Close`, which ends
  its pending releases' channels with `ErrUnavailable`; phase 2 through `Content.Resume`,
  the prune through `PruneRemovals(…, v)`; the stall count per loop item and
  `removal_stalled`, raised and cleared; an item's three answers (done, retry,
  retry-uncounted); `enqueue` for epic 6's start step 4 and epic 8's clone items; the loop's
  part of epic 4's `dropShare(s)`, ending the share's items with their pending releases'
  channels yielding `ErrUnavailable`, built here and tested in epic 6's T6.9; the series of
  §2.1 and each batch's root span; every store call the loop retries made under epic 4's
  retrying-owner context mark, which T4.4 builds and epic 7's self-fence reads (its D7), so the self-fence counts a phase 1 or batch that
  fails at the store as a whole.
- **Acceptance criteria:**
  - a truncate's masked refs are gone once the loop has run, and its removal record pruned;
  - a removal whose batches fail five times running raises `removal_stalled` naming the file
    and `truncate`, and the next batch that commits clears it;
  - another file's removal in the same journal completes while the first is backed off.
- **Tests:** over the fake's clock: five failures raise the condition and four do not
  (catches an off-by-one or a count across files); a retry-uncounted answer leaves the count
  where it was (catches epic 8's hold retries counted); with a `Content` wrapper through
  `newUnit`'s options failing one file's batches, the other file's removal completes and the stalled one
  finishes once the fault clears (catches a loop that waits on a failing item, R17). Over
  the real `kvstore`: no ref under the removal's mask after the loop, the record pruned
  (catches a prune skipped). The §8.1 removal wedge row (adapted), a conformance row over the
  real journal and E2's fault-injecting `kv.KV` (T2.3), red until the journal's `Truncate` and
  `Settle` exist.

### T5.4 A release's contract with the service

**Subtask T5.4 estimate: 4.5 man-days.**

- **Track:** B. **Depends on:** T5.1, T5.3.
- **Covers:** R1, R19–R25.
- **Deliverables:** E1's `Delete` signature changed to `(retry <-chan error, err error)` and
  `vfserr.ErrFileOpen` added, with `vfs.Errors()` omitting it; the code that lands them is
  on `feat/enginev2-delete-retry` (the engine's signature, `vfserr`, and `internal/vfs`'s
  wait on the channel), and RFC 17 §4.3's sentence that `Errors()` omits a value only the
  service and the engine exchange lands with the RFC pull request that carries it. In
  `remove.go`: `Delete`: the guard, the journal's `Delete` (never `Release`), phase 1 with
  `RemoveRelease`; the abort mapping and routine (D6); the hand-off to the loop (D7) with a
  channel buffered by one, yielding nil on commit, `ErrFileOpen` on an abort,
  `ErrUnavailable` when the loop ends first; the guard and the in-flight mark kept across the
  retries (R21); step 4 run once the retried phase 1 commits; `removal_stalled` after five
  failed retries, cleared by the commit or the abort. The `DelayClone` branch is epic 8's,
  and `Close`'s and `Drop`'s `ErrUnavailable` are tested in epic 6.
- **Acceptance criteria:**
  - a release whose phase 1 commits within the call answers a nil channel and nil;
  - one whose holder re-check finds an open answers `ErrFileOpen`, the loss logged naming
    the file, the reservation counter at zero;
  - one stalled at its deadline answers a channel and nil, and the channel yields nil once
    the store recovers; one stalled past five retries raises `removal_stalled`, and the abort
    that ends it clears it and yields `ErrFileOpen`, not `ErrUnavailable`;
  - `vfs.Errors()` does not list `ErrFileOpen`.
- **Tests:** over the fake: the journal's `Release` never called by `Delete` (catches the
  eviction call, R1); a store stalled past the deadline, then cleared: the channel yields
  nil once and the file's marker is settled (catches a pending release answered as failed);
  a stall ended by an abort yields `ErrFileOpen` (catches the catch-all mapping it to
  `ErrUnavailable`, D6); `vfs.Errors()` omits `ErrFileOpen` while every other `vfserr` value
  is listed (catches it added to the adapters' table, R25). Over E2's fixture with the real
  open-state table and `kvstore`: an open by handle racing the last close's release loses no
  bytes, and an aborted phase 1 leaves the file openable (R20).

### T5.5 Removals interleaved with writes and commits

**Subtask T5.5 estimate: 2.75 man-days.**

- **Track:** B. **Depends on:** T5.2, T5.3.
- **Covers:** R7, R26, R27.
- **Deliverables:** tests only, and the fake journal's hooks they need in `export_test.go`.
- **Acceptance criteria:**
  - interleavings of writes, truncates, deallocates and group commits under `-race` read
    back what their version order says;
  - a crash right after a truncate's phase 1 leaves every committed size backed by synced
    records, and a crash after a truncate up's journal step, before its group commit, leaves
    the new size served after the restart.
- **Tests:** a hook in the fake journal between its `Truncate` or `Deallocate` and the
  call's return starts a `ReadAt` of the removed extent on another goroutine, and waits
  until that read has either returned or is blocked on the file's entry lock (a lock-wait
  hook) before letting the removal go on: the read then finds the pending removal, never
  **Lost** (a build that releases the lock before the insert lets the read return inside
  the hook, reading **Lost**, every run, R7). A randomised interleaving test against a model
  of the version order (catches a removal applied out of order, R27). Conformance rows over
  the real journal and its crash seam: the truncate half of §5.1 sync before existence (R26),
  and a truncate up crashed before its group commit, whose size the restart serves (catches
  a grow with no journal step, which recovery cannot replay), both red until the journal's
  crash seam exists; the restart is epic 6's, which the row composes.

## 7. Order of work

```mermaid
flowchart TD
  E4(["epic 4: T4.3 group commit, T4.5 sync function, T4.7 claim"]):::sync --> T5_1["T5.1 Removals through phase 1"]
  E8(["epic 8: T8.1a registrations"]):::sync --> T5_1
  T5_1 --> T5_2["T5.2 Deallocate"]
  T5_1 --> T5_3["T5.3 Removal loop"]
  T5_1 --> C1(["after T5.1: the guard and phase 1, epic 8's T8.2 can start"]):::sync
  T5_3 --> T5_4["T5.4 Release contract"]
  T5_3 --> C2(["after T5.3: the loop, epics 6 and 8 can enqueue"]):::sync
  T5_2 & T5_3 --> T5_5["T5.5 Interleavings"]
  T5_4 & T5_5 --> C3(["after T5.5: removals complete over the fake journal"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T5.1 is the riskiest, for the entry-lock section shared with the clone's registration and
the wait on a batch in flight, and starts as soon as epic 4's T4.3, T4.5 and T4.7 land.
Checkpoint after T5.1: epic 8's T8.2 can take the guard. Checkpoint after T5.3: epic 6's
start step 4 and epic 8's phase 2 can enqueue.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T5.1, T5.2, epic 8's T8.1b | 5 (+1, epic 8's T8.1b, counted there) | epic 4's PR 2 and PR 3; epic 8's PR 1 (T8.1a) | the three removals' phase 1 with the guard and the mark, and the registration check T8.1b builds in T5.1's entry-lock section |
| 2 | T5.3, T5.4 | 7.5 | PR 1 | the loop and the release that retries from it |
| 3 | T5.5 | 2.75 | PR 2 | the interleavings and the crashes, once every removal exists |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

1. **Truncate up reads the committed size** (D4): one store read per truncate, and a method
   added to E1's frozen `Deps.Existence`, which the filesystem service could spare by
   passing the size, since it holds the File record. RFC 8 §12.1's `Truncate` takes only the
   new size; RFC 8 or RFC 17 §5.5 should say which.
2. **A journal turned read-only that undoes a `Delete`** (RFC 8 §8.1 step 1, RFC 1 §5.4):
   `journal` has no read-only state, so the release that fails before phase 1, its
   `ErrUnavailable` and the mark kept until the process ends are untested here.
3. **The loop's backoff** (D5) does not read `offload.retry_min`/`retry_max`, which RFC 8
   §8.1 names; the second part's scheduler brings them.
4. **`Delete`'s signature changes E1's frozen surface.** The retry channel and
   `vfserr.ErrFileOpen` are on `feat/enginev2-delete-retry`, pushed with no pull request yet,
   and RFC 17 §4.3's sentence on `Errors()` on a local RFC branch; T5.4 lands the code, and
   the sentence lands with the RFC pull request after #3608.
