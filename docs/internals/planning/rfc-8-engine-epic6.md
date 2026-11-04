# Epic 6: engine v2 start, bindings and recovery

**Sources:** [RFC 8](../rfcs/rfc-8-engine.md) §1.1 (the share table's `decision:`), §2.1
(the `Lifecycle` row), §2.3 (bindings, `Bind`, `Unbind`, `Drop`), §2.4 (a binding's
settings), §2.5 (start steps 1 to 5 and 8, the retried open of a journal refused as
unsuitable, `Bind`'s attach, stop steps 1 to 5), §2.7, §12.1 (`Start`, `Close`, `Bind`,
`Unbind`, `Drop`, and a content call naming an unbound or removed share),
§13 (E13, E15, E19, E29, E40) and §15's rows for those sections; [RFC 16](../rfcs/rfc-16-metadata-store.md) §3
(`Lifecycle`), §2.3.2 (the removal's steps 3 and 4) and §4.2 (`J‖journal`, `S‖id‖vl`,
`S‖id‖rr`); [RFC 1](../rfcs/rfc-1-journal.md) §3 (`Open`, `Share`, `Forget`, `Strand`,
`Stats`), §3.7, §3.10 and §4.3, for the journal calls; [RFC 28](../rfcs/rfc-28-journal-format.md)
§3.1 and §3.4, for the floor and `expect`; [RFC 14](../rfcs/rfc-14-open-state.md) §4.4, for
what a raised incarnation does to the grace instance.
**Track:** C.
**Epic 6 estimate: 10 subtasks, 35.75 man-days.**
**Depends on:** sync point **S2, red suite in CI**: E1's surface (`Engine`, `Deps`,
`Journal`, `Handle`, `Lifecycle`, `Share`, `ShareState`, `Stats`) and E2's fixture and fake
journal. T6.1 also needs the RFC 8 §2.1 and RFC 16 §3 text that settles #3631 and #3632, and
the source of a share's journal (§9 point 14), since it changes E1's frozen `Lifecycle`.
T6.2 extends the share table epic 3's T3.1 declares, already on epic 3's branch. T6.4 needs
E2's fault-injecting `kv.KV` (T2.3), which is not built yet, to fail the start transaction.
T6.7 needs epic 3's `Write` and `Draw` (T3.2, T3.3), where a version is issued, and the same
`kv.KV` to block the raise. T6.5 needs epic 4's per-file existence commit (T4.3) and epic
5's abort routine (T5.4); T6.6 and T6.8 epic 5's removal loop (T5.3); T6.9 epic 4's due set
(T4.3) and reservation counter (T4.7), and T6.8. T6.10 needs T6.9 and `journalv2` to declare
`ErrUnsuitableStorage` (RFC 1 §3). #3608 and #3609 put the share on every content call and
give a pending release its retry channel. Epic 8's T8.6 builds a rebuilt clone's `Served`
wait under a context that `Close` cancels and `Unbind` waits out (its D11).
**Blocks:** epic 7, whose refusal table answers the version-limit row with T6.7's raise
and whose self-fence counts the commits of a bound share; epic 8, whose clone is rebuilt
from T6.5's clone markers and T6.6's unfinished removals and joined by T6.8 and T6.9;
epic 3's cross-share row (its R5), which needs `Bind` for a second share; the
integration epic, which cannot run the filesystem over an engine that does not start.

---

## 1. Goal

Epic 6 gives the engine its life. `Start` opens each device's journal with the floor and
identity the store records, refuses by name the shares of a journal that is missing or
foreign, raises every shard's incarnation in one transaction, re-applies from the journal
every existence a crash left uncommitted before any file is served, resumes the unfinished
removals and strands the tags no share claims. `Bind` attaches a share above its floor;
`Unbind` and `Drop` end a removed share's work and its tag. The version limit is written
before any version is issued and raised before it is reached. `Close` stops in reverse and
never closes a component under a live loop.

**At the end of the epic:**

- after a crash, every write that was acknowledged and synced reads back, with the size and
  `mtime` it had, and every removal the crash cut off is applied, before the file is served;
- no version a client saw is issued again after a crash, and a share attached later never
  gets a version below one its files already hold;
- a journal found missing or foreign refuses its shares by name and is never opened as new;
- the share table is read on the request path without a lock;
- a removed share's tag is forgotten and nothing of it runs against a closed handle;
- `Close` leaves no goroutine and closes nothing a running loop still uses.

### 1.1 In scope

The store's `Lifecycle` view in `kvstore`; the copy-on-write share table and the bindings'
states; `Bind` (attach above the floor, a state change); `Start`'s steps 1 to 5 and 8 over
the journal and the store alone; the retried open of a journal refused as unsuitable and its
late attach; the version limit at start, at attach and while serving; `Close`'s steps 1 to 5
with the removal loop standing for the work scheduler; `Unbind` and `Drop`; the conformance
rows of RFC 8 §15 for §2.3, §2.5 and §2.7 that this part can pass.

### 1.2 Out of scope

- Start step 6 (the work queue) and step 7 (the scheduler's dispatch, the age tick, the
  rescan, eviction and the reseed), stop step 2's scheduler and step 3's offload loops, and
  the re-offer after a restart: RFC 8's second part, with offload. Here stop step 2 stops
  epic 5's removal loop and epic 8's clone batches.
- The syncer flow a binding opens at `Bind` and closes at `Drop` and `Close`, the key scope,
  the budgets beyond the journal's per-share limit, and `Bind`'s backend change: the second
  part.
- `keepHolds` (a removal that keeps snapshots offloads what its hold marks cover): RFC 12,
  out. `Unbind` refuses it.
- The asynchronous copy `Unbind` cancels (RFC 14 §2.7): RFC 14's, not built in this part.
- The operator's acknowledgement of a lost journal device, which re-initialises it: the
  second part. Here the shares stay refused, and a test re-initialises the directory as the
  operator's step would.
- The salvage export of a stranded tag and its `Forget` while serving (`salvage.path`): the
  second part. Here a stranded tag stays stranded until the next start.
- A read-only journal reopened in place (RFC 8 §2.5, E42): it needs RFC 1 §5.4's read-only
  state, spares and probe, which `journalv2` does not have. No epic of this part owns it
  (§9 point 8).
- `Bind` with `State: Closing` (a re-home's finish, RFC 27) and `Fenced` (RFC 8 §11.2's
  copied-image row): out, and epic 7 for the self-fence, which is not a share state.
- The chunking settings `Apply` records and reports as a migration (RFC 8 §2.4): the
  carver's, in the second part. `Apply` itself is E1's.
- The clone rebuild itself, from a marker or a `Removal` record: epic 8 (T8.6). This epic
  hands it the markers and records and serves no file before its turn.
- **(cluster)** paragraphs: a new primary settling before serving, a primary draining a
  shard it loses, per-(shard, tag) forget records, replicated journals.
- Imports (RFC 26's recovery import, RFC 27's move back): `Bind`'s strand of the journal's
  unclaimed tags before an imported share is served (RFC 8 §2.5, RFC 1 §3): out, with RFC 26
  and RFC 27. No share this part binds is imported.

### 1.3 Names used here

- **E1**, epic 1, interfaces: `pkg/block/enginev2`'s exported surface, `Deps` and its
  interfaces, frozen at S1.
- **E2**, epic 2, red e2e suite and test foundations: `engine-conformance` with its
  `known.yaml`, the fixture over the real journal, `kvstore` and the real open-state table,
  the fault-injecting `kv.KV` (its T2.3), the fake journal and the profiler wiring.
- **Epics 3, 4, 5, 7 and 8**: write, read and overlay (3); commit, group commit and losses
  (4); removals (5); capacity, self-fence and health (7); clone and copy (8). Their subtasks
  are cited as `T<epic>.<n>`.
- **S1**, interfaces frozen and `Content` agreed with the filesystem service. **S2**, red
  suite in CI. **S3**, engine complete over the fake journal: epics 3 to 8 done, every row
  whose journal call exists green. **S4**, the journal's calls built.
- **E13, E15, E19, E29, E40, E42**: RFC 8 §13's invariants of those numbers, not epics. E13: no
  background work outlives a component it uses; E15: existence is re-applied at recovery
  before a file is served; E19: every existence commit syncs the file in the journal first;
  E29: a journal serves a share it did not serve at open only by the attach rule; E40: no
  engine transaction exceeds the key budget `K`; E42: a read-only journal is reopened in
  place.
- **The integration epic**, epic 9: the filesystem service composed over the engine at
  RFC 17 §8's integration tier.
- **The removal loop**: epic 5's per-journal loop that runs a removal's phase 2 and prune,
  which stands for RFC 8 §6.1's work queue in this part.

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
   (`test/enginev2/known.yaml`) in the same change: `dt` fails the run if a listed test
   passes, so the change that turns it green removes it, a journal pull request included. A
   test over the real journal that starts red sits in that suite behind its build tag, never
   in `unit`, which keeps no known list.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on v1, enforced by
   the engine's one import test, a walk over every package of `pkg/block/enginev2` with
   one allowlist, which also refuses `pkg/vfs`: no package imports `pkg/block/engine`,
   `pkg/block/journal`, `pkg/block/syncer`, `pkg/block/carver`, `pkg/block/remote` or the
   v1 metadata service `pkg/metadata`. The walk checks every package on its own, so its
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
    `internal/` package of `enginev2`.
12. **Each subtask creates only what it declares.** No empty package and no stub without a
    caller. A package, an option or a test helper lands with its first consumer.

### 2.1 How the rules apply

| Rule | Subtasks | How |
| --- | --- | --- |
| 1 Unit tests | all | T6.2 to T6.10 test over E2's fake journal and the real `kvstore` on an in-memory `kv.KV`, through epic 3's unit composition until T6.2's `Bind` replaces its hook; T6.1 over `kvstoretest`'s suite. T6.3 extends the fake with `Open`'s refusals, `Forget`, `Strand` and `Stats`' tags and versions; T6.5 with `Files`, `Since` and `Settle`; T6.7 with `SetVersionLimit` and a counter that issues against it |
| 2 Fuzz | none | no parser. The `ponytail:` marker at the read-resolution table already says why |
| 3 Observability | T6.3, T6.5, T6.6, T6.7, T6.10 | no new counter and no new span: RFC 8 §14 names none for start, bindings or the version limit. Conditions through `Stats`: a share refused at start, named with its device (T6.3); `journal_lost` (T6.3, T6.10) and `journal_unsuitable` (T6.10), each per journal. Logs: a refused share and a stranded tag at `Warn`, with held and dirty bytes for the tag (T6.3, T6.6); a release marker settled as data loss at `Error`, naming the file (T6.5); a version-limit raise that fails at `Warn` (T6.7). |
| 4 Profiling | all | the unit composition's `prof.Watch` covers every test: a raise goroutine (T6.7), a retry loop (T6.10) or a loop left running past `Close` (T6.8) fails it |
| 5 Public and internal | all | internal tests of the share table (T6.2), the floor and limit arithmetic (T6.3, T6.7) and the trigger (T6.7); the rest through `*Engine`; `-race` in T6.2, T6.7, T6.8 and T6.9. What the engine hands the journal is asserted where the journal acts on it: the floor by the version the next `WriteAt` returns over the real journal (T6.2, T6.3), the limit by the version a `Draw` is refused at (T6.7); what it hands the store by reading the keys back through `kvstore` (T6.1, T6.4) |
| 6 Known failures | T6.2 to T6.10 | E2 delivered four of its rows; the rows this epic owns are written here, over E2's fixture, red in `known.yaml` naming this epic's issue until the journal calls they need exist, and removed in the change that turns them green |
| 7 v2 ignores v1 | all | nothing new is imported; `kvstore` is not v1 |
| 8 Real dependencies | T6.1, T6.3, T6.5, T6.7, T6.9, T6.10 | the identity, limits and incarnations from the real `kvstore`; the floor, the refusal of a missing or foreign journal, the recovery, the version limit, `Drop`'s `Forget` and the retried open over the real journal and its crash seam. Every §15 row runs over the fixture; the fake keeps the unit tests (the retry loop's, the content calls' answers after `Unbind`) |
| 9 Comments | all | epic 3's copy-on-write share table `decision:`, kept at the `bindings` type (T6.2); `kvstore`'s `decision:` at `RaiseIncarnations`, rewritten for `RaiseAtStart` (T6.1); the version-limit headroom `decision:` and burst `ponytail:` (T6.7), RFC 8 §2.5's `decision:` on an error a check meets (T6.3), and the markers of §4 |
| 10 Tests can fail | all | each test below names the change it catches |
| 11 Exports | T6.1, T6.3 | `Lifecycle` changes shape (D4) and gains `Tags` (D2), both pending agreement (§9 points 1 and 2); `Tags`' record type is declared in `entity`, beside the store's other records, so `kvstore` imports no engine, and `entity.Shard` gains the primary's journal identity (D10, §9 point 14); `kvstore` gains `RecordJournal` (D3, §9 point 5) and `Lifecycle()`; `Stats` gains the refused shares and the journal conditions (T6.3). Test hooks live in `export_test.go` |
| 12 Creates only what it declares | all | each fake journal call lands with the subtask that first calls it; `RecordJournal` with T6.1, whose fixture is its first caller; `kv.Counting`'s count of Updates and of the most keys one Update wrote with T6.1; the fixture's second share with T6.2 and its second device with T6.10 |

## 3. Requirements and invariants

Every MUST and MUST NOT that RFC 8 and the RFCs it cites place on this epic, numbered so that
subtasks and tests can cite them. Sections are RFC 8's unless named.

**The store's lifecycle view**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | The engine's store access outside block metadata's views and `Refs` is `Lifecycle` alone: `JournalID`, `VersionLimits`, `RaiseVersionLimits` and `RaiseIncarnations`, which D4 splits into `RaiseAtStart` and `RaiseAtAttach` and D2 extends with `Tags`, once the RFC text lands (§9 points 1 and 2) | invariant | §2.1, RFC 16 §3 | T6.1 |
| R2 | `JournalID` answers the identity the store records for a device; `J‖journal` is written when the journal is created, before anything in it is acknowledged | invariant | §2.5 step 1, RFC 16 §4.2, RFC 1 §3 | T6.1 |
| R3 | Every write of `S‖id‖vl` (at start, at an attach, at every raise) is a tracked max-write: it writes the larger of the value it reads and its own, never blindly, and never lowers the limit | invariant | §2.7, RFC 16 §4.2 | T6.1, T6.2, T6.3, T6.7 |
| R4 | The limits are raised in as many transactions as `K` takes, never one over every share | bound | §2.7, E40 | T6.1, T6.7 |
| R5 | A raise at attach of a shard this process has raised changes its incarnation and keeps its grace instance; a start's raise, and the first raise of any other shard since the process started, copy the incarnation into the grace instance | invariant | RFC 14 §4.4, RFC 16 §2 (`Shard`), RFC 8 §2.5 step 2 | T6.1, T6.2, T6.10 |

**The share table and the bindings**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R6 | The share table is read on the request path without a lock; an attach or removal copies the whole table once per change, and no lock shared by more than one share is held across I/O or a wait on a write, read or commit | MUST NOT | §1.1 | T6.2 |
| R7 | A binding holds the share's handle on each journal one of its shards lives on, and on no other, opened by `Share` at attach, its settings copied from the share record and its state; its handles and state are never stored, and `Start` rebuilds every binding from the share records | invariant | §2.3, RFC 11 §2.1 (one shard, one journal device) | T6.2, T6.3 |
| R8 | `Bind` validates a share's settings and refuses an invalid one with an error, keeping the binding it had | MUST | §2.4, §2.3 | T6.2 |
| R9 | `Bind` of a share not served attaches it: reads its version floor and limit, passes the larger to `Share`, writes the share's limit at the journal's as a tracked max-write, raises the shard's incarnation, and only then publishes the binding; a failure undoes these in reverse and publishes nothing | invariant | §2.5 (`Bind`), E29, RFC 1 §3 | T6.2 |
| R10 | `Bind` of a served share applies a state change, serving to quiesced and back, without restarting the engine or touching another binding | invariant | §2.3 | T6.2 |
| R11 | A share's full limit never refuses another share's writes on the same journal | test row | §2.3, §15.2 shared journal | T6.2 |
| R12 | A content call naming a share with no binding answers `ErrDelay` with `DelayFrozen`, a share refused at start or left unattached by a lost journal included; only a share being removed or retired answers `ErrStale` | invariant | §12.1 | T6.2, T6.3, T6.9 |

**Start**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R13 | `New` opens nothing; `Start` runs its steps in order, each complete before the next; a failing step closes in reverse what earlier steps opened and returns its error, so the engine is serving or as built | invariant | §2.5 | T6.3 |
| R14 | The engine opens each journal itself through `Deps.Open`, with the floor and the identity read from the store; start never creates a journal | invariant | §2.5 step 1 | T6.3 |
| R15 | The floor a journal opens with is at least every share's version floor and version limit (D5) | invariant | §2.5 step 1, §2.7; RFC 1 §4.3 and RFC 28 §3.1 word it otherwise (#3634) | T6.3, T6.7 |
| R16 | A journal missing or foreign where one is recorded (an `Open` with a recorded `expect` answering `ErrNoJournal`, an `IdentityError` or `ErrUnidentified`): every share on that device (D10) is refused, each named in `Stats` with its device, and `Start` does not fail for it | MUST | §2.5 step 1, RFC 28 §3.4 | T6.3 |
| R17 | The directory of a missing or foreign journal is never opened as a new journal until an operator acknowledges the loss | MUST NOT | §2.5 step 1, RFC 28 §3.4 | T6.3 |
| R18 | An `Open` with no identity recorded for its directory (zero `expect`), which always fails, fails `Start`: the metadata store lost when the directory holds a journal, uninitialised when it holds none, and the node serves nothing until an operator says what happened | invariant | §2.5 step 1, RFC 28 §3.4 | T6.3 |
| R19 | A share whose removal has reached its offload step is never attached at start; once the journal is open and before any share attaches, `Forget` runs for every tag `Stats` names whose share's removal reads forgotten | invariant | §2.5 step 1, §2.3 | T6.3 |
| R20 | Before any version is issued, every served share's limit is written at the journal's next version plus one block, and passed with `SetVersionLimit` only once every write of it has committed; the floor at `Open` and every mark keep the epoch half zero | MUST | §2.5 step 1, §2.7, RFC 1 §4.3 | T6.3, T6.7 |
| R21 | An error a check meets at `Open`, rather than a refusal it makes, fails `Start` as any failing step does | invariant | §2.5 (`decision:`) | T6.3 |
| R22 | One transaction raises the incarnation of every shard the node will serve, before any verifier is returned | MUST | §2.5 step 2, RFC 14 §4.4 | T6.4 |
| R23 | For each file the journal lists, the operations above its `applied` from `Since` are applied to existence in version order, each write with the `mtime` its record carries, recording the usage they add | invariant | §2.5 step 3, E15 | T6.5 |
| R24 | Each file is synced before its re-applied existence commits, and `Settle` is called with the new `applied` only after the commit | invariant | §2.5 step 3, E19 | T6.5 |
| R25 | A synced loss record above `applied` has the dropped write's existence committed from it, so the extent reads **Lost**, never as the file was before the write | invariant | §2.5 step 3, §5.1 | T6.5 |
| R26 | A clone marker is a clone, never applied as a plain removal | invariant | §2.5 step 3 | T6.5 |
| R27 | A release marker whose phase 1 aborts on a holder is settled without its removal and reported as data loss naming the file, so no later start re-applies it | invariant | §2.5 step 3, §8.1 | T6.5 |
| R28 | No file is served before step 3 has run for it; a clone's destination waits for its clone (epic 8) | invariant | §2.5 step 8, E15 | T6.5 |
| R29 | Unfinished removals are found by listing each shard's `S‖id‖rr` index, never by scanning files, and run from the removal loop once serving, each one not done batch by batch and each done one by its prune, a release's leftover keys included, so start time does not grow with the removals pending | invariant | §2.5 step 4, RFC 16 §4.2 | T6.6 |
| R30 | Once every share the journal serves is attached, and before any file of the journal is served, every tag `Stats` names that no share record of the installation names is stranded and reported with its held and dirty bytes; a tag a share record names is never stranded | MUST | §2.5 step 5, RFC 1 §3 | T6.6 |
| R31 | A journal whose `Open` refuses with `ErrUnsuitableStorage` at `Start`: its shares are not attached, `Stats` names each with its device and the rule, `journal_unsuitable` is raised, `Start` succeeds, nothing in the directory changes, and the open is retried every 30 s | invariant | §2.5, RFC 1 §3, §5.4 | T6.10 |
| R32 | A retried open that succeeds attaches the journal's shares as `Bind` would (step 1 for that journal, its `Forget`s and limits included, the shards' incarnations raised at attach, steps 3 to 5) and serves each file once step 3 has run for it, then clears the condition; one that finds the journal missing or foreign leaves the shares unattached behind the operator gate and raises `journal_lost` in place of `journal_unsuitable`; one, the late attach included, that fails for any other cause closes what it opened, in reverse, leaves the shares unattached, and is retried; `journal_unsuitable` then names that cause | invariant | §2.5 | T6.10 |
| R33 | Until the late attach serves, a `Bind`, `Unbind` or `Drop` naming a share the journal serves or would serve answers `ErrDelay` | invariant | §2.5 | T6.10 |

**The version limit**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R34 | The trigger is evaluated on every version the engine issues, not on a timer | invariant | §2.7, §15.2 sustained load | T6.7 |
| R35 | When `Stats` shows fewer versions left than the larger of half a block and the peak version rate times the margin, every served share's limit is raised by a block, and the new limit is passed with `SetVersionLimit` only after every share's raise has committed. The peak is the highest count of versions drawn in any one second of the last 60 s; the margin is 30 s plus the existence age plus 1 s | bound | §2.7 | T6.7 |
| R36 | `Bind` raises every served share's limit first when the counter it raised leaves fewer versions than the trigger | invariant | §2.5 (`Bind`) | T6.7 |
| R37 | A write that reaches the limit is refused retry-later, never `ErrNoSpace`, and is served once the raise commits | test row | §2.7, RFC 1 §4.3, §15.1 version limit | T6.7 (the raise), T7.1 (the answer) |
| R38 | The raise never runs on the request path: a version issued posts the check and never waits for a transaction | MUST NOT | §1.1 | T6.7 |

**Stop, `Unbind` and `Drop`**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R39 | `Close` first refuses every call with `ErrUnavailable` | invariant | §2.5 stop step 1 | T6.8 |
| R40 | `Close` stops the background work that takes journal versions (the removal loop, clone batches): it refuses new dispatch, cancels the batches in flight and joins them, then commits every file's pending existence in a last group commit per journal, within the stop bound | invariant | §2.5 stop step 2 | T6.8 |
| R41 | `Close` cancels and joins the other loops (the version-limit raise, the retried open) | invariant | §2.5 stop step 3 | T6.8, T6.10 |
| R42 | `Close` closes what the engine opened (handles, then journals) in the reverse of the order it opened them, and nothing it was handed, only after every join has completed; no component is closed while work that uses it runs | MUST NOT | §2.5 stop step 4, E13, RFC 1 §8.7 | T6.8 |
| R43 | A join that does not complete within its bound leaves the components it depends on open and reports the failure | MUST | §2.5 stop step 5 | T6.8 |
| R44 | `Unbind` moves the binding to removing and, in this order: admits no new call, clone or removal batch for the share (but the phase-2 batches of a clone its join waits on); joins every call admitted before, every clone with the share on either side and every batch in flight | MUST | §2.3 | T6.9 |
| R45 | `Drop` ends the share's work in the removal loop, closes the share's handle on each journal, then runs the whole-tag `Forget`, which the journal refuses while a handle is open; `Forget` is idempotent, so a repeated `Drop` succeeds | invariant | §2.3, RFC 1 §3 | T6.9 |
| R46 | Node stop never calls `Drop`: `Close` closes handles and keeps tags | invariant | §2.3 | T6.8 |
| R47 | Every write `Forget` drops uncommitted gets `Done` from the engine, charging nothing, exactly once | invariant | §5.1, §12.1 (`WriteOpts.Done`) | T6.9 |
| R48 | `Close` and `Drop` end a pending release's retry: its `retry` channel yields `ErrUnavailable` and no `done(...)` follows; after `Close` its marker is left for the next start's step 3, after `Drop` the whole-tag `Forget` drops it with the tag and no start runs it | invariant | §8.1 and §12.1 (`Delete`), §2.3, RFC 1 §3 | T6.8, T6.9 |
| R49 | `Close` cancels a rebuilt clone's wait on `Holds.Served`, leaving its clone `Removal` for the next start's step 4, so it never hangs on a shard that is never served here; `Unbind` joins the clone, waiting out the `Served` wait, as R44 says | invariant | §2.5 step 4, §2.3, epic 8's D11 | T6.8, T6.9 |

## 4. Decisions this epic takes

- **D1, the share table.** One `atomic.Pointer` to an immutable map from `ShareID` to
  `*binding`. `Bind`, `Unbind`, `Drop`, `Start` and the late attach copy the map, change it
  and swap the pointer under one lifecycle mutex that no content call takes. The mutex is
  held only for the copy and the swap, never across I/O, a join or a wait, so one share's
  long `Unbind` never holds up another share's `Bind` (RFC 8 §2.3: a change to one share
  changes one binding). A set of the shares with a lifecycle call in progress, kept under
  the same mutex, answers a second `Bind`, `Unbind` or `Drop` naming one of them with
  `ErrDelay`, which its caller retries. A binding's state (serving, quiesced, removing) is
  an atomic in the binding, so a state change swaps nothing; attach and removal copy the
  table, as RFC 8 §1.1's `decision:` says. Epic 3's T3.1 already declares the table,
  `atomic.Pointer[bindings]` in `engine.go` read through `bindingOf(share)` in `file.go`,
  with that `decision:` at the `bindings` type. T6.2 extends it and keeps that marker. No
  marker of its own.
- **D2, how start learns the removals and the installation's tags.** Start step 1 needs
  each share's removal progress (`SH‖shard‖rm`) to skip a share past its offload step and to
  forget a tag whose removal reads forgotten, and step 5 needs every tag a share record of
  the installation names, attached or not. `Start` is handed the shares to attach (RFC 23
  §2.2), and `Lifecycle`'s four calls read neither. T6.1 gives `Lifecycle` a fifth call,
  `Tags(ctx) (map[ShareID]TagRecord, error)`: every share record's tag, as the store's
  `uint32`, with `Forgotten` when every one of the share's `rm` rows reads forgotten, step 4
  or done, `Offloaded` when every row otherwise reads offloaded or later, and `Serving` for
  the rest. Keyed by `ShareID`, because a resumed removal's `Drop` names only the share, and
  the engine learns a tag only from a `Share` handed to `Start` or `Bind`, which a share
  past its offload step never is. `Start` keeps each share at `Offloaded` or `Forgotten` in
  the table in D9's removed state, holding its tag and no handle, so the resumed removal's
  `Drop` forgets the tag and a content call naming the share answers `ErrStale`. A
  `decision:` marker at `Tags` says so, withdrawn if RFC 8 §2.1 settles another source
  (§9 point 1).
- **D3, who records a journal's identity.** No interface of RFC 16 writes `J‖journal`. T6.1
  adds `kvstore`'s `RecordJournal(ctx, device, id)`, the store's part of the explicit
  initialisation step, which the fixture calls after `Init` and production's initialisation
  step will. `J‖journal` is keyed by identity, so `JournalID(device)` scans `J‖` for the
  device, and `RecordJournal` deletes, in the same transaction, any other `J‖` key naming
  the device, so a re-initialised device keeps one key and the scan answers its new
  identity. A `ponytail:` marker at the scan names the ceiling (a node with hundreds of
  devices) and the upgrade (a device index key). #3635 proposes a node argument as well;
  T6.1 takes it if the RFC text does. Start never writes `J‖journal` (#3642).
- **D4, an attach keeps the grace instance only of a shard already raised.** `kvstore`'s
  `RaiseIncarnations` copies the incarnation into the grace instance on every call, which is
  right for a start and for the first raise of a shard since the process started, and wrong
  for an attach of a shard this process has raised (R5). T6.1 gives the call that meaning,
  renamed `RaiseAtStart`, and adds `RaiseAtAttach(ctx, shard)`, which keeps the grace
  instance; `Lifecycle` declares both. The engine keeps the set of shards it has raised since
  `Start`: an attach calls `RaiseAtAttach` for a shard in it and `RaiseAtStart` for any
  other, since the restart lost that shard's open state (RFC 14 §4.4). `kvstore`'s
  `decision:` at `RaiseIncarnations`, as #3606 words it, already gives the start form "the
  first serve of a share's shard this process has not served": T6.1 rewrites that marker for
  `RaiseAtStart` and adds no second one. Pending the RFC 16 §3 text (§9 point 2).
- **D5, the floor at `Open`.** One number per journal: the largest of every share's version
  floor (`Content.VersionFloor`) and every share's limit (`VersionLimits`) among the shares
  the journal may serve, removed ones included, read through the `ShareID`s `Tags` names
  (D2) whose shard lives on that journal (D10). `journalv2`'s doc for `Open` names the
  smallest limit; both are above every version issued, and the largest also covers a
  share whose limit was raised ahead of the others. A `decision:` marker at the computation
  says so, withdrawn if the journal's doc and RFC 28 §3.1 settle the smallest (§9 point 4).
- **D6, the block and the trigger.** The block is a package constant, 2²⁴ versions, set by
  a hook in `export_test.go` for the tests that need 16. The peak rate is 60 one-second
  buckets on `Clock.Elapsed`; the margin is 30 s plus `existence_age` plus 1 s, read from the
  settings at each check. A version issued (`Write`, `Draw`, a removal's journal step, a
  clone target) calls `limits.issued(journal, v)`, which counts it and compares the limit it
  last passed with `SetVersionLimit`, less `v`, with the trigger; past it, it wakes the
  journal's raise goroutine and returns. No `Stats` call on the request path: `Stats`
  returns copies of its per-share maps, a cost that grows with the shares on the journal.
  The raise reads `Stats().NextVersion` once per raise. One raise runs at a time per
  journal; one woken during a raise runs again after. The raise holds a per-journal raise
  lock from its read of the counter to its `SetVersionLimit`, and `Bind`'s limit step takes
  the same lock and writes the larger of the limit last passed and a pending raise's target:
  a `Bind` that wrote the journal's old limit between a raise's commit and its
  `SetVersionLimit` would leave the new share below the limit the journal is then given,
  against RFC 1 §4.3's "smallest of the durable per-share limits", and a crash would issue
  those versions again. The lock is the journal's, not the lifecycle mutex (D1), and no
  content call takes it.
- **D7, the stop bound.** RFC 8 §2.5 names "the stop bound" without a value. It is the
  sooner of the deadline of the context `Close` is given and a package default, 30 s from
  the call, so a `Close(context.Background())` over a stuck loop still returns: past it, a
  join is abandoned and reported, and what it depends on, the journal and every handle on
  it, is left open (R43). 30 s matches the server's `shutdown_timeout` default, which the
  node's stop has to fit in. A `decision:` marker at `Close` says so, withdrawn if RFC 8 or
  RFC 23 sets the bound.
- **D8, the start transaction is not split.** Step 2 raises every shard in one transaction,
  as RFC 14 §4.4 requires, though E40 bounds every engine transaction by `K`. `kvstore`'s
  `ponytail:` at `RaiseIncarnations` already names the ceiling; this epic adds none (§9
  point 3).
- **D9, a dropped share stays known.** `Drop` keeps the binding in the table in a removed
  state, holding no handle, so a content call naming the share answers `ErrStale` rather
  than `DelayFrozen` (R12) for as long as `Tags` names the share: a later start puts it back
  in that state (D2) until its record is gone. A `ponytail:` marker at `Drop` names the
  cost, one entry per share being removed, and the upgrade, a set of removed `ShareID`s.
- **D10, the journal a share lives on.** RFC 11 §2.1 holds a shard in one journal device,
  "**Release 1:** this binds the single node too", but nothing handed to `Start`, `Bind` or
  `Lifecycle` names it: RFC 8 §12.1's `Share` has no device, and RFC 16 records it only as
  the journal identity in `Shard.Primary`, which `entity.Shard` does not carry yet. T6.1
  adds the primary's journal identity to the shard record, and `Tags`' record (D2) carries,
  per share, the journal identity of each of its shards, so the engine finds the journal of
  a share it attaches and of one past its offload step alike. The engine matches it against
  the identity `JournalID` answered for each device: the share gets a handle on that
  journal only (R7), is refused with that device (R16), counts in that journal's floor only
  (D5), and its `Drop` runs `Forget` there. A share whose shard names the journal of no
  device in `Deps.Devices` is refused by name. The fixture writes the identity where it
  creates the shard, until the control plane creates shards. A `decision:` marker at the
  match says so, withdrawn if RFC 8 §2.1 or §12.1 settles another source (§9 point 14).

## 5. Target tree

```text
pkg/metadata/kv/
  counting.go        T6.1  a count of Updates and the most keys one Update wrote
pkg/metadata/entity/
  control.go         T6.1  Tags' record, its three removal states (D2) and its shards'
                           journal identities (D10)
  openstate.go       T6.1  Shard's primary journal identity (D10)
pkg/metadata/kvstore/
  lifecycle.go       T6.1  Lifecycle(): JournalID, VersionLimits, RaiseVersionLimits,
                           RaiseAtStart, RaiseAtAttach, Tags (D2 to D4, D10); RecordJournal
  shards.go          T6.1  RaiseIncarnations split into its start and attach forms (D4);
                           the shard codec with the journal identity (D10)
  kvstoretest/
    lifecycle.go     T6.1  the view's cases
pkg/block/enginev2/
  deps.go            T6.1  Lifecycle's new shape (D2, D4, D10)
  share.go           T6.2  the share table (D1), the binding, Bind
                     T6.9  Unbind, Drop, the removed state (D9)
  engine.go          T6.3  Start's step sequence and undo stack
                     T6.8  Close (D7)
  recover.go         T6.3  step 1, the floor (D5), the share's journal (D10), the refusals
                     T6.4  step 2
                     T6.5  step 3
                     T6.6  steps 4 and 5
                     T6.7  the version limit: the trigger, the peak rate, the raise
                           goroutine and its lock (D6)
                     T6.10 the retried open and the late attach
  health.go          T6.3  refused shares and the journal conditions, recorded in the one
                           condition set Stats reports, which T6.3 or epic 7's T7.3
                           creates, whichever lands first
  export_test.go     T6.7  the block hook; T6.2 the unit composition through Bind
  enginev2test/
    fake.go          T6.3  Open's refusals, Forget, Strand, Stats' tags and versions
                     T6.5  Files, Since, Settle
                     T6.7  SetVersionLimit and a counter bounded by it
    fixture.go       T6.1  the real Lifecycle and RecordJournal after Init
                     T6.2  a second share on the fixture's journal
                     T6.10 a second device and its journal
  conformance/
    rfc8_a_test.go   T6.2 to T6.7, T6.10  the rows of §15.1 this epic owns
    rfc8_b_test.go   T6.2, T6.9  the rows of §15.2
test/enginev2/known.yaml  T6.2 to T6.10  each new red row, naming this epic's issue
```

## 6. Subtasks

### T6.1 The store's lifecycle view

**Subtask T6.1 estimate: 4 man-days.**

- **Track:** C. **Depends on:** S2; the RFC 8 §2.1 and RFC 16 §3 edits that settle #3631
  and #3632, and §9 point 14.
- **Covers:** R1 to R5.
- **Deliverables:** `kvstore.Store.Lifecycle()` returning a view with `JournalID(ctx,
  device string) ([16]byte, error)`, `VersionLimits(ctx, shares)`, `RaiseVersionLimits(ctx,
  to)` writing at most `K` keys per transaction, each key a tracked max-write over
  `RaiseVersionLimit`'s rule, except that a share whose record reads being removed or retired
  is skipped instead of refused with `ErrStale`, since nothing attaches it again and one such
  share must not fail the raise for every share batched with it; `RaiseAtStart(ctx, shards)`
  and `RaiseAtAttach(ctx, shard)` (D4), rewriting `kvstore`'s `decision:` at
  `RaiseIncarnations` for `RaiseAtStart`, and `Tags(ctx)` (D2), its record declared in
  `entity` and carrying the journal identity of each of the share's shards (D10); the
  primary's journal identity added to `entity.Shard` and its codec (D10);
  `RecordJournal(ctx, device, id)` writing `J‖journal` and deleting any other key
  naming the device (D3); the engine's `Lifecycle` declaring the six calls. `kv.Counting`
  gains a count of Updates and the most keys one Update wrote, its first consumer here. In
  E2's fixture, the real view replaces the stub `Lifecycle` that refuses every call
  (`unbuiltLifecycle`), and the fixture records
  `Fixture.Journal`, the `JournalID` it kept from `Init`, through `RecordJournal`, so the
  view's `JournalID` answers it for the fixture's directory.
- **Acceptance criteria:**
  - `JournalID` of a device with no record answers the zero identity, not an error; of a
    device whose journal was re-initialised, the newer identity.
  - `RaiseVersionLimits` never lowers a limit, never commits more than `K` keys, and skips a
    share being removed or retired.
  - `RaiseAtAttach` leaves `GraceInstance` as it was; `RaiseAtStart` sets it to the new
    incarnation.
  - `Tags` names a share being removed with its `ShareID`, its tag and its `TagState`, and
    a retired share, each with its shards' journal identities.
- **Tests:** a raise to a lower value leaves the stored one (catches a blind write, R3); two
  concurrent raises, the higher held after its read by a `kv.KV` wrapper in the test until
  the lower one commits, end at the higher (catches an untracked read); 3 × `K` shares raised:
  no `RaiseVersionLimits` Update wrote more than `K` keys, and they took at least 6 Updates
  (catches one transaction over every share, and a batch of `K` shares, R4); an attach raise
  followed by a read of the shard record: incarnation up, grace instance unchanged (catches
  the start meaning at attach, R5); `JournalID` after `RecordJournal` for two devices answers
  each its own (catches a scan that returns the first key); `RecordJournal` twice for one
  device: `JournalID` answers the second and one `J‖` key names the device (catches a
  re-initialisation that keeps the old key); a share with two shards, one `rm` row at
  forgotten and the other at offloaded: `Tags` answers `Offloaded` under its `ShareID`
  (catches a state taken from one row only, D2); two shares whose shards name two journals:
  `Tags` answers each its own identity, and a shard record written and read back keeps it
  (catches an identity dropped by the codec, D10); over the fixture, `JournalID` of its
  directory equals `Fixture.Journal` (catches an identity not recorded or recorded under
  another device), red in `known.yaml` until the journal has `Init`, which the fixture calls
  before anything else.

### T6.2 The share table and `Bind`

**Subtask T6.2 estimate: 4 man-days.**

- **Track:** C. **Depends on:** T6.1; epic 3's T3.1 (the table it extends).
- **Covers:** R6 to R11, R3, R5, R12.
- **Deliverables:** in `share.go`, epic 3's table extended (D1), its `decision:` kept, and
  the set of shares with a lifecycle call in progress; the binding: `Share`, a handle per
  journal, the state; `Bind` for a share not served (validate, read floor and limit, `Share`
  at the larger, the limit written at the journal's, the attach raise D4 picks, publish; undo
  in reverse on failure) and for a served one (a state change); the set of shards raised
  since `Start` (D4). Epic 3's unit composition binds through `Bind` from here, and its hook
  goes. E2's fixture gains a second share on its journal.
- **Acceptance criteria:**
  - A content call never waits on the lifecycle mutex.
  - A `Bind` that fails at any step leaves no handle open and the table unchanged.
  - A share bound above a floor of 9·10⁸ gets a first version above it from the real
    journal.
- **Tests:** a content call on share A while a `Bind` of share B holds the lifecycle mutex
  answers (catches a table read under the mutex, R6); `-race` with readers and binds;
  the attach raise made to fail: the handle is closed and the table holds no binding
  (catches a binding published before the raise, R9); a negative limit refused and the
  earlier binding kept (R8); a served share bound again `Quiesced`, then `Serving`: the
  table pointer is unchanged, another share's writes are served throughout, and the share's
  state follows each call (catches a state change that copies the table or touches another
  binding, R10); a share not handed to `Start`, then bound: its `GraceInstance` equals its
  new incarnation (catches the attach form on a first serve after a restart, R5); a content
  call naming a share never bound answers `ErrDelay` with `DelayFrozen`, not `ErrStale`, and
  touches no journal (R12);
  over the fixture, §15.1's "attach above the floor", adapted: a share whose files' versions
  reach 9·10⁸ is bound to a journal near 2·10⁶ and written over: the version `WriteAt`
  returns is above 9·10⁸ (catches `Share` called without the floor, R9; with no offload or
  eviction in this part the read is no evidence); over the fixture, §15.2's
  "shared journal": two shares, one filled to its limit, the other's writes accepted (R11);
  with epic 3's R5, two shares bound through `Bind`, one saturated with writes: the other's
  write latency stays within its baseline and no `engine_lock_wait_seconds` area grows with
  the loaded share's rate (§15.2's "no cross-share slowdown", the write half).

### T6.3 Start step 1: open the journals

**Subtask T6.3 estimate: 4.25 man-days.**

- **Track:** C. **Depends on:** T6.2.
- **Covers:** R7, R13 to R21, R3, R12.
- **Deliverables:** `Start(ctx, shares)`: the step sequence with its undo stack (R13); for
  each device of `Deps.Devices`, `JournalID`, the floor (D5), `Deps.Open`; the error mapped
  by `expect`, read first: with a recorded `expect`, a journal missing, foreign or
  unidentifiable (`ErrNoJournal`, `ErrIdentityMismatch`, `ErrUnidentified`) refusing its
  shares by name and raising `journal_lost` (R16); with zero `expect`, every error failing
  `Start` as the store lost or the device uninitialised (R18); any other error failing
  `Start` (R21); `Tags`' forgotten tags forgotten, its offloaded ones not attached, and both
  kept in the table in D9's removed state (R19, D2); each share
  given a handle on the journal its shard's record names (D10), at the larger of its floor
  and limit, and a share whose shard names no opened journal refused by name; every served
  share's limit written at `Stats().NextVersion` plus a block, then `SetVersionLimit`; `Stats`
  naming refused shares and their device; the `decision:` marker of RFC 8 §2.5 on a check's
  error.
- **Acceptance criteria:**
  - A refused device never has `Deps.Open` called on it twice in one `Start`, and nothing
    else touches its directory.
  - No `WriteAt` or `Draw` reaches a journal before its `SetVersionLimit`.
  - A failure at any point closes every handle and journal opened so far, in reverse.
- **Tests:** over the fake: a store recording identity A and a directory holding B, both
  shares of the device refused by name, `Start` succeeds, the other device's share serves
  and has `Share` called on its own journal only (catches a foreign journal opened, R16,
  and a handle on every journal, R7, D10), and a content call naming a refused share
  answers `ErrDelay` with `DelayFrozen`, never `ErrStale` (R12); a failure injected at the
  last handle closes the others in reverse (R13); `Tags` with one forgotten and one
  offloaded tag: `Forget` called for the first before any `Share`, no `Share` for the
  second, and a content call naming either answers `ErrStale` (R19, R12); a device with no
  recorded identity whose directory holds a journal, which `Open` with zero `expect` answers
  with an `IdentityError`: `Start` fails naming the store lost and closes what it opened
  (catches the error mapped before `expect` is read, R18); with a recorded `expect`, an
  `Open` answering `ErrUnidentified`: the device's shares refused by name and `Start`
  succeeds (R16); an `Open` that returns an I/O error from its platform check: `Start` fails
  and closes in reverse (catches a check's error taken for a refusal, R21). Over the fixture:
  §15.1's "journal gone", adapted: two shares written without offload, the directory
  deleted, `Start` refuses both by name and creates nothing (catches a journal re-created,
  R17); §15.1's
  "attach after a device loss", adapted: the device destroyed, re-initialised as the
  operator's step would, the share attached to it: its first version is above its old limit,
  and a share whose removal reads offloaded is not attached.

### T6.4 Start step 2: the start transaction

**Subtask T6.4 estimate: 1 man-days.**

- **Track:** C. **Depends on:** T6.3; E2's T2.3 (the fault-injecting `kv.KV`).
- **Covers:** R22.
- **Deliverables:** `RaiseAtStart` over every served shard in one call, after step 1 and
  before step 3, so before any `Write` can return a verifier; its failure fails `Start`; the
  shards raised recorded in D4's set. This is the node's start transaction that RFC 14 has
  the filesystem service read the `GraceInstance` from: once the engine composes with the
  service, it replaces the stand-in raise #3606 puts in the service's `serve` (§9 point 12).
- **Acceptance criteria:** the incarnation of every served shard rises once per `Start`, in
  one transaction.
- **Tests:** over the fixture, §15.1's "incarnation at start", the engine half: every
  shard's incarnation and grace instance rose, in one Update by T6.1's count, and the
  verifier changed across the restart (catches a start that raises only at attach); the
  fault-injecting `kv.KV` fails the transaction: `Start` returns the error and every journal
  is closed. E2's `TestRFC8A_Verifier`, which restarts and expects a new verifier, stays red
  in `known.yaml` until this subtask, T6.8 and epic 4's commit are in and the journal has
  `Init`, `Open` and `WriteAt`. The `RECLAIM_COMPLETE` half is open state's and the
  adapters'.

### T6.5 Start step 3: existence re-applied

**Subtask T6.5 estimate: 5.25 man-days.**

- **Track:** C. **Depends on:** T6.4; epic 4's T4.3 (a file synced and its existence
  committed); epic 5's T5.4 (the abort routine).
- **Covers:** R23 to R28.
- **Deliverables:** for each served share's handle, `Files`; per file, `Existence.Applied`,
  then `Since(applied)` replayed into the file's entry as pending operations in version
  order: a write with its record's `mtime`, a truncate, a deallocate, a delete, a synced
  loss as a loss operation; a clone marker handed to epic 8's rebuild and taken out of the
  plain replay (R26); the entries committed through epic 4's per-file commit, which syncs
  first; `Settle` with the new `applied` after each commit; a release marker whose phase 1
  aborts committed as settled and logged at `Error` as data loss (R27); files served only
  after (R28). Large shares are processed file by file, never all `Since` streams held at
  once.
- **Acceptance criteria:**
  - After a restart, a write acknowledged and synced but uncommitted reads back with its
    size and `mtime`; a truncate whose metadata step was cut off is applied.
  - A second restart re-applies nothing and reports nothing.
  - No `Done` is called for a re-applied write: nothing was reserved.
- **Tests:** over the fixture and the journal's crash seam: §15.1's "recovery through
  `Since`" (a crash after a truncate's journal step, before its removal record: `Since`
  yields the marker, existence applies it before the file is served, `Settle` drops it;
  catches a marker applied after serving); the recovery halves of §15.1's "journal
  authority" and "stable write and lazy existence", which E2 wrote (their `known.yaml`
  entries name this epic); a synced loss committed as **Lost** (catches a loss read as the
  old content, R25); `Since` yielding, in version order, a write of 0 to 4 KiB at v3, a
  truncate to 4 KiB at v5 and a write of 8 to 12 KiB at v6: the committed size is 12 KiB and
  4 to 8 KiB reads as a hole (catches a replay that applies writes and removals as separate
  passes, R23); T5.4's abort: a holder keeping the file open, the marker settled, the log
  line naming the file, and the next restart quiet (R27); a release marker a crash left
  mid-retry finished by the restart; the share's usage counters read back after recovery
  equal the bytes re-applied, and a second restart leaves them unchanged (catches usage not
  recorded, or counted twice, R23). Over the fake: a clone marker reaches the rebuild hook
  and no `Remove` (R26); the fake records the file's sync, the existence commit and `Settle`
  in that order, and an existence commit made to fail leaves `Settle` uncalled and the
  marker yielded again at the next start (catches a `Settle` before the commit, or a commit
  before the sync, R24).

### T6.6 Start steps 4 and 5: removals resumed, unclaimed tags stranded

**Subtask T6.6 estimate: 2.25 man-days.**

- **Track:** C. **Depends on:** T6.5; epic 5's T5.3 (the removal loop).
- **Covers:** R29, R30.
- **Deliverables:** per served shard, `Content.UnfinishedRemovals`; each plain removal posted
  to the journal's removal loop, its phase 2 if not done and its prune if done; each clone
  `Removal` handed to epic 8's rebuild; then, per
  journal, the tags `Stats().Shares` names that `Tags` does not name, each `Strand`ed and
  logged at `Warn` with its held and dirty bytes.
- **Acceptance criteria:**
  - `Start` returns before any posted removal runs a batch.
  - A tag of a share being removed, or of a share not attached at this start, is never
    stranded.
- **Tests:** over the fixture, §15.1's "recovery through `Since`", second half: a removal
  crashed between batches resumes and the extent reads removed throughout; a crash after a
  removal's last batch and before its prune: the restart prunes it, a release's leftover
  keys included, and its `S‖id‖rr` entry goes (catches a resume that posts only removals not
  done, R29); over the fake, 10⁴
  unfinished removals posted and `Start` returning before the loop's first batch (catches a
  start that runs them, R29); a tag left by a deleted share stranded before `Start` returns,
  so before any file of the journal is served, and a share's tag that
  `Tags` names but `Start` was not handed left alone (catches a tag computed against the
  attached set, R30).

### T6.7 The version limit while serving

**Subtask T6.7 estimate: 3.5 man-days.**

- **Track:** C. **Depends on:** T6.3; epic 3's T3.2 and T3.3; E2's T2.3.
- **Covers:** R34 to R38, R3, R4, R15, R20.
- **Deliverables:** in `recover.go` (D6): the per-journal peak-rate buckets,
  `issued(journal, v)` called from `Write` and `Draw` (epics 5 and 8 add their own calls),
  the raise goroutine: `RaiseVersionLimits` for every served share by a block,
  `SetVersionLimit` after the last transaction commits, under the journal's raise lock;
  `Bind`'s limit step under the same lock, at the larger of the limit last passed and a
  pending raise's target; `Bind`'s raise-all-first (R36); RFC 8 §2.7's headroom `decision:`
  and burst `ponytail:` markers at the trigger; the goroutine pushed on `Start`'s undo stack
  (R13), so a later failing step stops it, and joined by `Close`.
- **Acceptance criteria:**
  - The journal never gets a limit the store has not committed.
  - A request that issues a version past the trigger returns without waiting for the raise.
- **Tests:** over the fixture, §15.1's "version limit": a block of 16, `S‖id‖vl` committed
  before the first write; below 8 left the limit is raised before the journal is given it;
  the raise blocked by the fault-injecting `kv.KV`, the write reaching the limit refused and
  then served once the raise commits (its `ErrDelay` half turns green with T7.1); a crash
  with a reported, unsynced version, then a restart whose first write's version is above it
  and no version issued again (catches a floor taken from the version floor alone, R15, and
  a limit not written before the first version, R20); a raise held by a `kv.KV` wrapper
  between its last commit and `SetVersionLimit`, then a share bound: its `S‖id‖vl` is at
  least the limit the journal ends with (catches a `Bind` that writes the journal's old
  limit, D6). Over the fake: a rate held
  below the trigger and raised in one step, the raise due on the next `Draw` with the test
  clock not advanced (catches a timer, R34); the peak computed from a 60 s history (catches a
  window of the last second only); with a non-zero `existence_age` E and a steady peak rate
  r, versions left above half a block and between 31·r and (31 + E)·r make the raise due
  (catches a margin without the existence age, R35); a `Bind` whose `Share` leaves the
  journal's counter within the trigger: every served share's limit is raised before the new
  share's limit is written (R36); 3 × `K` shares: no raise Update wrote more than `K` keys,
  and a raise took at least 6 Updates by T6.1's count (R4); one share marked removing while a
  raise is in flight, and every other share's limit still raised and given to the journal
  (catches one removed share failing the batch).
  §15.2's sustained-load and burst rows are epic 7's T7.4, over this raise.

### T6.8 `Close`

**Subtask T6.8 estimate: 3.75 man-days.**

- **Track:** C. **Depends on:** T6.5, T6.7; epic 4's T4.3; epic 5's T5.3.
- **Covers:** R39 to R43, R46, R48, R49.
- **Deliverables:** `Close(ctx)`: refuse every call with `ErrUnavailable`; stop the removal
  loops and clone batches, refusing new dispatch, cancelling the batches in flight and
  joining them (each pending release's `retry` yielding `ErrUnavailable`, with no
  `done(...)`, its marker left for start); the cancel of every rebuilt
  clone's `Served` wait, through the context epic 8's T8.6 waits under, its clone `Removal`
  left for start; the last group commit per journal; cancel and join the
  version-limit raise and the retried open; close every handle, then every journal, in
  reverse, only after every join; a join past the stop bound (D7), the sooner of the
  context's deadline and the default, leaving its components open, the journal and every
  handle on it, and reported in the error.
- **Acceptance criteria:**
  - After a clean `Close`, a restart finds no append or hole fill to re-apply.
  - No `Handle.Close` or `Journal.Close` is called while a loop that uses it runs.
  - `Close` never calls `Forget`.
- **Tests:** over the fake: a removal loop held in a batch past the context's deadline,
  `Close` returns its error and the journal and every handle on it are still open (catches
  a close under a live loop, R42, R43); the same with `context.Background()` and the test
  clock advanced past the default: `Close` returns its error (catches a bound taken from
  `ctx` alone, D7); the fake records the order of every close (catches journals closed before
  handles); a content call and a `Bind` made once `Close` has begun answer `ErrUnavailable`,
  and the fake records no version drawn after it (R39); a clean `Close` and restart over the
  fixture with `Since` yielding nothing above `applied`, the share's tag still in `Stats` and
  a synced write reading back (catches a `Close` that calls `Forget`, R40, R46); T5.4's
  `ErrUnavailable` outcome: a `Close` while the loop retries a release's phase 1 yields
  `ErrUnavailable` on its channel with no `done(...)`, leaves the marker, and a restart then
  settles it (R48); a `Served` stub that never returns: `Close` returns within its
  deadline, the wait's goroutine is gone and the clone `Removal` is still listed at the next
  start (catches a `Close` hung on a shard never served, R49), written with T6.8 or T8.6,
  whichever lands last; `prof.Watch` finds no goroutine left.

### T6.9 `Unbind` and `Drop`

**Subtask T6.9 estimate: 4 man-days.**

- **Track:** C. **Depends on:** T6.8; epic 4's T4.3 and T4.7.
- **Covers:** R12, R44, R45, R47, R48, R49.
- **Deliverables:** `Unbind(ctx, id, keepHolds)`: `keepHolds` refused with `ErrInvalid`;
  the binding moved to removing, which content calls answer `ErrStale`; the join of calls
  admitted before, of clones on either side (epic 8 joins its own) and of the share's batch
  in flight, a rebuilt clone still waiting on `Holds.Served` included, so `Unbind` waits
  out that wait (epic 8's D11 raises `removal_stalled` while it lasts), with the lifecycle
  mutex not held (D1). `Drop(ctx, id)`: the share's files taken out of the group commit's
  due set and its in-flight batch joined, so no later batch captures one; the removal loop's
  retries of the share's releases ended with `ErrUnavailable`, with no `done(...)`, their
  markers then dropped by the whole-tag `Forget`; each handle closed, then the whole-tag
  `Forget`, the tag read from the binding, or from the removed entry `Start` kept for a
  share past its offload step (D2); `Done` with nothing charged for every write `Forget`
  dropped; the binding kept as removed (D9).
- **Acceptance criteria:**
  - From the moment `Unbind` is called, no call, clone or batch for the share starts, but
    the phase-2 batches of a clone the join waits on.
  - After `Drop`, no entry for the share's tag remains in the journal, and every accepted
    write of the share has had exactly one `Done`.
- **Tests:** over E2's fixture, §15.2's "unbind and drop", adapted (no offload pass):
  `Unbind` with 10⁴ pending entries and a removal batch in flight returns only once the
  batch has returned; while it is held in that join, a content call, a removal batch and a
  clone posted for the share are each refused (catches an `Unbind` that joins first and
  refuses after, R44); no entry for its tag remains after `Drop`, red in `known.yaml` until
  the journal has `Forget` (§9 point 10). Over the fake: one call per content method that
  returns an error, naming the share after `Unbind`, each `ErrStale`, not `DelayFrozen`
  (R12); `Drop` under writes with a batch due as it joins: T4.7's reservation counter back
  to zero and one `Done` per write (catches a `Done` missed or doubled, R47); `Forget`
  refused while a handle is open is never reached (catches `Forget` before `Close`, R45); a
  restart with the share's `rm` row at offloaded, then `Drop`: no handle closed, `Forget`
  run for its tag, `Stats` names no such tag, and a content call naming the share answers
  `ErrStale` (catches a `Drop` that knows only bound shares, R45, R12); an `Unbind` held in
  its join while another share's `Bind` completes, and a second `Unbind` of the same share
  answering `ErrDelay` (catches the lifecycle mutex held across a join, D1); T5.4's stall
  ended by `Drop` yielding `ErrUnavailable` with no `done(...)` and, once `Drop` returns, no
  marker of the tag in the journal (R48); `Unbind` of a share whose rebuilt clone waits on a
  `Served` stub: `Unbind` has not returned while the stub blocks, and returns only once the
  stub is released and the clone is done (catches a cancel in place of the join, R44, R49),
  written with T6.9 or T8.6, whichever lands last.

### T6.10 A journal refused as unsuitable at start

**Subtask T6.10 estimate: 3.75 man-days.**

- **Track:** C. **Depends on:** T6.6 and T6.9 (`Close`, `Unbind` and `Drop`); `journalv2`
  declaring `ErrUnsuitableStorage`.
- **Covers:** R5, R31 to R33, R41.
- **Deliverables:** in `recover.go`, an `Open` refused with `ErrUnsuitableStorage` leaving its
  shares unattached, named in `Stats` with device and rule, `journal_unsuitable` raised; one
  retry goroutine per refused journal on `Clock.After(30 s)`; a successful retry running
  step 1 for that journal, the attach raise D4 picks per shard and steps 3 to 5 for it, then
  clearing the condition; a missing or foreign journal at retry raising `journal_lost` in its
  place; any other failure, the late attach included, closing in reverse, leaving the shares
  unattached, naming the cause in `journal_unsuitable` and retrying; `Bind`, `Unbind` and
  `Drop` naming its shares answering `ErrDelay` meanwhile; a failed `Start` stopping the
  retries; `Close` joining them at its step 3; a closing flag set at `Close`'s step 1,
  checked by the retry before it opens and before it publishes, so a retry inside `Open`
  when `Close` begins starts no loop after stop step 2 has joined the removal loops and
  closes what it opened (E13, R42), and no join made under the lifecycle mutex (D1). E2's
  fixture gains a second device and its journal.
- **Acceptance criteria:**
  - A retry never opens a directory after `Close` or a failed `Start` returned.
  - A retry that fails leaves nothing open.
  - A retry that `Close` overtakes starts no loop and publishes no binding.
- **Tests:** over E2's fixture, with the test clock: §15.1's "unsuitable journal at start",
  adapted (no reseed): `Start` succeeds and the other device serves; the refused share is not
  attached, `Stats` names it with its device and the rule, `journal_unsuitable` is raised and
  nothing in the directory changed; the check cleared and the clock advanced 30 s: the share
  attaches, its content reads back, its shard's incarnation is higher than when `Start`
  returned and its `GraceInstance` above its value before the restart, the write verifier
  differs from before the restart and the condition clears; `Close` with the retry pending,
  then the check cleared: nothing opens the directory; the start transaction failed: `Start`
  returns its error and nothing opens the directory later; the directory emptied before the
  check clears: `journal_lost` in place of `journal_unsuitable` and nothing created; `Unbind`
  on the refused share answers `ErrDelay` and succeeds once it serves; a retry whose recovery
  fails once on I/O leaves the share unattached, `journal_unsuitable` names the error, and
  the next retry attaches it; a late attach whose `Since` is held: a read of that journal's
  files answers `ErrDelay` until step 3 has run for it (catches a file served before its
  recovery, R32). Red in `known.yaml` until the journal declares
  `ErrUnsuitableStorage` and refuses at its seam (§9 point 10). Over the fake: the retry
  loop's unit tests (one goroutine per refused journal, none left after `Close`); a retry's
  `Open` held, `Close` called, then `Open` released: no removal loop starts, the journal is
  closed, and `Close` returns (catches a late attach that outlives stop step 2, E13).

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 red suite in CI"]):::sync --> T6_1["T6.1 Lifecycle view"]
  rfc(["RFC 8 §2.1 and RFC 16 §3 text for #3631, #3632"]):::sync -.-> T6_1
  T6_1 --> T6_2["T6.2 Share table and Bind"]
  T6_2 --> T6_3["T6.3 Start step 1"]
  T6_3 --> T6_4["T6.4 Start step 2"]
  T6_4 --> C1(["after T6.4: shares bound and journals opened, no recovery"]):::sync
  ep3(["epic 3: Write, Draw"]):::sync -.-> T6_7
  T6_3 --> T6_7["T6.7 Version limit"]
  ep4(["epic 4: per-file existence commit, reservation counter"]):::sync -.-> T6_5 & T6_8 & T6_9
  ep5(["epic 5: abort routine, removal loop"]):::sync -.-> T6_5 & T6_6 & T6_8
  T6_4 --> T6_5["T6.5 Start step 3"]
  T6_5 --> T6_6["T6.6 Start steps 4 and 5"]
  T6_5 & T6_7 --> T6_8["T6.8 Close"]
  T6_8 --> T6_9["T6.9 Unbind and Drop"]
  J(["journalv2: ErrUnsuitableStorage"]):::sync -.-> T6_10
  T6_6 & T6_9 --> T6_10["T6.10 Unsuitable journal at start"]
  T6_6 & T6_9 & T6_10 --> S3(["S3 engine complete over the fake journal"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T6.1 to T6.4 need only the RFC text for #3631, #3632 and §9 point 14, epic 3's table (T3.1)
and E2's `kv.KV` (T2.3), and fill the wait for epics 4 and 5; T6.7 follows as soon as epic
3's `Write` and `Draw` land. T6.5 is the riskiest, for the order of sync, commit and
`Settle` across a crash, and starts once epic 4's T4.3 and epic 5's T5.4 exist. Checkpoint
after T6.4: a share bound and served over the fake journal; the fixture's `Start` gets past
`Lifecycle` once the journal has `Init`, which the fixture calls first. Checkpoint after
T6.6: a restart over the fake journal recovers every file.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T6.1, T6.2, T6.3, T6.4 | 13.25 | S2; T3.1, T2.3; the RFC text for #3631 and #3632 and §9 point 14 | the store view, the table, `Bind` and the journals opened: what needs only epic 3's table and E2's `kv.KV` |
| 2 | T6.7 | 3.5 | PR 1; T3.2, T3.3, T2.3 | the version limit while serving, on the paths that issue versions |
| 3 | T6.5, T6.6 | 7.5 | PR 1; T4.3, T5.3, T5.4 | recovery: existence re-applied, removals resumed, tags stranded |
| 4 | T6.8, T6.9 | 7.75 | PR 2, PR 3; T4.7; T8.6 for the `Served` wait's test | the ends of the engine and of a share: `Close`, `Unbind`, `Drop` |
| 5 | T6.10 | 3.75 | PR 3, PR 4; `ErrUnsuitableStorage` | the retried open and the late attach, which reuse steps 1 to 5 and call `Close` and `Unbind` |

A suggestion only: one pull request per subtask works as well. PR 2 can be open beside PR 3.

## 9. Open points

1. **How start reads the removals and the installation's tags** (D2), pending #3631. RFC 8 §2.5 step 1
   forgets a tag "whose share's `rm` rows read forgotten" and does not attach one at
   "offloaded or later", and step 5 strands "a tag no share record of the installation
   names"; RFC 8 §2.1 says `Lifecycle`'s four methods are the engine's only store access
   outside block metadata's views and `Refs`, and RFC 23 §2.2 hands `Start` only the
   attached shares. One of them must change: RFC 16 §3 gaining `Tags`, keyed by `ShareID`
   so a resumed removal's `Drop` finds the tag, is D2's choice, and #3631's proposed fix
   should take that shape. It changes E1's frozen `Lifecycle`, so T6.1 waits for the RFC
   text.
2. **An attach keeps the grace instance** (D4), pending #3632. RFC 16 §3 has one `RaiseIncarnations` "in
   the start transaction, or at attach", and `kvstore`'s copies the incarnation into the
   grace instance on every call, which RFC 14 §4.4 and RFC 16's `Shard.GraceInstance` allow
   only for a start or a failover that lost open state. RFC 16 §3 should declare the two
   forms, and say that the first raise of a shard since the process started takes the start
   form, since the restart lost its open state (D4); #3632's proposed `RaiseAttached` needs
   that rule. It changes E1's frozen `Lifecycle`, so T6.1 waits for the RFC text.
3. **The start transaction against `K`** (D8), pending #3633. RFC 8 §2.5 step 2, RFC 0 and RFC 14 §4.4 put
   every shard in one transaction; E40 and §2.7 bound every engine transaction by `K`, and
   §2.7 takes 10⁴ shares' 2 × 10⁴ keys as past it. On a node serving more shards than `K`,
   the two cannot both hold.
4. **The floor at `Open`** (D5), pending #3634. RFC 8 §2.5 says "per share, the larger of the version
   floor and the share's version limit"; RFC 28 §3.1 calls the floor "the highest content
   version recorded in metadata", with no limit; RFC 1 §4.3 and `journalv2`'s doc for `Open`
   take the smallest limit. One sentence in RFC 28 §3.1 would settle it.
5. **Who writes `J‖journal`** (D3), pending #3635. RFC 16 §4.2 has the key "written when the journal is
   created", and no interface of RFC 16 §3 writes it; RFC 16 §3.2's `Init` writes `\x00format`,
   `IN` and `SL` only. RFC 16 should name the write, and how `JournalID(device)` finds a key
   that is keyed by identity.
6. **`journal_lost` at start.** #3641 added `journal_lost` to RFC 8 §11.1's list, but only
   "when a retried open finds the journal missing or foreign", while T6.3 raises it at
   `Start` too, as RFC 0 §10 and RFC 25 read; pending #3661.
7. **E2's rows.** E2 delivered four rows of its 45, and three name this epic in
   `known.yaml`: journal authority and stable write and lazy existence (T6.5), and the
   verifier, which needs a restart that raises the incarnation (T6.4, T6.8) and epic 4.
   The rows this epic writes (incarnation at start, recovery through `Since`, journal gone,
   attach above the floor, attach after a device loss, version limit, shared journal,
   unbind and drop, unsuitable journal at start) are written in T6.2 to T6.10, and their
   estimates include them. E2's fault-injecting `kv.KV` (its T2.3) is not built either.
   T6.4 and T6.7 depend on it, and its days stay in E2's estimate.
8. **A read-only journal reopened in place** (RFC 8 §2.5, E42, and §15.2's read-only row) is
   in no epic of this part. It needs RFC 1 §5.4's read-only state, which `journalv2` does not
   have yet; once it does, it is a subtask of this epic or of the second part.
9. **`Unbind` with `keepHolds`** is refused: no snapshot exists in this part. A removal that
   keeps snapshots is RFC 12's, out.
10. **No row over the real journal turns green in this epic** until `journalv2`'s `Init`,
    `Open`, `Share`, `Forget`, `Strand`, `SetVersionLimit`, `Files`, `Since` and `Settle`
    exist. Each row leaves `known.yaml` in the change that turns it green.
11. **Two text fixes, merged.** RFC 1 §4.3 now sends the version limit's sizing to RFC 8
    §2.7, and §15.2's sustained-load row says a block of 2²⁰ is the design it must fail
    (#3641, closing #3637 and #3638). T6.7 follows §2.7.
12. **Who raises the incarnations once the engine composes with the filesystem service.**
    #3606's `serve` raises every share's shard in its own transaction, "standing in for the
    node's start transaction", and RFC 14 §4.4 has the service read the `GraceInstance` the
    node's start transaction raised. T6.4's raise is that transaction, so the integration
    epic drops the stand-in; a shard bound after `Start` is raised by `Bind`, in the form D4
    picks.
13. **A failed open at `Start` stops the node** (R21, RFC 8 §2.5's `decision:`). #3585
    proposes leaving such a journal closed and retrying it, as T6.10 does for an unsuitable
    one. If RFC 8 takes it, R21 and the `decision:` change, and T6.10's retry covers the
    other errors.
14. **Which journal a share lives on** (D10), with #3631. RFC 11 §2.1 binds a shard to one
    journal device on a single node too, and RFC 8 §2.3 gives a binding "the share's handle
    on each journal", but neither `Start`'s shares, RFC 8 §12.1's `Share`, `Lifecycle` nor
    RFC 16 §3 names a share's device; RFC 16 records it only as the journal identity in
    `Shard.Primary`. RFC 8 §2.1 or §12.1 should name the source: a `Lifecycle` read of the
    shard's journal identity, D10's choice, or a field on `Share`. It changes E1's frozen
    `Lifecycle`, so T6.1 waits for the RFC text, as for points 1 and 2.
