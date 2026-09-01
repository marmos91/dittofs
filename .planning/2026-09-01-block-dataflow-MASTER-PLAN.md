# Master plan — block data-flow: audit findings + pier/crane/ferry refactor

Status: **FOR DISCUSSION.** No code written. HIGH findings filed as #2227-#2231.

Inputs, both authoritative, neither superseded by this document:
- `2026-09-01-journal-audit-report.md` — 59 verified findings (6 HIGH / 8 MED / 45 LOW), 70 agents, 7 lenses, adversarially verified.
- `2026-09-01-pier-library-design-PLAN.md` — the target design (state model, three interfaces, folder structure, test/bench contracts).

Baseline: `ff14b24cb` (origin/develop). Audit tree: `~/dittofs-worktrees/audit-journal` (immutable).

---

## 1. Verdict

**Refactor, don't rewrite. Extract to package boundaries now, hold the repos.**

The audit's central result is not the bug count. It is the *shape* of the bugs:

> **All five distinct HIGH findings are residency-truth failures** — the cache's map of what
> it holds disagreeing with what it holds, resolving to zeros or to reclaimed live data.
> Not one is a throughput, eviction-policy or chunking bug.

That is the same signature as the entire prior field history (#1850, #1879, #1888, #2084,
#1872/#2073/#2093, #1956, #2110). Ten independent incidents, one defect class, over a year.
A design that makes that class unrepresentable is worth the disruption; one that merely
re-tidies the same model is not.

Corroborating the extraction case specifically:
- `RemoteStore`/`BlockID` are entirely dead — never implemented, never read, `nil` at the one
  production call site. The remote seam pier *actually* has is inbound-only (`Fill`), which is
  what makes the split viable.
- Import coupling is trivial: 11 `logger.Warn`, 2 `block.ErrFutureFormat`, one `blake3` call.
- Semantic coupling is real but localised: three type-asserted manifest interfaces, all in carve.

## 2. The five HIGH findings

| # | Issue | Finding | Where | Model |
|---|---|---|---|---|
| H1 | #2227 | `RestoreToVersion` blind to `cold.log`: deletes remote-durable files, zero-fills cold ranges | `store.go:1087` | `StateRemote` read as `StateAbsent` |
| H2 | #2228 | `repackSegment` never sets `target.records`: synced-gate calls a still-dirty segment evictable | `reclaim.go:834` | `StateDirty` → `StateLost` |
| H3 | #2229 | `RestoreToVersion` re-materializes via `WriteAt` and never fsyncs, against its own doc | `store.go:1079` | acknowledged ≠ `Durable` |
| H4 | #2230 | Torn `cold.log` tail never physically truncated at recovery — poisons every future append | `recovery.go:358` | cold markers lost → `StateAbsent` |
| H5 | #2231 | `Hydrate` has no delete-fence — a racing cold read resurrects a deleted file | `store.go:456` | tombstone fails to fence a transition |

H2 was found independently by two lenses (`bugs` and `gaps`), which is the strongest confirmation
signal the run produced.

**Three of five (H1, H3, and H1's blast radius) are in `RestoreToVersion` — the method with
ZERO test call sites across all 29 test files.** Package coverage is 79.2% and hides it entirely.
`CheckFormat`, `JournalVersion`, `PinVersion`, `SetPinVersion` and `FileCount` are equally
untested.

**A mitigation I would have assumed, and which the verify gate killed:** for H1 one might expect
`shares.SeedColdFromManifest` (snapshot.go:1595) to re-seed cold ranges afterwards. It is gated
`if remoteVerify`, and `RestoreToVersion` runs *only* on the `!remoteVerify` local-only branch.
There is no compensation, and the erroneous tombstones are fsynced before anything could undo
them. Do not let this reasoning get re-derived optimistically later.

## 3. Findings mapped against the design

Three buckets. The bucket decides *when* the work happens, not whether.

### 3a. Fix now, independent of the refactor (do not wait)

These are live defects on `develop` today. They are not blocked by, and must not be bundled
into, the extraction.

| Finding | Fix | Effort |
|---|---|---|
| **H2** `target.records` never set | one `Store` beside the existing two at `reclaim.go:834-835` | trivial, high value |
| **H4** torn `cold.log` tail | `loadCold` returns the intact offset; truncate+fsync before any later append, mirroring `loadSegment` | small |
| **H1** restore cold-blindness | fold `loadCold` into phase 1 as `applyColdLog` already does; re-assert cold entries in phase 2 rather than `Delete` | medium |
| **H3** restore never fsyncs | `groupCommit` per touched shard before returning | trivial |
| **H5** `Hydrate` delete-fence | per-shard `tombVer[id]`, gate `Hydrate`/`hydratable` as `truncVer` already gates | medium |
| **M** `Truncate`'s `truncVer` uses the pre-marker peek version | raise `truncVer` from the marker's real version in the same locked section | small |

**Every one needs a regression test that fails on unmodified `develop` first.** H1/H3 need
`RestoreToVersion` tests that do not exist at all — writing those is the prerequisite, and is
independently valuable regardless of what happens to the refactor.

### 3b. Closed by construction — the refactor removes the possibility

Fix nothing here separately; the design eliminates the class.

| Finding | Closed by |
|---|---|
| `doc.go`'s false stdlib/two-interfaces claim | §9.1 import-graph **test** — a claim that cannot rot |
| Dead API: `RemoteStore`, `BlockID`, `PinVersion`, `SegmentLocation`, `GC` | deleted; `RemoteStore` becomes `ferry.Store` |
| Three type-asserted manifest interfaces | evaporate: one becomes `DurableTail`, two become the caller calling itself |
| `store.go` 1297 / `reclaim.go` 999 / `carve.go` 995 | §6.2 file layout |
| `segment.go`/`index.go` holding the hot paths their names disown | `write.go` / `read.go` |
| Two files named `carve_dispatch.go` | one `ferry` |
| Six near-duplicate test store-openers | one harness in `piertest/` |
| `SetCarveTargets` unsynchronized field write | gone — collaborators move to construction |
| Doc drift (`gc.go`, `logblob.EvictBlob`) | rewritten wholesale |

### 3c. Created by the refactor if not handled — the new risks

These do not exist today. The design introduces them and must close them.

| Risk | Mitigation | Source |
|---|---|---|
| A free-form `durable []Extent` lets a buggy `fn` flip a range pier never offered — today's `flipIdx` cursor makes that structurally impossible | pier validates extent provenance against the offered-and-unflipped set | v3.1 change 2 |
| Reap outside the flush lock deletes the *next* pass's row — on a **2-second cadence**, not adversarial timing | `FlushOptions.AfterFile`, under the lock. **Mandatory** | v3.1 change 4 |
| Concurrent `fn` calls race over block boundaries | `fn` called strictly sequentially; ALL upload concurrency lives in ferry | v3.1 change 1 |
| `DurableTail` computed eagerly per file loses the per-run cost property | computed lazily, per call | v3.1 change 3 |
| `Extents()` collapsing five queries regresses the hot path | `Size` and `DurableExtent` stay separate methods; benchmark and keep receipts | review finding |

## 4. Sequencing

**Order: ferry → crane → pier.** Reverse of the data flow: leaves first, trunk last.

The property that earns this order: **steps 1 and 2 are behaviour-preserving.** No on-disk
format change, no state-model change, no flip-contract change — pure extraction, verifiable by
the existing suite plus the crash rigs. All semantic risk concentrates in step 3, taken last,
when the other two modules are already proven in production.

### Step 0 — the HIGH fixes (§3a), before any extraction

Each its own PR, each with a regression test that fails on unmodified `develop`. Landing these
first means the extraction starts from a correct baseline and is not competing with bug fixes
for review attention.

### Step 1 — ferry

Touches `engine/carve_dispatch.go` (upload window), `journal/carve_dispatch.go` (dispatcher),
`engine/blocksink.go` (the PUT). Does not touch on-disk format, state model, flip contract, or
the interval index. Delivers the ordered-completion contract step 3 depends on.

### Step 2 — crane

Touches `journal/carve.go`'s packing loop and `pkg/block/chunker`. The critical test is
**boundary stability**: the same blob chunked in one call vs. many must yield identical
boundaries, or dedup silently stops working across the fleet.

### Step 3 — pier

The seam inversion, `StateLost`, `AfterFile`, provenance validation, the query collapse, the
file reorganisation. Everything in §3c lands here.

### Step 4 — legacy cleanup

Separate, independently revertable workstream. **339 `legacy`/`Legacy` mentions in non-test
`pkg/block/`**, most of it outside the journal: the legacy-CAS object layout, `materializeLegacy`
called on **every** `WriteAt` and `ReadAt` in `fs.go`, vestigial config knobs
(`use_append_log`, `rollup_workers`, `stabilization_ms`, `orphan_log_min_age_seconds`),
`Locator`'s standalone form, `ContentHash`'s two legacy JSON encodings, `FileChunk`'s
multi-row-per-hash tolerance.

**Scope reduced by D4.** With no production stores in the field, most of this needs **deletion,
not deprecation**. The expensive part of removing a migration path is the answer to "what happens
to a store that skipped it" — and today there are no such stores. Do not build migration
scaffolding, compatibility shims, or multi-release deprecation windows for users who do not
exist; that is precisely the kind of speculative work this codebase's own conventions reject.

What still needs care, and only this: any format-affecting removal must be reflected in
`format.go`'s version stamp so a downgraded binary **refuses** rather than reading the result as
holes. That guard is the entire migration story now.

Still not bundled into refactor PRs — separate workstream, separate revert.

## 5. API completeness — DittoFS uses only public APIs

**Requirement: after extraction, DittoFS reaches pier, crane and ferry through their exported
APIs only. No embedding, no type assertions on the concrete type, no unexported structural
interfaces, no custom back doors.**

### Why this is a correctness requirement, not a style one

Today there are two contracts. `local.LocalStore` declares 18 methods. The informal one is
reached by structural typing on the concrete type:

| Site | Reaches | Failure mode |
|---|---|---|
| `local/fs/fs.go:68` | embeds `*journal.Store` — leaks all 31 exported methods onto `*FSStore` | any method added to pier silently appears on FSStore, ungated |
| `engine/flush.go` | `restorer`, `pinner`, `versioner`, `coldSeeder` | **silent no-op** if the assertion fails |
| `engine/offline.go` | `coldRangeReporter` | silent no-op |
| `shares/legacy_verify.go` | `coldSeedTracker` | silent no-op |
| `shares/blockstore_config.go:475` | `interface{ SetVerifyReads(bool) }` | silent no-op |

Those assertions' own doc comments say "No-op when the local store is not journal-backed." So a
method rename inside pier does not break the build — it silently disables snapshot pinning, cold
seed tracking, or per-read verification, and every test not exercising that exact path stays
green. **That is the same silent-failure shape as H1-H5.** `fs.go:57-63` already flags the embed
as a known hazard; this requirement discharges it.

### The enforcement mechanism

> **Only the construction site may name `*pier.Cache` (likewise `*crane.Boxer`, `*ferry.Ferry`).
> Everywhere else holds an interface DittoFS declares.**

Consequences, all of them wanted:

1. Any capability DittoFS needs must be on the library's exported API. A gap becomes a **compile
   error at the interface declaration**, not a silent no-op at runtime.
2. `FSStore` holds a named field, not an embed. Pass-throughs are written by hand — which is the
   point: each one is a deliberate decision that this method belongs in the tier's contract.
3. Every type assertion listed above is deleted. If the capability is real, it goes on the
   interface; if it is not, its call site goes.
4. **The API is complete iff DittoFS compiles.** That is a far better completeness test than any
   amount of review, and it runs on every build.

Greppable gate for "done":
```
rg '\*pier\.Cache|\*crane\.|\*ferry\.' --type go | grep -v _test.go
```
returns only the construction sites. Plus, in each library's own test suite:
`var _ SomeConsumerInterface = (*pier.Cache)(nil)` — so the library itself pins the contract it
promises.

### The forcing function

If DittoFS needs something the public API does not offer, that is a **signal the API is
incomplete**, and the resolution is to add it deliberately — with a doc comment, a test, and a
place in the surface — never to reach around it. Every such addition is a design decision made
in the open rather than a structural interface added quietly in a consumer.

This applies per step: ferry's consumers use only ferry's API at the end of step 1, crane's at
the end of step 2, pier's at the end of step 3. The requirement is not deferred to a cleanup pass.

## 6. The green bar

Per step, all of:

1. `go test ./...` and `go test -race ./...` green.
2. E2E green (`test/e2e/run-e2e.sh`).
3. **Crash rigs green on both the old and the new build**: `test/crash/device-loss.sh`,
   `test/crash/invalidate-cold-loss.sh`. A rig that passes only on the new build proves nothing
   about a regression.
4. Benchmarks recorded before and after on the same hardware. **Not in Docker Desktop** — it
   inflates DB round-trips ~2.7x and has already produced one wrong headline number here.
5. New regression tests **run against a deliberately broken build** before being trusted. A test
   that passes on unmodified code proves nothing about the bug it claims to pin.

Expect to add regression tests as we go — the untested-method list in §2 is the starting set,
not the complete one.

## 7. Contributors

External contributors are onboarding now and will reach the block module soon. **They should
learn the target vocabulary, not the one being retired** — so the RFC goes out before they get
there, not after.

Proposed: an RFC PR into `docs/internals/` (line-level review, versioned with the code, lands as
the permanent architecture doc) plus one GitHub Discussion as the announcement and
open-questions space, linking to it. **Discussions is currently disabled** on the repo;
`gh repo edit marmos91/dittofs --enable-discussions` turns it on.

## 8. Decisions taken

- **D1. Step 0 runs first, alone.** All five HIGH fixes plus their regression tests land before
  any extraction begins. The refactor starts from a correct, tested baseline, and a regression
  during steps 1-3 is unambiguously attributable to the refactor rather than to a concurrent bug
  fix — which matters specifically because this is the silent-zeros path.
- **D2. `RestoreToVersion` ships in pier, with a real test suite.** "Rewind to LSN V" is generic,
  and it needs ceiling-replay machinery over on-disk records that the public surface will not
  expose — keeping it DittoFS-side would mean widening pier's surface to let it reach in. The
  cost is accepted: pier must carry it AND test it before it is fit to publish. Its tests are
  part of step 0 (they are the prerequisite for fixing H1 and H3 at all), not deferred to step 3.
- **D4. H1 is NOT an incident — no production exposure.** Customers are evaluating DittoFS;
  nothing runs in production. So the five HIGH findings are serious bugs to fix on the normal
  path, not a data-loss event to respond to, and step 0 does not need an emergency release.
  **This also cheapens step 4 substantially** — see §4.
- **D3. The 45 LOW findings fold into the step that touches their file.** No separate backlog, no
  issue churn.
  **Guard against the known failure mode of D3:** LOWs living in files no step rewrites would be
  silently dropped. Therefore `2026-09-01-journal-audit-report.md` stays in the repo as the
  record, and step 3 ends with an explicit sweep: walk all 45, mark each addressed / consciously
  declined / still open. A LOW that is declined is a decision, not an oversight — but it has to
  be written down as one.

## 9. Open questions

1. **Who reviews step 3?** It rewrites the silent-zeros path. One reviewer is not enough, and the
   external contributors will not be up to speed in time.
2. **Does step 0 ship as a release?** No production exposure (D4), so this is a judgement call
   about testers' builds rather than an incident response. Cutting one anyway is cheap and gives
   the refactor a clean tagged baseline to diff against.
