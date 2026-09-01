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
| A free-form `durable []Extent` lets a buggy `fn` flip a range pier never offered — today's `flipIdx` cursor makes that structurally impossible | pier validates every returned extent against the offered-and-unflipped set, matching on **(offset, length, version)** — offset alone would flip new bytes when old ones were uploaded, which is the #1872 shape | §4.1 C4 |
| Reap outside the flush lock deletes the *next* pass's row — on a **2-second cadence**, not adversarial timing | `FlushOptions.AfterFile`, under the lock. **Mandatory** | §4.2 |
| Concurrent `fn` calls race over block boundaries | `fn` called strictly sequentially; ALL upload concurrency lives in ferry | §4.1 C1 |
| `DurableTail` computed eagerly per file loses the per-run cost property | computed lazily, per call | §4.1 C3 |
| `Extents()` collapsing five queries regresses the hot path | `Size` and `DurableExtent` stay separate methods; benchmark and keep receipts | review finding |
| `fn` erroring skips the reap, stranding superseded rows that DID commit | on error pier flips validated extents and **still calls `AfterFile`** | §4.1 C5 |
| Deferred credit bunches most flips onto the `Final` call on upload-bound files | accepted; must be **measured** (residency + time-to-flip vs today) before code | §4.4 |
| A shared `ferry.Completions()` channel routes one file's upload completion to another file's callback — many files flush concurrently and `Submit` carries no `FileID` — silently losing durability credit | `Submit` returns a **per-call future**; no shared stream exists | §4.1 C3 |
| Whole-extent credit matching forfeits a whole run when one unrelated byte moves — the common case on scattered writes, since `splitRuns` groups by offset only | validate and flip **per fragment**, mirroring `flipUpTo` | §4.1 C4 |
| One `crane.Boxer` hoisted to `Syncer` lifetime interleaves two files' bytes into one block — **cross-file corruption**, and pier cannot detect it | stated caller obligation: `fn` and its accumulator constructed fresh per `Flush` | §4.1 C9 |

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

Separate, independently revertable workstream. **384 `legacy`/`Legacy` mentions in non-test
`pkg/block/`** (measured 2026-09-01; the earlier 339 has grown), and they are not evenly spread:

| package | LOC | legacy mentions |
|---|---|---|
| **`local/fs`** | 925 | **200** |
| `engine` | 11,304 | 77 |
| `remote` | 374 | 27 |
| `pkg/block` root | 2,038 | 24 |
| `remote/memory` | 579 | 20 |
| `remote/s3` | 1,258 | 16 |
| `compression` + `encryption` | 1,597 | 20 |
| **`journal`** | 6,290 | **0** |

`journal` is already clean. **`local/fs` alone holds 52% of the module's legacy surface**, which
makes it the first thing step 4 removes — see §4.1 below. The remainder: the legacy-CAS object
layout, vestigial config knobs
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

#### 4.1 — delete `pkg/block/local/fs` outright

`local/fs` is 925 non-test LOC in four files, and it is **not** a storage tier. It is a wrapper:

| file | LOC | what it is |
|---|---|---|
| `fs.go` | 321 | ten one-line forwards to the embedded `*journal.Store`, plus the legacy gate |
| `legacy_reader.go` | 254 | legacy |
| `legacy_migrate.go` | 204 | legacy |
| `legacy.go` | 146 | legacy |

604 of those 925 lines are legacy and die with D4. The file's own header calls what remains
*"string↔FileID data-plane shims (shadow the embedded FileID methods)"* — ten methods whose entire
body is `journal.FileID(payloadID)`, plus `Start()` and `SetMetrics()`, which are empty stubs.

Once the legacy is gone there is no tier left to keep. Only four behaviours in the package are
not legacy or shim, and each has a better home in pier:

1. `journal.CheckFormat(dir/journal)` **before** `Open` — the guard that stops a directory written
   by a newer release from reading as holes. Move it *inside* `pier.Open`, where it cannot be
   skipped by a caller. Note it is on the zero-test list in §2: it needs a test as it moves.
2. The `filepath.Join(dir, "journal")` subdirectory convention — pier's own layout decision.
3. `durable = true` — a field.
4. The `nil` remote argument — that is the dead `journal.RemoteStore` seam, which §7.3 turns into
   `ferry.Store`. It disappears on its own.

**What stays:** `local.LocalStore` — the interface DittoFS declares — is exactly the §5 mechanism
and must survive. It has two real implementations (`local/memory` is production-imported at
`shares/blockstore_config.go:25`, not a test double), so it is not a one-implementation interface.
It is the *wrapper* that goes, never the contract. `pier.Cache` satisfies `local.LocalStore`
directly.

**The one real decision — the ID type.** `journal.FileID` is a defined type (`type FileID string`),
which is the only reason the ten shims exist. Either alias it (`type FileID = string`, zero
call-site churn) or re-declare `local.LocalStore` in terms of `pier.FileID` and convert at the
metadata boundary instead. **Take the second.** An alias discards the only type safety the
conversion currently buys, and §5's entire premise is that this boundary is checked at compile
time. The churn is mechanical and the compiler finds every site.

**Why this leads step 4 rather than trailing it:** `local/fs` holds 200 of the module's 384 legacy
mentions. One package deletion removes 52% of the legacy surface, and it removes the embed that
§5 identifies as the live silent-failure hazard. It also shrinks §5's own workload — with the
package gone, the "pass-throughs written by hand" item has nothing to write.

**Reachability, checked:** no non-test caller reaches past `FSStore` to the embedded
`*journal.Store`; every consumer holds `local.LocalStore`. So the shadowed-method hazard is
prospective, not a live bug, and the deletion is not urgent — it is just clearly correct.

## 5. API completeness — DittoFS uses only public APIs

**Requirement: after extraction, DittoFS reaches pier, crane and ferry through their exported
APIs only. No embedding, no type assertions on the concrete type, no unexported structural
interfaces, no custom back doors.**

### Why this is a correctness requirement, not a style one

Today there are two contracts. `local.LocalStore` declares 20 methods. The informal one is
reached by structural typing on the concrete type.

**Measured, not assumed** — a runtime type-assertion probe against `*fs.FSStore`, the production
local store, resolves every site below. The result splits the table in two, and the halves need
different fixes:

| Site | Reaches | Holds today? |
|---|---|---|
| `local/fs/fs.go:68` | embeds `*journal.Store` — leaks all 31 exported methods onto `*FSStore` | n/a — this embed is *why* the rest hold |
| `engine/flush.go:262,275,285` | `restorer`, `pinner`, `versioner` | ✅ yes, via promotion |
| `engine/offline.go:59` | `coldRangeReporter` | ✅ yes |
| `shares/legacy_verify.go:248,258` | `coldSeedTracker`, `legacyArchiveMigrator` | ✅ yes |
| `shares/blockstore_config.go:475` | `interface{ SetVerifyReads(bool) }` | ✅ yes |
| `engine/legacy_migration.go:66` | `legacyChunkFileMigrator{MigrateLegacyChunkFiles}` | ❌ **never — no implementation exists** |
| `engine/legacy_migration.go:79` | `prober{HasLogBlobSubstrate}` | ❌ **never — no implementation exists** |

So the hazard is **prospective, not live**. Seven assertions resolve because `FSStore` embeds
`*journal.Store` and promotion covers them; a rename inside pier would not break the build, it
would silently disable snapshot pinning, cold seed tracking, or per-read verification, and every
test not exercising that exact path would stay green. **That is the same silent-failure shape as
H1-H5, held off by nothing but an embed.** `fs.go:57-63` already flags it; this requirement
discharges it.

The last two are a different finding and are **already dead**. Neither `MigrateLegacyChunkFiles`
nor `HasLogBlobSubstrate` is implemented anywhere in the repository — the doc comment at
`legacy_migration.go:28` names `FSStore.MigrateLegacyChunkFiles`, which does not exist — so
`migrateLegacyCAS`'s local phase is unreachable in every configuration. That is not an API-boundary
problem; it is deletion work, and it belongs to step 4.

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

### 6.1 Each module owns its own tests

**A module's tests live in that module, and must pass with nothing else in the tree.** pier's
suite runs against pier alone; likewise crane and ferry. This is not tidiness — it is the same
compile-time completeness check as §5, applied to behaviour:

1. A test that can only be written from DittoFS names a capability the library's public API does
   not expose. That is a gap to close deliberately, exactly as §5's forcing function requires.
2. `git subtree split` must stay mechanical. A module whose tests live in its consumer does not
   survive extraction — it arrives at its new home green and untested.
3. Today's suites are the opposite of this: the residency behaviour is pinned largely from
   `pkg/block/engine` and `pkg/controlplane/runtime`, which is why a journal-level defect could
   reach production five separate times with a green tree.

Concretely, per module:

- **`piertest/`** — the one options-taking store-opener replacing today's six near-duplicate
  openers (§6.3), plus the residency-truth state table: every operation's effect on all five
  states, including `StateLost`. `RestoreToVersion` gets the real suite D2 promises here, not in
  a consumer.
- **`cranetest/`** — boundary stability first (the same blob chunked in one call vs. many must
  yield identical boundaries, or fleet-wide dedup silently degrades), then packing determinism.
- **`ferrytest/`** — ordered completion under concurrency, window saturation, retry idempotence,
  and the per-call-future contract that replaces the shared `Completions()` channel (§4.1 C3).

What stays in DittoFS: integration tests that cross module boundaries, the E2E suite, and the
crash rigs — those test the composition, which is DittoFS's job and no library's.

Two rules carry over unchanged and apply inside each module: a new regression test is run against
a **deliberately broken build** before it is trusted, and a guard that has never actually refused
anything is unverified rather than proven.

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

## 10. The external API — what the runtime calls

Everything above is internal shape. This section fixes the surface DittoFS's control plane and
protocol adapters actually call when a read or write handler touches a file. It is written last
because the module split only pays off if this boundary is narrow.

### 10.1 Where it stands

`*engine.Store` exports **46 methods; 44 have callers outside `pkg/block`.** That is not an API,
it is the absence of one. Measured on the same tree, it also contains:

| Method | State | Consequence |
|---|---|---|
| `EvictLocal` | takes the close lock, does `_ = payloadID`, returns `nil` | a **no-op that reports success**. `shares/blockstore_ops.go:365` increments `result.LocalFilesEvicted` per file, so the admin API reports N evicted when zero were. The real work is the `DrainLocalSynced` call below it. |
| `LocalStore()` | `return nil`, unconditionally — **deliberate** | Not a defect: the journal switchover (`489594417`) made local GC obsolete, and the nil is how it was retired — the journal self-manages segment reclaim. But `ShareLocalStores()` therefore always returns empty, so `blockgc.go:196`'s `runLocalGC` loop body never executes. `runLocalGC`, `ShareLocalStores`, `LocalStoreEntry`, `collectGarbageLocalFn` and `engine.CollectGarbageLocal` are **dead machinery kept alive for a caller that cannot fire** — a step-4 deletion, not a bug. |
| `Local()`, `RemoteStore()` | hand out the internal tiers | the §5 disease one level up: the runtime reaches around the API instead of through it |
| `HealthCheck` / `Healthcheck` | both exist, different signatures | casing drift on one concept, both externally called |
| `Stats`, `GetStats`, `GetStatsLite`, `LocalStats`, `SyncCounts` | five | one concept |

### 10.2 The shape follows from NFS and SMB, not from taste

Both protocols have the **same** write model, and it is precisely *unstable write + explicit
commit*:

| | NFS v3/v4 | SMB2/3 |
|---|---|---|
| write carries a stability request | `stable_how`: `UNSTABLE` / `DATA_SYNC` / `FILE_SYNC` | `SMB2_WRITEFLAG_WRITE_THROUGH` |
| write reports what was achieved | `committed` + `writeverf` | status |
| explicit commit | `COMMIT(offset, count)` — **range-scoped** | `SMB2 FLUSH` |

Today's engine exposes `WriteAt(ctx, payloadID, offset, data) error` — no stability in, no
durability out — and `Flush(ctx, payloadID)`, which is **whole-file**. The cost of that is
already visible in the adapters and they do not even agree with each other:

- `internal/adapter/nfs/v4/handlers/write.go:217` hardcodes `UNSTABLE4`, commented *"always
  returns UNSTABLE4 (cache is always enabled)"*. The server cannot tell a client that bytes are
  durable **even when they are**, so every NFS client must issue a COMMIT round-trip and hold its
  retransmit buffer until it completes.
- `internal/adapter/smb/handlers/write.go:461` computes and honours `writeThrough`.

One adapter implements the capability, the other throws it away, because the block store has no
way to express it. That is an API defect, not an adapter defect.

### 10.3 The data plane — six methods

```go
type Stability uint8
const (
    Unstable Stability = iota // buffered; a Commit is required
    DataSync                  // data durable, metadata may lag
    FileSync                  // data + metadata durable
)

// Verifier changes if and only if the store lost unstable writes. It is the
// client's ONLY signal to resend — NFS writeverf, and the same value SMB needs
// after a session reconnect.
type Verifier [8]byte

type DataPlane interface {
    // want is the caller's request; got is what was ACTUALLY achieved and may
    // exceed want. Returning got is what lets an adapter answer NFS's committed
    // field honestly and let the client drop its retransmit buffer without a
    // COMMIT round-trip.
    WriteAt(ctx context.Context, f FileID, off int64, p []byte, want Stability) (got Stability, err error)
    ReadAt(ctx context.Context, f FileID, off int64, p []byte) (int, error)

    // Range-scoped, mirroring NFS COMMIT(offset, count). length == 0 means
    // "to end of file", which is the SMB FLUSH case.
    Commit(ctx context.Context, f FileID, off, length int64) (Verifier, error)

    Truncate(ctx context.Context, f FileID, size int64) error
    PunchHole(ctx context.Context, f FileID, off, length int64) error
    Delete(ctx context.Context, f FileID) error
}
```

`got Stability` and the range-scoped `Commit` are the two additions. Neither is speculative
generality — each is a field the wire protocols already carry and the current API drops.

### 10.4 Residency is part of the API, not an internal detail

Every one of H1-H5 was a disagreement about where bytes live, and **no interface exposed that
question**, so no caller could ask and no test could pin it. The five-state model becomes public:

```go
type State uint8
const (
    Absent State = iota // never written; reading zeros is CORRECT
    Dirty               // local only — the sole copy, never reclaimable
    Synced              // local and remote-durable
    Cold                // remote-durable, not local; a read faults it in
    Lost                // was written, not retrievable — reads MUST error, never zero
)

type Span struct { Off, Len int64; State State }

// Extents is the query that makes StateLost observable. OfflineReadiness,
// manifest_check, and the offline gates all become callers of this instead of
// each re-deriving residency from a different partial view — which is what let
// #2110 report Safe() for a lost interval.
func (s *Store) Extents(ctx context.Context, f FileID) ([]Span, error)
```

Lifecycle, drain, snapshot/restore, capacity and stats each get their own small interface. Mixing
them into one receiver is how 46 methods happened.

### 10.5 `pkg/block` root holds the API and nothing else

After §4.1 and the legacy deletion, the root keeps the contract and the types the contract names —
`FileID`, `Stability`, `Verifier`, `State`, `Span`, `ContentHash`, `FileChunk`, `Locator` — and
nothing else. Everything that is machinery moves down into `pier` / `crane` / `ferry` / `engine`;
everything legacy is deleted outright rather than relocated. A reader opening `pkg/block/` should
see what the block store promises, not how it keeps the promise.

### 10.6 Folder and file structure

Two rules, both forcing functions rather than aesthetics:

1. **No non-test file over ~400 LOC.** Current state: **18 of 108 files exceed it, 5,565 LOC above
   the line** — `journal/store.go` 1297, `engine/syncer.go` 1056, `journal/reclaim.go` 999,
   `journal/carve.go` 995, `engine/gc.go` 876, `remote/s3/store.go` 751, `engine/readwrite.go` 735,
   `engine/fetch.go` 693. The journal side of that list is already scheduled by §6.2/§6.3; the
   engine side is not, and needs the same treatment. The limit is a guideline with teeth: a file
   that will not fit is a file holding more than one responsibility, which is the condition every
   one of §6.3's entries describes.
2. **A file's name states its responsibility.** `segment.go`/`index.go` holding the hot read and
   write paths is the anti-pattern; `write.go`/`read.go` is the fix.

### 10.7 One integration test and one benchmark for the whole ingestion path

Per §6.1 each module tests itself, but **composition is DittoFS's job**, and the ingestion path
has never been exercised end to end in one place. Add exactly one of each, both driving the §10.3
API in the shape an adapter drives it:

- **Integration test** — adapter-shaped writes (unstable, then a range `Commit`) through
  `WriteAt → pier → crane → ferry → remote`, asserting the **residency transition after every
  step** via §10.4's `Extents`, not just the final bytes. It must cover: unstable write leaves
  `Dirty`; commit moves to `Synced`; eviction moves to `Cold`; a cold read faults back in; and a
  deliberately corrupted remote produces `Lost` **with an error**, never zeros. That last case is
  the one no existing test covers and the one that has shipped five times.
- **Benchmark** — the same path, measured, with the `Verifier` stable across the run. It is the
  before/after number §6 item 4 requires, and it is what will show whether §4.4's deferred-credit
  change moved time-to-flip.

Both live in DittoFS, not in a module: they test the seam, and the seam is what the split creates.

## 11. Format migration

Deferred from §9 open question 3 and now decided. This replaces that entry.

### 11.1 Compatibility classes, not a version number

Do not ask "what version is this store". Ask **what an older binary must do when it meets
something it does not understand** — the ext4/btrfs model, and the only one that keeps cheap
changes cheap:

| Class | An older binary… | Your term |
|---|---|---|
| `compat` | opens read-write, ignores the feature | minor |
| `roCompat` | may open **read-only** only | minor, one-way |
| `incompat` | **must refuse** | major |

A single monotonic integer collapses all three into "refuse", which is why every change becomes
a major one. `format.go`'s stamp plus `journal.CheckFormat` already implement the `incompat` row;
this generalises it. Carry the flags as three bit sets in the stamp, not one number.

**Re-check the gate on every transition, not only at open.** btrfs shipped a bug where a store
with unsupported `roCompat` features could be opened read-only and then *remounted read-write*,
because the check ran before the read-only flag was set. Any future read-only mode inherits that
trap.

### 11.2 Registry — migrations are files in a folder

```go
type Class uint8 // Compat | ROCompat | Incompat

type Migration struct {
    From, To Stamp
    Class    Class
    Desc     string

    Up   func(ctx context.Context, s Substrate) error
    // Down is present ONLY where a revert is honestly possible. Its absence is
    // a fact about the migration, not an omission — see 11.4.
    Down func(ctx context.Context, s Substrate) error
}
```

One `.go` file per migration in a `migrations/` folder, self-registering via `init()`. The runner
sorts by `From`/`To`, refuses a registry with a gap or a fork, and applies iteratively.

### 11.3 One runner, two substrates

The algorithm — read stamp, select, order, gate on class, apply, record, abort — is identical
local and remote. Only three primitives differ, so only those are injected:

```go
type Substrate interface {
    ReadStamp(ctx context.Context) (Stamp, error)

    // CompareAndSetStamp is the FENCE. It must fail if the stamp changed since
    // ReadStamp. This is the whole concurrency story; everything else assumes it.
    CompareAndSetStamp(ctx context.Context, from, to Stamp) error

    Units(ctx context.Context, m Migration) iter.Seq2[Unit, error]
}
```

- **`pier.Substrate`** — stamp is a file; the fence is write-temp + fsync + rename.
- **`ferry.Substrate`** — stamp is an object at a well-known key; the fence is a conditional PUT.

#### Where the remote fence lives

Measured on this tree: `pkg/block/remote/` implements **no `If-None-Match`/`If-Match`**, and
shares **do** share buckets (`acquireRemoteStore`/`releaseRemoteStore` ref-count by
`remoteConfigID`). Two shares on one bucket, no conditional write, and no fsync to fall back on.

**Client-side CAS is not an option and must not be attempted.** Compare-and-set cannot be built
out of non-atomic primitives — read-check-write has a window under every schedule, and an
emulation moves the race rather than closing it. A fence that cannot refuse is worse than no
fence, because it accrues confidence in proportion to how long it is silently useless.

**The fence goes in the metadata store instead.** All four backends implement `WithTransaction`
(badger, sqlite, postgres, memory), so `CompareAndSetStamp` for the remote substrate is a
transactional row update there, not an object write. This fences the reachable case: shares that
share a bucket share a metadata store within one deployment.

It does **not** fence two independent deployments pointed at one bucket. The answer to that is
refusal, not emulation — §11.5 already makes major migrations an explicit operator action, and
the runner states plainly that it cannot detect a second deployment.

**Conditional PUT is hardening, not the foundation.** `PutObjectInput` in
`aws-sdk-go-v2/service/s3 v1.97.3` already carries `IfNoneMatch`/`IfMatch`, so the client is
capable; whether a given backend honours it is a per-backend fact to probe, not assume. Where it
is supported, use it as a second, independent check. Where it is not, the metadata fence stands
alone and the runner says so. Never let its absence silently degrade into no fence at all.

This is not hypothetical work in another sense either: the S3 layout has already changed once.

`cas/{hh}/{hh}/{hex}` → `blocks/<blockID>`, migrated by the ad-hoc code §4 deletes.

### 11.4 Revert means abort and un-expand, never downgrade

Roll-forward is the operational default; reverting a completed destructive change generally is not
possible without a backup. What makes revert cheap is **expand/contract**: add the new form,
dual-write, backfill, and only then remove the old. Everything before the contract step is
reversible; the contract step is the point of no return.

So `Down` exists for pre-contract steps and for aborting an in-progress `Up`, and the runner
records which phase a migration reached so it knows which is available. A migration whose `Down`
is `nil` past the contract point is stating a fact, and the runner must say so plainly rather than
appear to offer a revert it cannot perform.

The documented failure mode is stopping halfway — shipping the expand and never contracting,
leaving both forms alive forever. Each migration names the release its contract phase lands in.

### 11.5 Trigger

`compat` and `roCompat` migrations apply automatically at open. `incompat` migrations **refuse to
open** and print the `dfsctl` command to run, so a data-rewriting change is an operator decision
with a window to take a backup. This is a deliberate change from today's behaviour, where the
cas→blocks pass runs backgrounded on every boot with no operator involvement.

### 11.6 Relationship to D4

D4 says delete legacy paths rather than build scaffolding for users who do not exist, and it still
holds — this machine is **not** a reason to keep any of §4's legacy surface. The two are about
different things: D4 governs *this* cleanup, §11 governs *future* format changes, and its first
migration should be written when a real format change needs one, not speculatively. What ships
with the refactor is the stamp, the three classes, the registry and the gate — the parts that must
exist before the first migration, and no migrations at all.

## 12. `harbour` — the assembly

pier, crane and ferry are three libraries; something must wire them. Today §6.2 assigns that to
`dittofs/pkg/block/engine`. Moving the wiring into the library set makes the trio testable and
`git subtree split` shippable — otherwise the split yields three modules nobody can assemble
without re-deriving the `Flush` seam from prose.

**It is not a fourth peer module, and it holds no policy.** Every judgement — when to evict, what
counts as durable, what the manifest says — stays in DittoFS. An assembly that starts deciding is
the god object relocated, and it would re-absorb precisely the residency-truth judgements that
produced H1-H5.

Its scope is defined by subtraction. **Enforcement of the §4.1 seam contract stays in pier**, for
everything pier can observe: per-fragment credit validation, strictly sequential `fn`,
`AfterFile` under the flush lock. The assembly owns only the residue pier structurally *cannot*
see:

1. **C9 — the fresh-per-`Flush` accumulator.** The design records that pier cannot detect a
   hoisted `crane.Boxer`, and that hoisting interleaves two files' bytes into one block:
   cross-file corruption, undetectable from pier. Today C9 is a *stated caller obligation*. The
   assembly makes it structural by constructing the accumulator inside a per-`Flush` factory, so
   a hoisted one cannot be expressed — the same move as ferry's per-call futures replacing the
   shared `Completions()` channel.
2. **Config consistency** — crane's chunk params must match what pier records, or dedup silently
   degrades across the fleet. One constructor, one place to get it wrong.

That is the whole surface: a constructor and a `fn` factory. If it grows a third responsibility,
that is the signal it is absorbing policy.

```
harbour/
  harbour.go    New(pier, crane, ferry, Options) — assembles, validates config consistency
  flush.go      the per-Flush fn factory; C9 made structural
  README.md
```

**The name is load-bearing.** A harbour is the *place* a pier, its cranes and its ferries sit —
not an actor. `harbourmaster` was the closer fit for "sequences the three" and was rejected for
exactly that reason: a harbourmaster has authority, and a package named for authority invites the
policy drift §12 exists to prevent. If someone proposes adding a decision to `harbour`, the name
itself is the argument against it.

**D14. The assembly is `harbour`**, ships with the library set, and holds no policy.

§10.7's ingestion integration test and benchmark move here, since this is the smallest thing that
can run the whole path without DittoFS in the tree.

### D9-D12 — migration and assembly (§11, §12)

- **D9. Scope is the block store only.** The four metadata backends keep their existing schema
  handling. The block format is the one that has actually churned twice.
- **D10. Compatibility classes, not a version integer**, and the runner/substrate split of §11.3
  is committed: one runner, `pier.Substrate` and `ferry.Substrate`, three injected primitives.
- **D11. Revert means abort-in-progress and undo pre-contract only.** No downgrade of a completed
  destructive migration. `Down` is optional and its absence is a stated fact.
- **D12. Minor applies automatically, major refuses and requires an explicit command.**
- **D13. The remote fence lives in the metadata store**, not the bucket. Client-side CAS is
  forbidden. Conditional PUT is defence in depth where the backend supports it.

## 13. Merged audit results — journal + engine + block root

Three audits, one rubric. `pkg/block/journal` (2026-09-01, baseline `ff14b24cb`),
`pkg/block/engine` and `pkg/block` root (both at `4ec814bc2`). Reports:
`.planning/2026-09-01-journal-audit-report.md`, and `report.md` in each audited directory of the
`audit-engine` worktree.

| Audit | LOC | HIGH | MED | LOW | Agents |
|---|---|---|---|---|---|
| journal | 6,290 | 6 (5 distinct) | 8 | 45 | 70 |
| engine | 11,304 | 1 | 13 | 52 | 89 |
| block root | 2,038 | 0 | 2 | 20 | 45 |
| **total** | **19,632** | **7** | **23** | **117** | **204** |

### 13.1 The new HIGH extends the taxonomy

**Remote sweep reclaims a hash a concurrent dedup write just re-referenced**
(`engine/gc_sweep_index.go:69`). Independently re-verified here; two refutation attempts failed.

1. `sweepFromSyncedIndex` decides orphan-vs-live from `syncedAt` vs grace cutoff and `gcs.Has(h)`
   against a mark set snapshotted **before** the sweep, with no revalidation before
   `ReclaimDeadChunk` + `DeleteSynced`.
2. A dedup hit writes a manifest row and nothing else — `blocksink.go:298`,
   `if c.Data == nil { continue // deduped: manifest row only, nothing to frame }` — so it never
   reaches `MarkSynced` and **never refreshes `syncedAt`**.
3. No unlink clears a marker: every non-test `DeleteSynced` caller is itself a GC or legacy path
   (`blockgc.go:490`, `gc_sweep_index.go:113`, `gc_block.go:133,157`, `legacy_migration.go:208`).
   So an orphaned hash keeps its ORIGINAL `syncedAt`, and `IsSynced` still answers true to the
   write-side dedup oracle.

A new file dedups onto a past-grace orphan; its manifest row is invisible to the already-taken
snapshot; the sweep frees the only durable copy while a live row points at it. Both operations
log success.

**Why this matters beyond one bug.** The journal audit's five HIGHs were state *disagreements at
rest* — two views of the same byte, out of sync. This one is a **time-of-check/time-of-use race**:
every view is individually correct, and the defect is that they are read at different instants
while a writer moves between them. The five-state model in §10.4 is necessary but not sufficient;
it needs **transitions under concurrency**, not just states. Fold that into `piertest`'s state
table — every transition needs a concurrent-writer case, not only a sequential one.

### 13.2 The finding that neither single-package audit could have found

`ComputeObjectID` (`block/objectid.go:36`) folds only `ChunkRef.Hash` into its Merkle root,
never `Size`/`StartOffset`. Since `PruneChunkRefsToSize` deliberately keeps a ref straddling the
new EOF **intact**, truncating a file 8 MiB → 4 MiB recomputes a byte-identical `ObjectID`. Two
files with different content, one identity — the #1872 shape, lifted to whole-file identity.

The reported harm was **refuted, and the refutation is the better finding**. The file-level dedup
short-circuit that would conflate them does not exist:

| layer | state |
|---|---|
| `docs/internals/architecture.md:1357-1391` | describes the feature |
| `metadata/store.go:467` `FindByObjectID` | interface present |
| `shares/coordinator.go:303` `ErrObjectIDConflict` | produced, **never consumed** |
| `storetest/objectid_{lookup,roundtrip}.go` | ~450 lines of conformance tests |
| partial UNIQUE index, both SQL dialects | enforced in schema |
| **`applyFileLevelDedupHit`** | **never written** — named only in comments |

Every supporting layer shipped; the consumer did not. `FindByObjectID` has exactly one non-test
call site, itself an uncalled pass-through. The real consequence is narrow: the live UNIQUE index
turns the collision into an unhandled 23505 on a legitimate truncate.

This is only visible by reading `pkg/block`, `pkg/metadata`, `pkg/controlplane` and the docs
together. Both single-package audits, by construction, saw a coherent fragment.

**Decision needed — the D4 question again:** finish the feature, or delete the scaffolding.
Deleting is consistent with "no production stores, delete eagerly", and removes the UNIQUE index
that is the only thing the collision can currently hurt. Finishing it requires fixing
`ComputeObjectID` first — mixing `Size`/`StartOffset` into the digest and bumping the domain
prefix — which makes it **§11's first real migration customer**.

### 13.3 The uncomfortable conclusion about scope

Engine findings by area, most-hit first: `gc-mark-sweep-compaction` **15**,
`reclaim-reconcile-audit` 10, `write-path-carve` 9, `syncer-upload-health` 9,
`composition-lifecycle` 8, `read-path-cold-fetch` 6, `manifest-check-repair` 4,
`offline-readiness` 3.

Per §6.2, the extraction moves roughly **500 of engine's 11,304 LOC** into ferry — the upload
window, the PUT, and a dead interface. GC, reclaim, manifest check/repair, cold-read resolution
and offline readiness all stay. **The single new HIGH lives in `gc_sweep_index.go`, which does not
move**, and the heaviest-hit area is the one least touched by the refactor.

So the merged result does not fit the three buckets of §3 cleanly, and a fourth is needed:

- **3a. Fix now** — the five journal HIGHs (#2227-#2231), plus the GC resurrection race.
- **3b. Closed by construction** — unchanged.
- **3c. Created by the refactor** — unchanged.
- **3d. Untouched by the refactor.** The majority of engine's findings. The split neither fixes
  nor endangers them; it moves a public API boundary *around* them, after which a residency-truth
  defect in `gc.go` sits on the far side of an interface, harder to see rather than easier.

That is an argument for sequencing, not against the refactor: **§3d work is worth doing before
the boundary hardens**, while the code is still one package and a cross-cutting fix is one diff.

### 13.4 Cross-boundary findings

Three defects span packages, and each was invisible to at least one audit:

1. **The ObjectID/dedup scaffolding** — §13.2. Spans `pkg/block`, `pkg/metadata`,
   `pkg/controlplane`, and the architecture doc.
2. **The GC resurrection race** — §13.1. The dedup oracle is in `engine/blocksink.go`; the marker
   lifecycle is in `pkg/metadata`'s `SyncedHashStore`. Neither half is wrong alone.
3. **DEALLOCATE's two-phase punch** (`pkg/metadata/sparse.go:99`, MED). The manifest prune commits
   durably **before** `blockStore.PunchHole` zeroes the bytes, with no compensating write. Phase
   two failing returns `NFS4ERR_IO` to the client while phase one's damage stands. Spans metadata,
   engine, and the NFSv4 adapter — outside all three audit scopes, found only because the engine
   audit was allowed to grep beyond its own package.

### 13.5 Next actions

1. **Filed as #2238.** Add it to step 0 alongside #2227-#2231. The cheap fix is to re-arm `syncedAt` on a dedup hit, so the existing grace gate covers the
   resurrection window; the correct fix is to revalidate liveness inside the transaction that
   deletes the marker.
2. Decide §13.2: finish the dedup feature or delete its scaffolding.
3. Delete the retired local-GC machinery (see §10.1) — dead since the journal switchover.
4. Triage the 117 LOW findings per D3 — fold into the step that touches the file, sweep the
   remainder at the end of step 3. Note the prior lesson that "duplicated"/"boilerplate"-looking
   LOW findings have twice concealed real drift bugs here; triage by diffing, never in bulk.
5. Re-verify every journal finding before acting: that audit's baseline `ff14b24cb` predates
   `4ec814bc2`.

## 9. Open questions

1. **Who reviews step 3?** It rewrites the silent-zeros path. One reviewer is not enough, and the
   external contributors will not be up to speed in time.
2. **Does step 0 ship as a release?** No production exposure (D4), so this is a judgement call
   about testers' builds rather than an incident response. Cutting one anyway is cheap and gives
   the refactor a clean tagged baseline to diff against.
3. **Does the remote backend honour `If-None-Match`?** No longer blocking after D13 — the fence
   is in the metadata store — but it decides whether the second, independent check exists. One
   conditional PUT against a scratch key settles it; probe rather than assume.
4. **Where does the assembly (§12) live in the tree, and what is it called?** It is small enough
   that "a package" and "a module" are both defensible; the split decision only matters at
   `git subtree split` time.
