# Epic 4: engine v2 commit, group commit and losses

**Sources:** [RFC 8](../rfcs/rfc-8-engine.md) §4.1 (the write verifier), §5.1, §5.2, §11.3,
§12.1 (`Commit`, `LossSeq`, `WriteOpts.Done`), §13 (E15, E19, E24, E27, E31, E35, E40), §14
(this epic's series and spans) and §15's rows for those sections;
[RFC 1](../rfcs/rfc-1-journal.md) §3.8, §5.2 and §5.3, for the journal's sync, loss events
and failed windows; [RFC 6](../rfcs/rfc-6-block-metadata.md) §3.3, §5.2 and §10.1, for
overwrite records, the key budget `K` and `Existence`; [RFC 17](../rfcs/rfc-17-vfs.md) §5.6
and §5.8, for `Done` and the verifier the service hands its clients.
**Track:** B.
**Package paths:** `feat/storage-v2`'s, after #3645 renamed `enginev2` to `engine` and
`journalv2` to `journal`.
**Epic 4 estimate: 7 subtasks, 22 man-days.**
**Depends on:** sync point **S2, red suite in CI**: E1's surface (`Commit`, `LossSeq`,
`WriteOpts.Done`, `Settings`, `Deps.Existence`) and E2's fake journal; epic 3, built:
T3.1's per-file entries and their drop of an empty entry marked dead, T3.2's `Write`, which
records each pending operation with its `Done`, and its stand-in verifier (`zeroVerifier`, behind the engine's `sample` field),
which T4.1 replaces. #3608 and #3609, which put the share on every content call.
**Blocks:** epic 5, whose phase 1 commits a file's pending existence through this epic's
claim and calls its `Done`s, and whose removal record the group commit's due rule must
precede; epic 6, whose recovery commits each file's existence through T4.3's per-file
commit, whose `Drop` takes a share's files out of T4.3's due set, and whose tests count
`Done` with T4.7's reservation counter; epic 7, whose capacity sync feeds T4.6's per-file
report, whose `Stats` exposes T4.4's `existence_commit_failing` and this epic's counters, and
whose self-fence watches the group commit's transactions through T4.4's retrying-owner mark; epic 8, whose clone commits its
source's pending existence through T4.3's per-file commit before it reads `asOf`.

---

## 1. Goal

Epic 4 gives the engine its stability point and everything that commits existence. `Commit`
syncs the file and answers, waiting for a metadata transaction only when a pending write
overwrites committed content; one group commit per journal commits the rest within a bounded
age, in bounded batches, retrying only the files that fail; every accepted write gets its
`Done` exactly once; and the verifier and the loss sequence change exactly when an
acknowledged write may have been lost.

**At the end of the epic:**

- a `Commit` whose extent covers no committed content is answered after the journal's
  sync, with no metadata transaction; one whose extent covers committed content also
  waits for a recorded sync mark; one over an overwrite of committed content waits for the group
  commit that covers its file, and under a stalled store answers `ErrDelay` at its
  deadline, never success;
- every file's pending existence is committed within `existence_age`, one transaction per
  batch of at most `G` files, and a file that fails is retried alone, never delaying another;
- the verifier changes across a restart and after a loss of an acknowledged write the remote
  tier does not hold, and on nothing else;
- every write `Write` accepted gets `Done` exactly once, whether its existence commits, a
  failed window drops it, its loss record commits or its journal's device is lost whole.

### 1.1 In scope

The verifier and its five inputs; `Commit` and `LossSeq`; the group commit per journal, its
due set, its batches, its in-batch mark, its split and its backoff; the per-file commit
other epics call and the wait on a batch in flight; the
loss sequence, the journal-wide floor and the drain of the journal's loss ring; the failed
window reported per file; `Done` on the four paths this epic builds; the split of
`kvstore.PendingOp`'s time into a modify and a change time; `kvstore`'s marks write; the series
`engine_stability_points_total`, `engine_pending_existence_ops` and
`engine_group_commit_splits_total`; the spans `engine.sync` and `engine.existence_commit`;
the condition `existence_commit_failing`. The test components it adds to E2's: the fake
journal's sync marks, `FailSync(also)` and `Durable`, the fixture's `FailNextSync` and `Stall`, and
the engine test package's `newUnitOver`.

### 1.2 Out of scope

- `Truncate`, `Deallocate`, `Delete`, the per-file guard and the removal's phase 1, whose
  `Done` path is another of `Done`'s: epic 5. The group commit's capture skipping a file whose guard
  is held, and `commitThrough`'s wait for the guard's release (epic 5's D2), land with the
  guard, in T5.1.
- Rebuilding entries from `Since` and committing recovered existence before a file is served,
  the incarnation raise at start, `Close`'s join, `Forget`'s `Done` and `Drop`'s removal from
  the due set: epic 6. This epic gives them the per-file commit and the due set's removal.
- The capacity refusal table, the capacity sync, the self-fence and `Stats`'s fields: epic 7.
- The aborted release's `Done` path: epic 5, T5.4.
- Between the drain and T4.6, a failed window's write: T4.5's drain raises the file's loss
  sequence for a `LossFailedWindow` event, but leaves its pending write in the entry, so a
  group commit still commits it as written; T4.6 drops it from the entry and from any batch
  that claimed it, with `Done` charging nothing. Until T4.6 lands, the sequence is right and
  the size may not be.
- A start past an acknowledged device loss starting each attached file's loss sequence at 1
  (RFC 8 §5.1): epic 6, over T4.5's floor.
- The cut (`PrepareCut`, `CommitCut`, `Deps.Gate`) and the stamp a group commit writes after
  it commits (RFC 12): the second part. No existence commit here passes a gate.
- An offer's capture, offload's commit and the "a commit is reported only once durable" row
  for offload commits: the second part.
- The stop-serving sweep's `Done` (a shard moved, handed over or its lease lost): a cluster,
  which this part does not have.
- A journal device that dies under the running process: this epic builds what the engine
  does about it (T4.5), but the journal reports no such state yet (§9, point 3).

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
- **E15, E19, E24, E27, E31, E35, E40**: RFC 8 §13's invariants of those numbers, not epics.
  E15: existence answered from the journal until committed; E19: every existence commit
  syncs first; E24: a group-commit conflict delays only the conflicting files; E27: the
  verifier changes exactly when an acknowledged unstable write may have been lost; E31: a
  stability point waits for the sync and, for an overwrite, its existence commit; E35: a
  failed window reported once per file; E40: no transaction past the key budget `K`.
- **`G`, `K`**: the group commit's file bound, `group_commit.max_files`
  (`Settings.GroupCommitMaxFiles`), and RFC 6 §5.2's key budget, which `kvstore`'s
  `Existence.Commit` applies itself.
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
| 1 Unit tests | all | every subtask tests over E2's fake journal and the real `kvstore` on an in-memory `kv.KV`, through epic 3's unit composition; the split's test puts a `kv.KV` wrapper under `kvstore` through `newUnitOver`, and the stalled store's a stalling `Existence` wrapper, since E2's fault-injecting `kv.KV` does not exist |
| 2 Fuzz | none | no parser: the group commit's split is checked by its table test (T4.4), which carries RFC 8 §15's `ponytail:` marker |
| 3 Observability | T4.2, T4.3, T4.4 | T4.2 counts `engine_stability_points_total` and opens `engine.sync`; T4.3 records `engine_pending_existence_ops` (the operations one commit applied) and opens `engine.existence_commit`; T4.4 counts `engine_group_commit_splits_total` and raises `existence_commit_failing` naming the file, logged at `Warn` on entering and leaving. Counters in unexported fields epic 7's `Stats` exposes; none labelled by share |
| 4 Profiling | all | the unit composition calls `prof.Watch`; T4.3's timer and its batch goroutine are joined by `closeGroupCommits`, which the tests call as epic 6's `Close` will, so a leak fails the test that composes the engine |
| 5 Public and internal | all | the split (T4.4) and the claim (T4.7) tested internally; `Commit` and `LossSeq` through `*Engine`; `-race` in T4.3, T4.4 and T4.7. What the commit writes, the split times and the loss op's charge included, is asserted over the real `kvstore` (T4.3, T4.5), and the verifier where the service reads it, by `Commit`'s answer against `Write`'s (T4.1) |
| 6 Known failures | T4.1, T4.2, T4.3, T4.4, T4.6 | `TestRFC8A_Verifier` and `TestRFC8A_StableWriteLazyExistence` are already listed, naming epic 6, whose start they also need; T4.1 and T4.2 add their engine assertions and leave those entries. The new rows, verifier inputs (T4.1), overwrite waits for existence and stalled store (T4.2), sync before existence (T4.3), group commit (T4.4), and failed window with verifier after a loss (T4.6), enter `test/engine/known.yaml` naming this epic's issue, red until the journal's `Init`, `Sync`, crash seam and loss events exist; a row an epic-4 change turns green leaves it in that change |
| 7 v2 ignores v1 | none | no new import |
| 8 Real dependencies | T4.1, T4.2, T4.3, T4.4, T4.6 | every conformance row runs over the real journal and `kvstore`; the fake journal is for the unit tests of the engine's logic only, none of which stands in for a row |
| 9 Comments | all | the markers of §4 at their code sites, and three of RFC 8 §5.1's: the loss sequence's floor (`ponytail:`, T4.5), the lazy existence of appends (`decision:`, T4.2) and the stable-overwrite cost (`ponytail:`, T4.2) |
| 10 Tests can fail | all | each test below names the change it catches |
| 11 Exports | all | nothing new is exported from `engine`; the test hooks live in `export_test.go`, the reservation counter in `enginetest`. `kvstore.PendingOp`'s new fields and `OpLost` are `kvstore`'s own, used by its caller |
| 12 Creates only what it declares | all | the per-file commit lands in T4.3 with its first consumer, `Commit` (T4.2); the sync function lands in T4.5 with its first consumer, the drain; the retrying-owner mark lands in T4.4 with its first setter, the group commit, or its reader, epic 7's T7.2, whichever lands first: epic 4 blocks epic 7, so a dependency of T4.4 on T7.2 would be a cycle, and T7.2 does not depend on T4.4 |

## 3. Requirements and invariants

Every MUST and MUST NOT that RFC 8 and the journal RFCs place on this epic, numbered so that
subtasks and tests can cite them.

**The verifier**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | The verifier is derived from the primary's node and node epoch, the process instance ID drawn at random when the process starts, the shard's incarnation and the loss generation of the journal holding the file; never from the clock alone. A device lost whole raises no loss generation, so the sync function answers `ErrUnavailable` on the journal it halted (D11), and no `Commit` answers a verifier for it until a new attach raises the incarnation | MUST, MUST NOT | RFC 8 §4.1 | T4.1, T4.5 |
| R2 | `Write` samples the inputs before it stages; `Commit` after its sync and every wait it makes | MUST | §4.1 | T4.1, T4.2 |
| R3 | Every call between two changes of the inputs returns one verifier | MUST | §4.1 | T4.1 |
| R4 | The loss generation is monotonic for the life of the process: a journal closed and opened again in the process continues from the value it had, so no verifier repeats one already given. The journal guarantees it (`Stats().LossGeneration`); the engine only reads it, and the conformance row checks it | invariant | §4.1, RFC 1 §3.8 | the journal; T4.1's row |
| R5 | The verifier changes exactly when an acknowledged unstable write may have been lost, and on nothing else: a loss of content the journal reports as offloaded changes nothing | invariant | E27, §4.1 | T4.1, T4.5 |
| R6 | §4.1 verifier, verifier inputs (adapted: the journal reopened in-process; a shard handed away and back and an offloaded loss are the second part's) and verifier after a loss | test row | §15.1 | T4.1, T4.6 |

**The stability point**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R7 | `Commit` follows RFC 8 §5.1 steps 1 to 4 | MUST | §5.1 | T4.2 |
| R8 | The engine keeps no committed size or holes: `Commit` asks `Existence.Overwrites`, one read per call | invariant | §5.1, RFC 6 §10.1 | T4.2 |
| R9 | What each of `Commit`'s waits covers: RFC 8 §5.1 steps 2 and 3 | invariant | §5.1 | T4.2 |
| R10 | A stability point joins the group commit and never issues its own: it asks the journal's group commit to run now and waits for the batch that covers its file, or the last of them for a file split over several; points that arrive together share transactions | invariant | §5.1 | T4.2, T4.3 |
| R11 | Under a store that commits nothing, `Commit` answers as RFC 8 §11.2's one rule for a stalled store gives | MUST | §5.1, §11.2, E31 | T4.2 |
| R12 | `Commit` answers as RFC 8 §5.1 step 4 | MUST | §5.1 step 4 | T4.2 |
| R13 | §5.1 stable write, lazy existence; overwrite waits for existence (adapted: an overwrite of journal-held committed content); stalled store; marks wait, §11.2 stalled store | test row | §15.1, §11.2 | T4.2 |

**The group commit**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R14 | A file's pending existence is committed within `existence_age` of the sync that made it recoverable, and before a removal's first transaction (the removal's half is epic 5's T5.1, whose phase 1 takes it through T4.7's claim) | MUST | §5.1 | T4.3 |
| R15 | Pending existence of one journal due together is committed together, one transaction per bounded batch, never one per write or per file | MUST | §5.2 | T4.3 |
| R16 | A batch holds at most `G` files and, through `kvstore`, at most `K` keys written; a file needing more than `K` commits over several transactions in version order, and a stability point on it waits for the last | MUST | §5.2, E40 | T4.3 |
| R17 | Every existence commit syncs the file in the journal first | MUST | §5.1, E19 | T4.3 |
| R18 | The commit writes `size`, holes, `mtime`, `ctime`, `applied` and the file's version from the operations it applies: `ctime` from each operation's change time, `mtime` only from an operation that set it, so the File record agrees with what `Overlay` answered before the commit; until the commit returns, the operations stay in the entry, so `Overlay` and `Read` keep answering them | invariant | §5.1 step 4, RFC 7 §9.4, E15 | T4.3 |
| R19 | A file's existence commits are serialised, a removal's phase 1 and a clone's among them: none starts on a file while a batch that captured it has not returned, so no commit at a higher version lands first and leaves the captured operations at or below `applied`, skipped | invariant | §5.1 step 4, RFC 6 §3.4 | T4.3 |
| R20 | An entry dropped once its operations commit never orphans a write: a write that found the entry before the drop lands in a live entry, and the next commit commits it | invariant | §5.1, E15 | T4.3 |
| R21 | §5.1 sync before existence, the write and `Commit` half: every committed `size` is backed by synced records | test row | §15.1 | T4.3 |
| R21a | A commit is reported only once durable: no `Commit` that waits on an existence commit answers, and no `Settle` covers a version, before that commit has returned; what else a `Commit` waits for is RFC 8 §5.1 steps 1 to 4 | MUST | §5.1 | T4.2, T4.3 |
| R21b | A stability point never waits for an offload. Nothing in this part offloads, so it holds by construction; the second part's offload commit carries its test | MUST NOT | §5.1 | none here |

**The split and conflicts**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R22 | A conflict or any other per-file failure never fails or delays a file that did not fail: the files `Existence.Commit` names in its map, which committed every other file in the same call, are retried alone; a whole-call failure, such as a conflict the store names no key for, halves the batch until each failing file stands alone | MUST NOT | §5.2, E24 | T4.4 |
| R23 | A file that still fails alone is backed off by itself and raises `existence_commit_failing` naming it; its stability points answer `ErrDelay` meanwhile, and no other file waits on it | MUST | §5.2, §11.1 | T4.4 |
| R24 | A metadata conflict is retried under the caller's deadline and never reaches a client as an I/O error; one unresolved at the deadline answers `ErrDelay` with `DelayConflict` | MUST, MUST NOT | §11.3 | T4.4 |
| R25 | A failed existence commit is retried by the next group commit, and fails a reply only through R11's rule | invariant | §5.1 | T4.4 |
| R26 | §5.2 group commit: 64 writers flushing while `chmod` hits random files of the journal; one file's File record undecodable, and another's removal record (an adaptation: §15.2's row names the File record only; the existence fence is checked for presence, never decoded); §5.2 sync marks ride in one batch | test row | §15.2 | T4.4 |

**Losses**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R27 | Each file has a loss sequence, in memory and monotonic for the life of the process, raised each time a loss event drops an acknowledged write of the file not yet offloaded | MUST | §5.1 | T4.5 |
| R28 | `Commit` returns the loss sequence read after its sync; `LossSeq` returns it without one | MUST | §5.1, §12.1 | T4.2, T4.5 |
| R29 | Every sync the engine makes, and `LossSeq`, drains the loss ring up to the journal's loss generation when it is above the last one applied, before any existence commit that follows the sync; where the ring reports events missed, the sequence of every file of that journal is raised | MUST | §5.1 | T4.5 |
| R30 | A journal device lost whole under the running process raises every attached file's sequence | MUST | §5.1 | T4.5 |
| R31 | The sequence is a journal-wide floor plus per-file entries only for files whose own loss raised them above it; a file's sequence is the larger (RFC 8 §5.1's `ponytail:` marker) | bound | §5.1 | T4.5 |
| R32 | A synced append or hole fill a loss event drops before its existence commit is committed from its loss event, uncarved with nothing held, so it reads **Lost**, never zeros or older content, and the commit charges nothing | MUST | §5.1, "Why an overwrite waits" | T4.5 |
| R33 | A failed sync window is reported once per file: to the next `Sync` of each file that had a write in the window, one waiting or the first to come later, through every sync the engine makes; a later `Sync` of the file does not wait on the failed window | MUST | §5.1, E35 | T4.6 |
| R34 | A `Commit` that meets a failed window answers the verifier sampled after it and the raised loss sequence, never an I/O error; the file's next `Commit` answers the same verifier as the writes after it | invariant | §5.1, RFC 17 V25 | T4.6 |
| R35 | §5.1 failed window | test row | §15.1 | T4.6 |

**`Done`**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R36 | `Done` is called exactly once for every write `Write` accepted: by the existence commit that records its usage, after it commits; by the engine, charging nothing, for a write a failed window drops and for every uncommitted write of a journal whose device is lost whole; by the commit from its loss event | MUST | §5.1, §12.1 | T4.3, T4.5, T4.6, T4.7 |
| R37 | The paths are exclusive: a write leaves the set of accepted, uncommitted writes under one lock, and the path that removes it calls `Done`; the commit claims the write before its transaction, calls `Done` once it commits and releases the claim when it fails and is retried | invariant | §5.1 | T4.7 |
| R38 | A write a failed window drops leaves the batch that claimed it: the batch neither commits its size nor calls its `Done`. A corrupt loss drained before the batch builds its operations is committed from that batch as `OpLost` (R32); one reported after it built them is committed as written (D13) | invariant | §5.1 | T4.6, T4.7 |
| R39 | After a restart nothing is reserved and nothing waits on a `Done`: recovery calls none. The reservations go with the process, so nothing in this epic can break it; epic 6's recovery (T6.5) carries its test | invariant | §5.1 | T4.7 |

## 4. Decisions this epic takes

- **D1, where the verifier's inputs come from.** The node ID and node epoch and the shard's
  incarnation are the binding's, which epic 6's `Bind` fills from the start transaction that
  raises the incarnations (RFC 8 §2.5). Until then nothing composes them: a test hook,
  `SetPrimary`, sets them, and they are zero in E2's fixture (§9, point 6). The process instance ID is drawn in `New` from `crypto/rand`. The loss
  generation is the journal's `Stats().LossGeneration`. The verifier is the first 8 bytes of
  a SHA-256 over the five inputs in a fixed order, so two inputs never cancel. A `decision:`
  marker at the derivation names the order and the hash.
- **D2, the generation's read.** `Write` and `Commit` each read `Stats()` once. A
  `ponytail:` marker: a write pays one journal `Stats()` call; add a journal call returning
  the generation alone if a profile shows it on the write path.
- **D3, the version a sync reached.** The journal's `Sync` returns no version. `Commit` takes
  the highest pending version in the file's entry, under its lock, just before it calls
  `Sync`: every operation at or below it was staged before the sync and is covered by it,
  and one staged during the sync is the next stability point's. A `decision:` marker names
  it, withdrawn if RFC 1's `Sync` returns the version it reached.
- **D4, when a file is due.** The engine cannot see the journal's timer sync, so a file is
  due once its oldest uncommitted operation was staged `existence_age` ago by the clock,
  earlier than the RFC's bound, never later, or at once when a stability point, recovery or
  a clone asks. A removal needs no trigger: its phase 1 commits the file's pending existence
  itself (epic 5). One timer per journal fires at the earliest due time. A `decision:`
  marker at the due rule names it.
- **D5, one batch in flight per journal, and the in-batch mark.** The group commit of a
  journal runs one batch at a time on a goroutine the engine owns, taking at most `G` due
  files in due order. The group commit of a journal starts when its first binding is made and
  is joined by `closeGroupCommits`, which epic 6's `Close` calls (§9, point 6). The capture,
  under each file's entry lock, marks the file in-batch and claims its operations in place,
  as a count (the claim's marks are T4.6's): they stay in the entry, so `Overlay` and `Read` keep answering them
  (R18). `Sync` is the share's handle's, so the batch makes one sync per share among its
  files, through the sync function (D9); only then, under each entry's lock again, does it
  build the store's `PendingExistence` with D6's times from the claimed operations, so a
  loss the drain marked after the capture is left out. It commits them in one
  `Existence.Commit`, which splits by `K` itself and returns only once every
  file's transactions are done. Each existence commit runs under its own bound,
  `conflictBound`, 1 s, because `kv.Update` retries a conflict until its context ends; a
  whole-call error splits the batch (T4.4) unless it is store-wide: the bound expiring with
  no conflict (a stalled store) or the store closed. A file's removal records (its masks) or the
  hole and overwrite records a commit loads that do not decode reach the engine as a whole-call error, so the split is what isolates them
  (RFC 8 §5.2's "any other per-file failure"). On return the committed operations leave their entries, the
  in-batch mark is cleared and its waiters woken. A batch wakes the waiters on every file it
  took from the due set, whether it captured the file or not, so a `commitNow` on a file it
  could not capture looks again rather than waiting for a batch that never comes. A file
  whose entry starts with an operation the capture cannot take (a removal's, under a guard)
  goes back on the due set one `existence_age` later. `waitBatch(fe)`, which epic 5's phase 1
  and epic 8's clone call after taking the guard, waits the mark out, so no commit of the
  file starts while a batch holds it (R19). A `ponytail:` marker: one batch per journal at a
  time caps existence throughput at one transaction chain per journal; pipeline batches when
  B5 shows stability points queueing behind one.
- **D6, the split times and the loss op.** `kvstore.PendingOp` gains `Mtime` and `Ctime` in
  place of `Time`; `Existence.Commit` sets `Mtime` from the newest operation with a non-zero
  one and `Ctime` from the newest operation. The batch fills them, after its sync, from each
  claimed operation's time and its `setsMtime` (epic 3's D10). `kvstore`'s `Remove` and `Clone`, which take the
  same type, read `Ctime` where they read `Time`. A new kind, `kvstore.OpLost`, commits an
  extent uncarved, holding nothing, and changes neither `Charged` nor the usage counters,
  for R32.
- **D7, the failing file's backoff.** A file failing alone goes back on the due set at once,
  held only by a doubling backoff from 100 ms to 30 s before a batch may take it again. A `ponytail:` marker: a fixed doubling
  schedule rather than the shared scheduler's `offload.retry_min`/`retry_max`; move it there
  when offload brings the scheduler. Its stability points answer `ErrDelay` with
  `DelayConflict` when the last failure was a conflict, else `DelayCommit`.
- **D8, the loss ring's drain.** The drain compares the journal's generation with the last
  one applied and, when it moved, reads `Stats().Losses`; a gap in `SeqNum` after the last
  event seen raises the journal's floor. Events marked `Offloaded` raise nothing, and so do
  `LossStale` events, which drop content older than offloaded content. A
  `LossFailedWindow` event raises the file's sequence (R27); dropping the matching pending
  write, its `Done` charging nothing (R38), is T4.6's. A `LossCorrupt` event of a pending
  write commits it from the event as `OpLost` (R32). A `decision:` marker names the cases, since `journal`'s `LossEvent` carries no synced flag
  (§9, point 2).
- **D9, the one sync function.** Every `Sync` the engine makes goes through one function:
  the group commit's, `Commit`'s, epic 5's phase 1, epic 6's recovery, epic 7's capacity
  sync and epic 8's clone. After the sync returns, and before the caller commits any
  existence, it runs the drain (D8). On a `SyncError` it syncs again until one succeeds,
  since RFC 1 §5.3 promises nothing durable past a failed window, and raises each named
  file's sequence once (T4.6): a failed window arrives both as the `SyncError` and as a
  `LossFailedWindow` event, so the files a `SyncError` names are noted in the journal's
  `losses.windows`, and the event of a noted file is consumed there rather than raising it
  again. A file a `SyncError` names with no event (the ring overflowed) keeps its pending
  writes, which the next group commit commits as written; a `decision:` marker at the
  function names it (§9, point 5). The two split a failed window's write: the drain (T4.5)
  raises the file's sequence, and the write stays pending, committed as written by the next
  group commit, until T4.6 drops it from the entry and from any batch that claimed it. The loss generation the journal raised changes the
  verifier of every later `Commit`, so the report needs no other state. A `decision:` marker
  at the function says every engine sync must go through it.
- **D10, the per-file commit.** `commitNow(ctx, b, f)` notes the highest version pending for
  `f`, marks `f` due now, wakes its journal's group commit and waits until a batch that
  **covers** that version returns: one whose capture of `f` took every operation at or below
  it. A batch already in flight that captured `f` at a lower version does not cover it, so
  `commitNow` waits for the next. `commitThrough(ctx, b, f, target)` does the same for a
  version the caller names, which `Commit` takes under the entry's lock before its sync
  (D3), so it never waits for operations staged after that. `commitAll(ctx, j)` does the
  same for every file of a journal, for epic 6's `Close`. `Commit` (T4.2) calls
  `commitThrough`; epic 6's recovery and epic 8's clone call `commitNow`. No marker: it is the stability point's own join (R10).
- **D11, a device lost whole.** `lostWhole(j)` raises the journal's floor and halts the
  journal: the sync function answers `ErrUnavailable` on it, so neither the group commit
  nor a `Commit` answers past the loss, and the verifier the client sees next comes after
  a new attach raised the incarnation (R5, RFC 8 §4.1's five inputs unchanged); and, for every
  uncommitted write of every file on that journal, removes it from its entry and calls its
  `Done` charging nothing, under each entry's lock. A snapshot of the due set would miss a
  write staged meanwhile, so the journal is first marked halted, and `Write` refuses under
  the entry's lock a write to a halted journal, answered as D12 says. Nothing in `journal` reports the state yet, so `lostWhole` is called
  from a test hook (§9, point 3); a `decision:` marker says so.
- **D12, a write refused on a halted journal.** `Write` answers a write to a journal
  `lostWhole` halted with `ErrUnavailable` and calls no `Done`: the write was never
  accepted, so no reservation is held for it, and the filesystem service maps the error as
  it maps any other unavailable journal. A `decision:` marker at the refusal names it,
  withdrawn if the service needs a cause it can retry, which `ErrDelay` would give.
- **D13, a loss reported after the batch built its operations.** A corrupt loss the drain
  finds before the batch builds the store's operations commits the write as `OpLost` from
  that batch (R32). One reported after, between `pending()` and the commit, is committed as
  written: the journal no longer holds its bytes, and committed unoffloaded content with
  none behind it reads **Lost** only if the journal reports it. A `decision:` marker names
  the fix: a loss path for committed unoffloaded content, which a later drain turns into an
  `OpLost` over committed existence.

## 5. Target tree

```text
pkg/block/engine/
  commit.go         T4.1  the verifier's five inputs and the derivation (D1, D2)
                    T4.2  Commit
  groupcommit.go    T4.3  the group commit per journal: due set, batch, in-batch mark, commitNow, commitThrough, commitAll, closeGroupCommits, dropShare (D4, D5, D10)
                    T4.4  the split, the failing file's backoff, existence_commit_failing, the retrying-owner mark (D7)
                    T4.7  the claim and its release
  loss.go           T4.5  the sync function, the drain, the loss sequence and its floor, LossSeq, a device lost whole (D8, D9, D11, D12)
                    T4.6  the failed window's report in the sync function
  write.go          T4.1  the `sample` field set to D1's derivation in place of `zeroVerifier`
  unbuilt.go        T4.2, T4.5  Commit and LossSeq leave it
  export_test.go    T4.1–T4.7  the binding's inputs, the batch hook, the loss hook
  enginetest/
    reservations.go T4.7  the reservation counter, for this epic's tests and epics 5, 6 and 8
  conformance/
    rfc8_commit_test.go  T4.1–T4.4, T4.6  the rows of rule 6
pkg/metadata/kvstore/
  existence.go      T4.3  PendingOp's Mtime and Ctime (D6)
                    T4.5  OpLost (D6)
test/engine/known.yaml  T4.1–T4.4, T4.6  their entries
```

## 6. Subtasks

### T4.1 The verifier

**Subtask T4.1 estimate: 2 man-days.**

- **Track:** B. **Depends on:** S2; epic 3's T3.2, built, whose stand-in it replaces.
- **Covers:** R1–R6.
- **Deliverables:** in `commit.go`: the process instance drawn in `New`; the node, epoch and
  incarnation on the binding, which nothing composes before epic 6's `Bind` (§9, point 6):
  until then `export_test.go`'s `SetPrimary` hook sets them, and E2's fixture leaves them
  zero; the engine's `sample` field set to D1's derivation over the five inputs (the
  binding's three, the process instance and the journal's generation (D2)), in place of `zeroVerifier`.
- **Acceptance criteria:**
  - two writes in one process and binding answer equal verifiers, however far the clock
    moved between them; a raised incarnation, a new instance or a raised generation answers
    a different one;
  - the generation is read before `WriteAt`, so a loss between staging and the reply shows in
    the next call's verifier, not this one's.
- **Tests:** over the fake: two writes with the test clock advanced an hour between them
  answer equal verifiers (catches a clock input); each input raised alone changes the
  verifier (catches an input dropped from the hash); a loss injected by a fake-journal hook
  between `WriteAt` and the reply leaves that write's verifier and changes the next
  `Commit`'s (catches sampling after staging). A conformance row over the real journal: close
  and reopen the journal in-process after a loss, the generation continues and no verifier
  repeats (RFC 8 §15.1 verifier inputs, adapted). It checks the journal's guarantee (R4)
  through the verifier, and cannot run until epic 6's `Start` composes the engine in E2's
  fixture or the journal can inject a loss, so it stays red until then.

### T4.2 `Commit`

**Subtask T4.2 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T4.1, T4.3, T4.5.
- **Covers:** R7–R13, R28.
- **Deliverables:** in `commit.go`: `Commit(ctx, s, f, off, n)` as RFC 8 §5.1 steps 1–4: the
  version under the entry lock (D3), the sync through the sync function (D9) inside an
  `engine.sync` span, one `Existence.Overwrites` over the pending writes in the extent at or
  below that version, and when it answers true, `commitThrough` that version (D10) within the
  caller's context; the marks wait of step 3 over the marks the sync returned; then the
  verifier and the loss sequence sampled. `ErrDelay` at the deadline: `DelayCommit` from a stalled
  store, T4.4's cause from a failing file. `engine_stability_points_total`. RFC 8 §5.1's
  `decision:` on lazy appends and its `ponytail:` on the stable-overwrite cost at the
  `Overwrites` call.
- **Acceptance criteria:**
  - a `Commit` over an extent with no committed content makes one sync and no `Existence.Commit`;
  - a `Commit` over an overwrite of committed content returns only after the transaction that
    commits it, and a write staged after the sync is not waited for;
  - with the store stalled, a `Commit` over an extent with no committed content answers and
    an overwrite's, or a whole-file one over committed content plus appends, answers
    `ErrDelay` at its deadline.
- **Tests:** over the fake and the real `kvstore` with a transaction counter: a `Commit` over
  an extent with no committed content makes zero transactions (catches a commit before every reply); an overwrite's
  returns after one (catches an answer after the sync alone); an operation staged during
  `Commit`'s sync whose batch a hook keeps from being captured does not hold the `Commit`,
  which returns once its own version is committed (catches a wait on versions above D3's,
  through `commitNow` in place of `commitThrough`); with a stalling `Existence` wrapper
  standing in for E2's fault-injecting `kv.KV`, which does not exist, an overwrite's
  `Commit` answers `ErrDelay` at its deadline and one over an extent with no committed content
  succeeds (catches success under a stall, R11). Conformance rows over the real journal for §5.1 overwrite waits for
  existence (adapted), stalled store, and "§5.1 marks wait, §11.2 stalled store", the latter two through E2's `Fixture.Stall`, added here, red until the journal's `Sync` exists, and the
  engine assertions of `TestRFC8A_StableWriteLazyExistence`.

### T4.3 The group commit per journal

**Subtask T4.3 estimate: 5 man-days.**

- **Track:** B. **Depends on:** S2; epic 3's T3.1 and T3.2, built.
- **Covers:** R10, R14–R21, R36 (the commit's path).
- **Deliverables:** in `groupcommit.go`: per journal, started when its first binding is
  made, the due set of files (D4), its timer, one batch in flight (D5) on a goroutine joined
  by `closeGroupCommits`, which epic 6's `Close` calls; the capture under each entry's lock,
  which marks the file in-batch and claims its operations in place, as a count (the claim's
  first form; T4.7 makes it exclusive against the other paths); one sync per share among the
  batch's files through each share's handle, replaced by the sync function in T4.5; then,
  under each entry's lock, `PendingExistence` built from the claimed operations with D6's
  times, and `Existence.Commit` inside an `engine.existence_commit` span, the journal's
  sync marks passed as its `marks` argument on exactly one transaction of each group commit,
  or alone in a call with no pending existence, and the marks a committed transaction
  recorded handed to the journal's `MarksRecorded` (RFC 8 §5.2); on
  return, each committed file settled in the journal, `Settle(id, v)` at the highest version
  the commit applied for it, so a truncate up's removal marker (epic 5 D4) and a synced
  loss committed as `OpLost` are dropped at once rather than at the next start (RFC 1 §3.6,
  §3.10); its operations removed from its entry and their `Done`s called,
  the in-batch mark cleared and its waiters woken, the entry dropped when empty, marked dead
  under its own lock (epic 3's `unlock`); `engine_pending_existence_ops`; `waitBatch`,
  `commitNow`, `commitThrough` and `commitAll` (D5, D10); `dropShare(s)`, which removes a share's files from
  the due set and waits for the batch in flight, built here and called by epic 6's `Drop`,
  which tests it (T6.9). `kvstore`'s `PendingOp` split (D6), with its own test.
  `kvstore`'s marks write (RFC 6 §10.1): `Existence.Commit`'s `marks *MarksWrite`, its
  tracked `og` check, `ErrOpenGenMoved` and `ErrSyncMarks`, with its own test. E2's fake
  journal gains sync marks: `Sync` returns a mark per stream covering what it synced, and
  `MarksRecorded` logs what it is handed.
- **Acceptance criteria:**
  - a file with pending writes and no stability point is committed within `existence_age`,
    by the test clock;
  - 64 files due together commit in one `Existence.Commit` when `G` is at least 64, and in
    `⌈64 / G⌉` when it is less;
  - the `Sync` of a batch's files precedes its `Existence.Commit`;
  - a suspended write's commit leaves `Mtime` at the earlier write's and moves `Ctime`;
  - a file's operations are answered by `Overlay` while their batch is in flight;
  - a committed file is settled at the highest version committed for it.
- **Tests:** over the fake with a call log: `Sync` before `Existence.Commit` for every batch
  (catches a commit before the sync, R17); 64 due files and `G = 16` give four calls
  (catches an unbounded batch); the test clock advanced to `existence_age` commits a lone
  write (catches no timer); every committed file's `Settle` logged at the highest version the
  batch committed for it, and none for a file the store failed (catches no settle, or one
  past what committed). Over the real `kvstore`: a suspended write committed over an
  earlier write keeps the earlier `Mtime` and carries its own `Ctime` (catches one time for
  both, R18). With a batch held by a hook between its capture and its `Existence.Commit`:
  `Overlay` still answers the captured writes (catches operations moved out of the entry,
  R18); a `waitBatch` on the file returns only once the batch returns (catches a commit that
  overtakes the batch, R19); a `commitNow` for a write staged after the capture returns only
  after the next batch, not the held one (catches D10's "covers" taken as "any batch").
  Over the fake: 64 due files and `G = 16` carry the marks on exactly one of the four calls,
  a group commit with no batch due makes one call with the marks alone, and `MarksRecorded`
  is called with what each committed call recorded (catches marks on every transaction, or
  on none).
  Under `-race`, a hook between `lockFor`'s lookup and its lock in which the commit drops the
  entry: the write lands in a live entry, `Overlay` reflects it, the next batch commits it
  and its `Done` runs once (catches an orphaned write, R20). A conformance row over the real
  journal for §5.1 sync before existence, the write and `Commit` half, red until the
  journal's crash seam and `Sync` exist.

### T4.4 The split and the failing file

**Subtask T4.4 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T4.2, T4.3.
- **Covers:** R22–R26.
- **Deliverables:** in `groupcommit.go`: T4.3's sync marks kept through a split, and on
  `ErrOpenGenMoved` or `ErrSyncMarks` the batch and the marks handled, as RFC 8 §5.2 gives; the
  files `Existence.Commit`'s map names, the other
  files having committed in the same call, retried alone in the next batch; a whole-call
  error halving the batch until each failing file stands alone; a file failing alone backed
  off by D7's schedule; `existence_commit_failing` naming it, raised and cleared, logged at
  `Warn`; the cause its stability points answer (D7); a conflict retried within the
  caller's deadline and answered `DelayConflict` at it, the engine naming a conflict through
  `kvstore.ErrConflict`, since its import rule forbids `kv`; `existence_commit_failing`
  cleared too when a capture finds the file's entry gone, on `lostWhole` and on
  `dropShare`; `engine_group_commit_splits_total`; RFC 8 §15's `ponytail:` at the split
  table; the retrying-owner context mark, an unexported context value with its setter and
  reader, which this subtask builds unless epic 7's T7.2 landed first and built it, and under which every
  store call the group commit retries is made, so that epic 7's self-fence (T7.2, its D7),
  which reads the mark and reuses it, counts a whole-call failure as failing and a file's
  own failure not; epic 5's removal loop sets the same mark. In the engine's test package, `newUnitOver`, which composes the unit over a given
  `kv.KV`, for a wrapper that fails or conflicts on chosen keys; E2's fault-injecting
  `kv.KV` does not exist.
- **Acceptance criteria:**
  - a conflict on one file of 64 that the store names no key for delays the other 63 by at
    most `log₂ 64` extra calls; one the store names in its map delays none;
  - the failing file's `Commit` answers `ErrDelay` while every other file's succeeds;
  - the condition clears on the file's next commit that succeeds, when a capture finds its
    entry gone, on `lostWhole` and on `dropShare`.
- **Tests:** the split's table over a stub `Existence`: each failure shape (a file in the
  map, a whole-call error, two failing files, every file failing, each on a batch that
  carries the marks) against the expected calls
  (catches a committed file resubmitted, or a failing one resubmitted with the rest). Over
  the real `kvstore` over a `kv.KV` wrapper, through `newUnitOver`, making one file's File
  record undecodable: the other files' overwrite `Commit`s succeed, that file's answers `ErrDelay`,
  and the condition names it (catches a whole-batch failure, R22, R23). A conflict held past
  a `Commit`'s deadline answers `DelayConflict`, never `ErrUnavailable` (catches a conflict
  surfaced as an I/O error, R24); a conflict held past `conflictBound` splits the batch, and so
  does one file's removal record made undecodable over the real `kvstore` (the
  other files commit, that file fails alone: catches a split on conflicts only, RFC 8
  §5.2); a store stalled past the bound, or closed, does not split (catches 2G−1 bounded
  calls on a store-wide failure, or a commit left retrying until the caller's deadline); the condition cleared on `dropShare` and on `lostWhole`
  (catches a condition naming a file nothing will commit); a `kv.KV` wrapper through
  `newUnitOver` that records the mark on each call sees it on every group-commit call and on
  no `Existence.Overwrites` call of a `Commit` (catches a retried call left unmarked, which
  the self-fence would never count). The §5.2 group commit row, a conformance row over the real
  journal and `kvstore`: 64 writers flushing while `chmod` writes random File records, no
  flush waiting beyond the split bound, red until the journal's `Sync` exists. The §5.2 sync
  marks ride in one batch row, over the same, red until the journal's `Sync` returns marks.

### T4.5 The loss sequence

**Subtask T4.5 estimate: 4 man-days.**

- **Track:** B. **Depends on:** T4.3.
- **Covers:** R5, R27–R32, R36 (the loss event's and the lost device's paths).
- **Deliverables:** in `loss.go`: the sync function (D9), which the group commit then uses;
  per journal, the floor and the map of per-file entries (R31, with RFC 8 §5.1's `ponytail:`
  marker); the last generation applied and the last event seen; the drain (D8), run by the
  sync function and by `LossSeq`; `LossSeq`; the commit of a synced pending write from its
  loss event as `OpLost` through T4.3's batch, and its `Done` from that commit; `lostWhole`
  (D11), the halted journal's refusal in `Write` (D12), and its hook. The unmatched loss:
  `Write` takes its version from `WriteAt` before it inserts the operation under the entry's
  lock, so a drain in that gap finds no write to mark. `markLost`, finding no pending write
  for an event, keeps it per file in the journal's loss state, under the lock the insert
  takes, and the insert checks it: a write whose version it names is inserted marked lost,
  and its batch commits it as `OpLost` (R32); a kept event below the write's version is a
  committed write's (§9, point 8), discarded, since a file's writes arrive in version order
  (epic 3's D3). A `decision:` marker at the discard names that order, to be withdrawn with
  D3's own: two writes of one file in the gap at once would have the later discard the
  earlier one's event, committed as data. A `ponytail:` marker: a kept event of a file never written again stays
  until the process ends, one per such loss; prune it with point 8's store-side loss path.
  `kvstore`'s `OpLost` (D6),
  with its own test. E2's fake journal gains a `Report` method that injects a loss event,
  an offloaded one included, and a gap in the ring, which the tests below need.
- **Acceptance criteria:**
  - a corrupt record of an acknowledged, unoffloaded write raises its file's sequence once;
    an `Offloaded` event raises nothing;
  - a gap in the ring raises every file of the journal, by the floor;
  - a group commit whose sync follows a loss event drains it before it commits, so it never
    commits the dropped write as data;
  - a synced append dropped before its existence commit reads **Lost** once committed, its
    `Done` runs once, and the file's `Charged` does not move;
  - a corrupt loss drained between a write's `WriteAt` and its insert is committed as
    `OpLost`, never as the write.
- **Tests:** over the fake's loss ring, through `Report`: one event raises one file (catches
  a journal-wide raise for every loss); an event marked `Offloaded`, or a `LossStale` one,
  raises nothing (catches R5's "nothing else"); a gap raises a file with no event (catches a drain that trusts the ring);
  `LossSeq` after a loss without a `Commit` is raised (catches a drain only in `Commit`); a
  corrupt loss reported between a batch's capture and its sync is committed as `OpLost`, not
  as the write (catches a store operation built at capture, before the drain, R29). A hook
  between `Write`'s `WriteAt` and its insert reports a corrupt loss of that write and runs
  `LossSeq`: the next batch commits the write as `OpLost`, a `Read` answers `ErrLost` and its
  `Done` runs once (catches an event the drain advances past unmarked, committed as data,
  R32). Over the real `kvstore`: the loss event's commit
  leaves the extent uncarved, a `Read` answers `ErrLost`, and `Charged` and the usage
  counters are unchanged (catches a hole committed instead, or an `OpWrite` that charges,
  R32). `lostWhole` under `-race` with writers: every uncommitted write's `Done` once, every
  file's sequence raised, and a write racing it refused rather than left with no `Done`;
  after it, a `Commit` on the halted journal answers `ErrUnavailable`, never a verifier
  (catches a verifier answered past the loss, R5).

### T4.6 The failed window, per file

**Subtask T4.6 estimate: 3 man-days.**

- **Track:** B. **Depends on:** T4.2, T4.3, T4.5.
- **Covers:** R6 (verifier after a loss), R33–R35, R36 (the failed window's path).
- **Deliverables:** in `loss.go`: the sync function's `SyncError` case (D9): the sync
  repeated until one succeeds, each named file's sequence raised once through
  `losses.windows`, used by every engine sync, epic 7's capacity sync included; in
  `groupcommit.go`: the claim's and the drop's marks on an operation, made under the entry's
  lock, so a `LossFailedWindow` event drops the matching pending write from its entry and
  from any batch that claimed it, its `Done` charging nothing (R38); a `LossFailedWindow`
  event that finds no matching pending write, its write between `WriteAt` and the insert,
  kept as T4.5 keeps an unmatched corrupt loss, and the insert dropping the write it names
  instead of inserting it, its `Done` charging nothing, so it is never committed as written
  (R32, R38); `Commit`
  answering it with the new verifier and sequence, never mapped to `ErrUnavailable` (epic
  3's D4 left an unnamed journal error so).
- **Acceptance criteria:**
  - a window failed by the group commit's sync, with no `Commit` waiting, makes the file's
    next `Commit` answer a verifier other than its writes' and a raised sequence, and the one
    after answer the writes' verifier;
  - another file's `Commit` in the same journal is unaffected beyond the new generation;
  - a write whose window fails between its `WriteAt` and its insert is never committed.
- **Tests:** over the fake, a `SyncError` returned to the group commit's sync names file A:
  A's next `Commit` answers the new verifier and a raised sequence, A's following one after a
  new write matches that write's (catches a report lost when no `Commit` waited, and a file
  wedged); a `SyncError` on `Commit`'s own sync answers success with the new verifier, not an
  error (catches D4's mapping); a window reported as both a `SyncError` and an event raises
  the sequence once (catches a double raise); a `SyncError` followed by a successful sync
  answers only after the second (catches a sync that returns at the failure); a batch
  claims a write, then its window fails before the batch commits: the committed size
  excludes it (catches a batch that keeps the dropped write); a hook between `Write`'s
  `WriteAt` and its insert reports a `LossFailedWindow` event of that write and runs
  `LossSeq`: the committed size excludes it and its `Done` runs once, charging nothing
  (catches an unmatched failed window whose write is committed as written, R38). E2's fake journal gains
  `FailSync(also)` and `Durable`, and its fixture `FailNextSync`, for these tests and the
  conformance row over the real journal for §5.1 failed window and §4.1 verifier after a
  loss, red until the journal fails windows.

### T4.7 `Done` exactly once

**Subtask T4.7 estimate: 2 man-days.**

- **Track:** B. **Depends on:** T4.6.
- **Covers:** R36–R39.
- **Deliverables:** the claim's release on a failed commit, over T4.6's marks;
  `enginetest/reservations.go`: the reservation counter, a `Done` wrapper incremented by
  each accepted `Write` and decremented by its `Done`, failing the test on a second call,
  which epics 5, 6 and 8 reuse.
- **Acceptance criteria:**
  - after failed windows, losses and a device lost whole, under `-race`, the counter is zero
    and no `Done` ran twice;
  - a write dropped from a captured batch is not in the size the batch commits.
- **Tests:** over the fake with the counter: failed windows, corrupt losses and `lostWhole`
  interleaved with writes and batches return it to zero (catches a missed `Done`); a batch
  whose commit fails then succeeds calls each `Done` once (catches a `Done` before the
  commit or none after the retry); a batch captures a write, then its window fails before
  the batch commits, and the batch retries: the committed size excludes the write and its
  `Done` ran once, from the drop (catches a batch that keeps the write and calls `Done` once,
  which the counter alone passes). R39 has no test here: nothing in this epic can reserve
  across a restart, and epic 6's recovery test checks that it calls no `Done`.

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 red suite in CI"]):::sync --> E3(["epic 3: T3.1 entries, T3.2 Write, built"]):::sync
  E3 --> T4_1["T4.1 Verifier"]
  E3 --> T4_3["T4.3 Group commit"]
  T4_3 --> C1(["after T4.3: per-file commit, epics 5, 6 and 8 can commit existence"]):::sync
  T4_3 --> T4_5["T4.5 Loss sequence"]
  T4_1 & T4_3 & T4_5 --> T4_2["T4.2 Commit"]
  T4_2 & T4_3 --> T4_4["T4.4 Split"]
  T4_2 & T4_5 --> T4_6["T4.6 Failed window"]
  T4_6 --> T4_7["T4.7 Done exactly once"]
  T4_4 & T4_6 & T4_7 --> C2(["after T4.7: every accepted write gets one Done"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T4.3 is the riskiest, for the claim against the entry's drop and the in-batch mark under
`-race`, and starts beside T4.1. Checkpoint after T4.3: epic 5's phase 1, epic 6's recovery
and epic 8's clone can commit a file's existence and wait out its batch. Checkpoint after
T4.7: `Done` runs exactly once on this epic's four paths.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T4.1, T4.3 | 7 | — | the verifier and the group commit, with the per-file commit the other epics call; T4.1 needs nothing of T4.3 and can open alone, as a 2 man-day pull request, leaving T4.3 as a 5 man-day one |
| 2 | T4.5, T4.2 | 7 | PR 1 | the sync function and its drain, and the stability point that syncs through it |
| 3 | T4.4, T4.6, T4.7 | 8 | PR 2 | the split, the failed window and `Done` |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

1. **No content call commits a file's existence on demand.** RFC 17 §5.5's owner change and
   RFC 7 §9.5's explicit time set first commit the file's pending existence, "joining the
   group commit", but RFC 8 §12.1 offers only `Commit`, which waits only for overwrites.
   This epic builds the join (`commitNow`, D10) for its own and other epics' use; RFC 8 §12.1
   should say which call the service makes: `Commit` committing all pending existence when
   asked, or a method of its own.
2. **`journal`'s `LossEvent` has no synced flag.** RFC 1 §3.8's loss record names whether
   the write was synced, which decides whether it is committed from the record or dropped.
   D8 decides by the reason instead; RFC 1 or `journal` should carry the flag.
3. **A device lost whole under the running process has no signal.** RFC 8 §5.1 raises every
   attached file's sequence at the loss, but `journal` reports no such state, and RFC 1
   §5.4's read-only state is not built either. T4.5 builds the engine's half (D11) behind a
   test hook; the trigger waits on the journal.
4. **The version a sync reached** (D3): RFC 1's `Sync` could return it.
5. **A missed ring and the pending writes it covered.** A gap in the loss ring raises the
   floor (R29), but the engine cannot tell which pending writes the missed events dropped,
   so a group commit can commit existence over a record a missed event dropped, reading
   **Lost** where RFC 8 says a dropped unsynced write reads as before. The ring's bound
   should make a miss rare enough, or the journal should refuse a `Sync` until the engine
   has caught up.
6. **Owed by epic 6.** Three things this epic leaves to epic 6, which its document should
   name:
   - `Bind` fills the binding's node ID, node epoch and the shard's incarnation, T4.1's
     inputs, from the start transaction that raises the incarnations (D1);
   - a start past an acknowledged device loss starts each attached file's loss sequence at
     1, by setting T4.5's floor of that journal to 1 before any file is served;
   - `Close` commits every file's pending existence through `commitAll` (D10), then joins
     the group commits with `closeGroupCommits` before it closes the journals, and calls no
     `Done` for what it committed twice.
7. **RFC 8 §5.1 step 4 names no `Settle`.** T4.3 settles each committed file at the highest
   version it committed, as RFC 1 §3.6 and §3.10 need for a truncate up's marker and a
   synced loss's `OpLost`. Settled: RFC 8 §5.1 says so since #3716.
8. **A synced loss of committed, unoffloaded content reaches no metadata.** A corrupt loss
   reported once its write is committed as written (D13, or a loss a ring gap hides, point 5)
   lands at or below the file's `applied`, where `kvstore` skips an `OpLost`. The extent still
   reads **Lost**, after a crash too: it is committed uncarved, with no chunk or a stale one, and
   once `Settle` and repack (RFC 1 §7.2) drop the journal's record of it, RFC 0 §4.2's table
   classes it **Lost** and the read fails, never serving the stale chunk. What metadata never
   records is the loss on the File, and the extent's charge is never released. The fix is a
   store-side loss path that records the loss against the File and uncharges the extent at or
   below `applied`, driven from T4.5's discard of a kept event below a write's version, never
   from `markLost` itself, since an event it keeps may still name a write between `WriteAt` and
   its insert, which the batch commits as `OpLost`; and for a gap from the journal's
   `Since(id, 0)` loss records. It needs a `kvstore` call and an RFC 6 line, and is epic 4's,
   under #3734. `Settle` needs no change: it never passes a write metadata does not hold. Whether
   a **Lost** extent counts in `Charged` at all is undecided by the RFCs: RFC 6 §2.4 defines
   `Charged` as "the file's logical bytes excluding holes", while RFC 8 §5.1 has a write
   committed from its loss record charge nothing. #3734 decides the uncharge.
