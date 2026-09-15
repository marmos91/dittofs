# CI throughput and merge gating — plan

## 1. The job

Cut DittoFS PR CI wall-clock from ~20–30 minutes to ~5–8, **without deleting a single test**, and
make a green CI mean "safe to merge" — which today it does not.

Two problems, one root each:

| Problem | Root cause | Fix shape |
| --- | --- | --- |
| PR CI takes forever | ~44 jobs per push into a ~20-slot free-runner pool; ~85% of wall-clock is queue wait | Delete jobs, not tests |
| Green does not mean safe | No required status checks and 0 required approvals in either ruleset | Add a gate |

These are independent. The second is the more important one and costs no CI time.

## 2. The evidence

Measured against the GitHub API, not inferred.

### 2.1 Queue starvation is the whole story

Lint run `34976565728`: all 10 jobs created at `13:40:52`. Longest job 2m17s. Run wall 1254s (21 min).

| job | started | duration |
| --- | --- | --- |
| Commit Type Matches Diff | 13:44:44 | 8s |
| New Package Tests | 13:48:07 | 7s |
| golangci-lint | 13:57:00 | 2m17s |
| Format & Vet | 13:59:02 | 1m33s |
| ShellCheck | 14:01:32 | 12s |

Conformance run `34979004304` shows the same shape harder:

| job | duration | queue wait |
| --- | --- | --- |
| Grading logic | 10s | **997s** |
| Resolve suite matrix | 5s | **901s** |
| Build Binaries | 124s | **805s** |
| NFS v3 / memory | 368s | **767s** |

A ten-second grader waits sixteen minutes for a runner.

Cause: public repo, free standard runners, shared concurrency pool. One PR push creates ~44 jobs
(Lint 10, Conformance 18, NFS Conformance 8, **CodeQL 3** — its `analyze` job has a three-entry
matrix `[actions, go, python]` — Unit 1, Integration 1, Windows 1, Secret Scan 1, CI Health 1).
Nine open PRs plus every `develop` push draw from the same pool. The pool cannot drain that, so
latency is set by scheduling, not by test execution.

**Consequence for the plan: parallelising the unit suite across a matrix would make this worse.**
The fix is fewer jobs.

### 2.2 Green is not enforced

```
ruleset "Protect develop": deletion, copilot_code_review, code_quality(warnings),
                          required_signatures, pull_request(reviews=0)
required_status_checks:   ABSENT
required_approving_review_count: 0
```

Nothing stops a merge with 44 red checks, and nothing requires a green one to be read. Every
question about "does green mean safe" is currently moot.

### 2.3 Windows Build is largely a duplicate, but `-short` prunes the *expensive* tests

`windows-build.yml:59` runs `go test -short -race -p 2 -v ./...`.

- `pkg`/`internal`/`cmd` contain **1099** test files.
- `-short` guards **13** call sites in that tree, all `if testing.Short()` (19 `testing.Short()`
  references total, 6 of them negations or bench guards).
- The rest of the **148** repo-wide references (73 files) live in `test/e2e` (**119** references
  across 52 files) and `test/integration`, which the
  untagged `./...` run does not compile.

So Windows re-runs ~1086 test files that Linux already ran, race-enabled, on the most contended and
most expensive runner. Avg wall 1477s. The two `go build` steps are the part that is genuinely
Windows-specific.

**Caveat that cuts the other way:** the 13 guards are not evenly spread — they sit in exactly the
costly places (`pkg/snapshot/backup_bench_test.go` ×5, `pkg/controlplane/runtime/snapshot_concurrency_test.go`
×4, `pkg/block/engine` soak/GC, `pkg/metadata/parent_mtime_contention_test.go`). `-short` may
already be pruning the most expensive tests, so the Windows job could be cheaper than the file count
suggests. **Measure the job's actual test-step duration before acting on 1.4** — the saving may be
smaller than the wall-clock average implies.

### 2.4 Duplicate grader execution

`test/conformance/run_test.sh` and `test/common/known-failures_test.sh` run in **both**
`conformance.yml:123-126` and `lint.yml:75-78`. The reviewer's recommendation is to drop the two
steps from `lint.yml` and keep `check-docs.sh` there, since `check-docs.sh` is the docs-drift guard
that `conformance.yml` (docs-ignored) structurally cannot provide. That inverts 1.3 below.

### 2.5 Duplicate build + scaffolding

`conformance.yml` "Build Binaries" and `nfs-pynfs.yml` "Build Binaries" both nix-build `dfs` +
`dfsctl` and both upload an artifact named `dfs-binaries`. `conformance.yml:83` computes a `pynfs`
matrix output it never uses — dead. `nfs-pynfs.yml` re-resolves the same `suites.json` with its own
matrix/grading/baseline jobs. The `conformance.yml` header already calls the merged layout the
intended end state; the wiring is half-done.

Note the cells are **not** duplicated: pynfs runs only in `nfs-pynfs.yml`. What is duplicated is the
scaffolding around them.

### 2.6 Dead weight and missing guards

- `unit-tests.yml` uploads artifact `dfs-1.26.x`; nothing downloads it. Every `download-artifact`
  step in the repo reads `dfs-binaries`. Dead upload.
- `unit-tests.yml:30` and `integration-tests.yml:35` carry a **single-entry** `matrix: go-version`
  (`["1.26.x"]`) that expands to one job. Cosmetic, but it is what makes the required-check name
  `Unit Tests (1.26.x)` instead of `Unit Tests`.
- **`lint.yml` has zero `timeout-minutes` on any of its 10 jobs** (verified: `grep -c` returns 0).
  Every other workflow caps its jobs; Lint does not, so a wedged job inherits the 6-hour default and
  holds a runner slot for the rest of the day. This is precisely the failure `integration-tests.yml`
  documents adding its 45-min cap to avoid, and it is *worse* here because Lint is 10 jobs.

### 2.7 Correction to a plausible-looking claim, and then a correction to that correction

`actions/setup-go` with `cache: true` caches **both** `GOMODCACHE` and `GOCACHE` ("built-in caching
and restoration for Go modules and build outputs"). All 17 jobs already use it.

An earlier draft concluded from this that build-cache sharing is "already done and not an item in
this plan". That is true as written and **misleading in effect**. It is cached into *one*
never-refreshed, first-writer-wins entry. See §2.8.

### 2.8 The Go build cache is paid for and never received

Read from the Actions cache API (`/actions/cache/usage` and a full paged pull of `/actions/caches`).
Two independent investigations reached this from opposite ends; their entry counts differ by a few
hundred because the cache was churning between the two pulls, which is itself the finding.

**(a) The repo is over GitHub's 10 GB per-repo cache cap, and Nix NARs are 99.9% of the entries.**

| | |
| --- | --- |
| Active cache | ~6,100-6,700 entries, **~10.8 GB** against a **10 GB ceiling** |
| magic-nix-cache NAR + narinfo | **~9.2-9.6 GB** |
| Distinct Nix store objects behind them | **499** — so **~95% is duplicate** |
| Worst single object | one 85 MB store path held **56 times = 4.65 GB = 46% of the entire budget** |
| Everything else | `setup-go` 3 entries / ~0.9 GB, codeql 3 / 0.2 GB, gitleaks 1 / 5 MB |

*(Entry count genuinely moves between pulls — the usage API returned 6,140 entries / 10.884 GB while
a full paged pull minutes later returned 6,505 / 10.84 GB, so a few-hundred disagreement is churn,
not error. **Size is not in that situation.** ~10.8 GB is corroborated by two independent routes; a
12.2 GB figure from one pass is not reproducible and is treated here as wrong rather than as the top
of a range. Distinct store objects measured 493-499, also churn. "~95% duplicate" is correct by
**bytes** — 9.69 GB held for 0.49 GB unique — and should be quoted that way; deriving it from the
entry count uses the wrong denominator and merely happens to land on the same answer.)*

Eviction is not theoretical. Every NAR entry was last accessed inside a **~1 hour window**
(oldest 14:40Z when queried at 15:46Z; a second pull saw a 49-minute window). Anything not read
hourly is already gone. Entry counts moved 5,782 → 6,041 → 6,654 → 6,700 during a single session.

Two multipliers produce the 56 copies: GHA per-branch cache scoping (7 distinct refs — unavoidable)
**and** within-scope duplication. **The duplication is not on `develop` — it is on long-lived PR
merge refs**, which changes both the mechanism and who pays for it:

| ref | size | entries | max copies of one object |
| --- | ---: | ---: | ---: |
| `refs/pull/2589/merge` | 2.86 GB | 1,741 | **25** |
| `refs/pull/2598/merge` | 1.85 GB | 1,257 | 14 |
| `refs/heads/develop` | 1.83 GB | 742 | 4 |

`develop` holds one copy of the worst object and at most four of any object. A "concurrent uploaders
per push" story predicts an even spread across refs; the data shows **accumulation over a PR's
lifetime** (25 push-generations retained on one ref). The fix shape is unchanged, but the budget is
proportional to **open PR count** (10 today), which means **any** per-PR cache re-creates it —
including Phase 0.2. Size 0.2 against open-PR count, not against one run.

Upload concurrency still contributes, because concurrent
Nix jobs all miss simultaneously and all upload under distinct random key suffixes.
`magic-nix-cache-action@main` appears in **7 job definitions** across `conformance.yml`,
`nfs-pynfs.yml` and `e2e-tests.yml`, several of them matrices — roughly 11 uploaders per push.

Direct wall-clock cost is **~0** (`Install Nix` 8-10s, `Setup Nix cache` 9-11s). The damage is
entirely indirect: it leaves no budget for any other cache. This is why it is Phase 0 and not a
Phase 3 nicety.

**(b) `setup-go`'s cache is frozen at 10 MB and cannot self-heal.**

```
480 MB  setup-go-Linux-x64-ubuntu24-go-1.26.0-f90f2b60...
 10 MB  setup-go-Linux-x64-ubuntu24-go-1.26.8-f90f2b60...   <- what "1.26.x" resolves to today
412 MB  setup-go-Windows-x64-go-1.26.8-6310ae1a...
```

The key is `setup-go-${os}-${arch}-go-${resolved-version}-${sha256(go.sum)}` with **no run/SHA
component and no `restore-keys`**, and `actions/cache` keys are write-once. Worse, a primary-key
hit makes the action **skip the save** — visible in the `Format & Vet` log:

```
Cache Size: ~10 MB (10562882 B)
Cache hit occurred on the primary key setup-go-Linux-x64-ubuntu24-go-1.26.8-f90f2b60..., not saving cache.
```

So the first job to finish pins the cache that every later job — and every later run — inherits,
for the whole life of (Go version × `go.sum` hash). For scale: a full `GOCACHE` for this repo
measured **4.3 GB** after one complete unit run, and **704 MB** after a plain `go build`. 10 MB is
module metadata with no build cache in it at all.

**The trigger is broader than the `go.sum` bump this repo already knows about.** The hosted runner
image bumped Go **1.26.0 → 1.26.8** at 17:35Z on 2026-09-14, minting a fresh key with **no repo
change whatsoever**. Note also the split: `go-version: "1.26.x"` resolves to 1.26.8 (the ~8 Linux
jobs, 10 MB) while `go-version-file: go.mod` resolves to 1.26.0 (operator/CodeQL, 480 MB) — the
healthy cache is attached to the jobs that need it least.

**What a warm cache is worth — and the honest tension between two measurements.**

| measurement | result |
| --- | --- |
| Local: `go test ./pkg/... ./internal/... ./cmd/...` cold → warm | **342s → 32s**, 140/142 packages `(cached)` |
| Local: `go build ./pkg/... ./internal/... ./cmd/...` cold → warm | 109.9s → 8.1s |
| CI natural experiment across the 17:35Z flip (same `go.sum`, 503 MB entry vs 10 MB entry) | **no signal**: `go vet` 90s → 82s, golangci 109s → 127s, Unit Tests 766s → 867s (overlapping ranges) |

These are not in conflict; they measure different things. The CI experiment compared an inadequate
cache against a more inadequate one — a single ~500 MB entry shared by `go vet`, golangci,
`-race -covermode=atomic` tests and plain builds cannot hold four build configurations, because
GOCACHE keys include `-race` and `-cover`. **The 342s → 32s figure is a ceiling from a same-machine,
no-change re-run, not a CI projection.** What CI would see is bounded by per-commit invalidation
fan-out, measured in §2.9.

**Correction both investigations landed on independently:** `-coverprofile` / `-covermode=atomic`
do **not** defeat Go's test cache on Go 1.26. A second run prints `ok ... (cached) coverage: 82.5%
of statements`. Both expected otherwise. `unit-tests.yml`'s coverage flags are not the problem.

### 2.9 Where the runner-seconds actually are, and what that rules out

Summed from the jobs API (`completed_at - started_at`, skipped jobs excluded) for a representative
**successful** PR run of each workflow:

| Workflow | jobs | runner-sec | hermetically sandboxable? |
| --- | ---: | ---: | --- |
| Conformance Suites | 16 | **7,329** | No — docker-compose, `sudo` mounts, smbtorture/pjdfstest/wpts binaries |
| NFS Protocol Conformance | 7 | **2,868** | No — pynfs via docker, kernel NFS client |
| Windows Build | 1 | **1,848** | Yes, with a second toolchain |
| Unit Tests | 1 | **1,055** | **Yes** |
| Integration Tests | 1 | 454 | No — live postgres service, docker AD-DC/KDC |
| Lint | 9 | 344 | Partly (**282s** of Go vet/lint; rest is shellcheck + docs + jq) |
| **Total** | **35** | **13,898** (~232 runner-min) | |

**~3,185s / ~23% of runner-seconds, across 3 of 35 jobs, is Go compilation and testing** — ~24% if
CodeQL's `Analyze (go)` leg (210s) is counted. The other ~76% is shell, `sudo`, kernel mounts,
docker-compose and foreign test binaries.

> **This table was wrong in an earlier version of this document and the error mattered.** It carried
> Windows Build at 667s and Unit Tests at 390s, which made the Go share look like ~10%. 667s was a
> run from **2026-09-04**; the four most recent Windows runs are 1848/1748/1322/667s. 390s matched
> **no run at all** — Unit Tests measures 1002-1055s today. The table had silently mixed measurement
> epochs: the Lint row was from the current day and reproduced to the second, the two Go rows were
> not. Verified independently against `actions/runs/<id>/jobs` before correcting.
>
> **The conclusion survives; its margin does not.** §3 rejects Bazel partly on a revisit trigger of
> "`go test` share of runner-seconds exceeds ~50%". At the corrected ~23-24% that trigger is 2.4x
> closer than the retracted figure implied. Anyone re-opening the build-system question should
> re-measure this table first rather than citing it, and should take **N samples**, not one — see
> §6 on bursty queue depth. **Do not quote a single-run number from this document as settled.**

**Per-commit invalidation fan-out** (reverse-dependency closure over the last 30 first-parent
`develop` commits, each selected package weighted by its measured test elapsed time):

- **15 of 30** commits touch a non-Go file and must fall back to running everything, under the
  literal wording of that rule. (An earlier draft said 5, which matches none of the plausible
  definitions: 15/30 touch any non-Go file, 7/30 touch a non-Go non-doc file, 3/30 contain zero
  `.go` files. State the definition whenever this is re-measured.) **This matters because the
  medians below are computed over the remainder** — with half the commits at 100% fallback, the
  median over *all* 30 sits at the fallback boundary, not at 6.8%.
- The other 25 select a median of **28 of 179** packages (min 5, max 109).
- Time-weighted: **median 6.8%** of suite time, **mean 21%**, max 92.1%.

**The graph is a funnel, and the funnel is where the work is.** `pkg/block/engine` (165.0s) and
`pkg/block/journal` (151.3s) are together **52% of unit-test time**, and both sit downstream of
`pkg/block` (fan-in 102). Block dataflow is the active work area, so **the commits that most need a
fast gate are the ones any dependency-based technique helps least**: two journal commits each
selected 69.9% of suite time, one selected 92.1%. A warm build cache is a big win on a median
commit and a modest one on the commits actually being pushed right now. Say that, do not average it.

## 3. What is NOT wrong (do not touch)

- **E2E off the PR path.** 45–75 min, correctly push + nightly.
- **`smb-client-compat` push-only + weekly, path-scoped.** Correct.
- **`suites.json` as the single manifest with per-event tiers.** Good design; the reason
  conformance tiering is already sane.
- **`cancel-in-progress: true` everywhere.** Correct.
- **`required-tests.json` + `commit-type-matches-diff`.** Both exist because a real incident shipped
  (83e532d78, a `docs:`-titled commit that reverted security fixes and went green). High value.
- **`golangci-lint skip-cache: true`.** Documents a real SA5011 false positive; 20s well spent.
- **`-p 1` in integration tests.** Forced by a shared postgres DB. Removing it causes races.
- **Test breadth.** 1,195 tracked test files (1,099 under `pkg`/`internal`/`cmd`), POSIX/WPTS/smbtorture
  conformance, E2E. That is the asset. Nothing here deletes coverage. *(An earlier draft said 4475;
  that figure counted `.claude/worktrees/`. See §10.)*
- **The build system.** Bazel/Pants/Please/Nx were considered and **rejected**. Per §2.9, only ~10%
  of runner-seconds and 3 of 35 jobs are hermetically sandboxable; the other 88% is shell, `sudo`,
  kernel mounts and docker-compose that Bazel could only wrap as `tags = ["manual", "no-sandbox"]`
  and shell out to — what the workflows already do, plus a BUILD file. It removes **zero** jobs and
  keeping BUILD files fresh **adds one**, into the ~20-slot pool §2.1 identifies as the bottleneck.
  Nix already occupies the hermetic-build slot (packages, `devShells.ci`, `checks`, the `vendorHash`
  bot, and a remote binary cache). Adopting Bazel means maintaining two. Measured mitigating detail:
  **0 files import "C"** and sqlite is pure Go (`modernc`/`glebarez`), so the genuinely nasty part of
  `rules_go` would not apply — it still is not worth an estimated 2-4 engineer-weeks plus a monthly
  remote-cache bill against a pipeline that currently costs **$0**. Revisit only if Go packages
  exceed ~600 (now **179**), modules exceed ~5 (now 2), the `go test` share of runner-seconds exceeds
  ~50% (now **~10%**), CI spend exceeds ~$300/mo, or Phase 0 lands, verifies as restoring, and Unit
  Tests *still* exceeds ~5 min. Nx is a JS/TS tool, Pants' Go backend is experimental, Please is
  unmaintained relative to Bazel: the choice is Bazel or nothing, and today it is nothing.
  Nothing here deletes coverage.

## 4. The change set

**Phase 2 can be done first, and there is a good argument that it should be.** §1 calls the missing
gate the more important of the two problems, and it costs **zero CI time**. The "consolidate first"
constraint below exists only because merging jobs renames checks — and it **dissolves entirely if
Phase 2 requires stable aggregator names**, which this plan recommends anyway for the path-filter
reason. A reader who implements only Phase 2 captures the item this document itself ranks first.
The ordering below is a convenience, not a dependency.

Ordered. **Consolidate before wiring required checks** — merging jobs changes check names, so
configuring the gate first means configuring it against names about to be deleted.

### Phase 0 — repair the cache budget (prerequisite, attacks execution not queue)

Ordered first because Phases 0.2 and 0.3 need ~1-4 GB of cache budget per run and §2.8 shows there
is currently none: the repo is over the 10 GB cap with a ~1 hour eviction horizon. Adding a Go build
cache before fixing the Nix duplication just adds another entry to the churn.

| # | Change | Saves | Confidence |
| --- | --- | --- | --- |
| 0.1 | **Get the Nix NAR store out of the Actions cache.** Either point it at Cachix / an S3 bucket (the repo already has S3 credential machinery), or drop `magic-nix-cache-action` from the jobs that only enter a dev shell — `conformance.yml:148` and `nfs-pynfs.yml:119` run `nix develop .#ci --command go build` and want a toolchain, not a 9 GB NAR store. Only the `pjdfstest`/`pynfs` derivation builds genuinely benefit. | frees **~9 GB** of a 10 GB budget; ~0s directly | Medium — the consuming jobs' Nix work is 12-23s/cell, so a bad outcome costs ~10-20s per cell. **Drop it from one matrix job and measure before doing all of them.** |
| 0.2 | **Add an explicit rolling `~/.cache/go-build` cache** to every job running `go build`/`go test`/`go vet`, keyed per job and per commit with a `restore-keys` fallback. Do not rely on `setup-go`'s built-in cache for build objects — §2.8(b) shows why it cannot work. | bounded above by 342s → 32s locally; realistically a median-commit win per §2.9 fan-out | Medium — ceiling is measured, CI delivery is not |
| 0.3 | **Verify it lands.** One step, no guessing. | — | None — pure observation |

```yaml
# 0.2 — in every job that runs go build / go test / go vet.
# setup-go's own key is shared across all Go jobs and is write-once, so the first
# job to finish pins a cache every other job then inherits (§2.8b). Key the build
# cache per job and per commit, and fall back to the newest one.
- uses: actions/cache@v4
  with:
    path: ~/.cache/go-build
    key: gocache-${{ runner.os }}-${{ github.job }}-${{ github.sha }}
    restore-keys: gocache-${{ runner.os }}-${{ github.job }}-
```

```yaml
# 0.3 — a cache you did not watch restore is a cache you do not have.
# The FIRST go test must print (cached), straight off the restore, with nothing
# run before it. Assert on it: a probe that cannot fail verifies nothing.
- run: |
    size=$(du -sm "$(go env GOCACHE)" 2>/dev/null | cut -f1 || echo 0)
    echo "restored GOCACHE: ${size} MB"
    [ "${size:-0}" -ge 200 ] || { echo "::error::GOCACHE restored at ${size} MB - cache is not landing"; exit 1; }
    go test ./pkg/block/journal/ 2>&1 | tee /tmp/probe.txt
    grep -q '(cached)' /tmp/probe.txt || { echo "::error::no cached result off a restored cache"; exit 1; }
```

> **An earlier version of this probe could not fail, and it was the document's designated
> falsifier.** It ran `go test -count=1 ./pkg/block/journal/` and *then* `go test
> ./pkg/block/journal/`, expecting `(cached)`. But `-count=1` **populates** the test cache, so the
> second invocation prints `(cached)` on a completely cold `GOCACHE` — demonstrated directly on a
> wiped cache. Had it shipped, Phase 0 would have been recorded as verified-restoring while nothing
> restored, and §3's "Phase 0 lands, verifies as restoring, and Unit Tests *still* exceeds ~5 min"
> revisit trigger would have been permanently disarmed by a tautology. **A verification step whose
> success is unconditional is worse than no verification step**, because it converts an open
> question into a recorded answer.

Fork-PR runs restore from `develop`'s cache and write to a PR-scoped one they cannot leak back —
both the correct security posture and the behaviour wanted here.

**Phase 0 is *not* independent of Phase 1, and an earlier draft claimed it was.** The claim was
made by checking job *names* and *triggers*; the interaction runs through the cache key itself.
0.2 keys on `${{ github.job }}` — the job **id** — and Phase 1.1 merges job definitions, which
changes job ids. Three consequences:

1. Every 0.2 cache key is invalidated the moment Phase 1 lands. One-time cold run; minor.
2. **Persistent, and self-defeating:** 1.1 group (a) merges `format-vet` (`go vet`, no `-race`) with
   `required-tests` (`go test`). §2.8 diagnoses the 500 MB entry's failure as *exactly* this — one
   entry shared by `go vet`, golangci, `-race -covermode=atomic` tests and plain builds cannot hold
   four build configurations, because GOCACHE keys include `-race` and `-cover`. **Phase 1.1 puts
   two build configurations back under one key. Phase 1 undoes Phase 0.2 by construction.**
3. For a matrix job every leg shares one `github.job`, so legs race on one key — first-writer-wins,
   the §2.8(b) failure reproduced.

**Therefore:** key 0.2 on a build-configuration discriminator, not the job id
(`gocache-${{ runner.os }}-race-cover-${{ github.sha }}` for the `-race -cover` test job,
a separate key for plain build/vet), and treat "does the key survive Phase 1" as part of 1.1's
acceptance. Phase 0 remains independent of **Phase 2**; it is not independent of Phase 1.

**Phase 0.2 will re-breach the cap that Phase 0.1 just cleared, unless it is scoped.** A full
`GOCACHE` here is **4.3 GB** after one unit run. One entry per job per commit across ~10 Go jobs
exhausts a 10 GB budget in roughly **two pushes**, and the measured eviction window is ~55 minutes
under repo-wide LRU. 0.1 frees ~9.7 GB; 0.2 as first drafted spends it immediately. Scope 0.2 to
the two or three jobs whose duration actually justifies a multi-GB round trip — Unit Tests first —
and measure the upload/download cost against the job it is meant to shorten before extending it.
Nothing in this plan yet estimates that round-trip cost.

### Phase 1 — delete jobs (attacks the dominant cause)

| # | Change | Saves | Confidence |
| --- | --- | --- | --- |
| 1.1 | Lint: 10 jobs → **5 job definitions, 4 active on a typical PR**. Group (a) Go-toolchain checks sharing checkout + `setup-go` (`format-vet`, `spec-citations`, `required-tests` — note `spec-citations` runs `go run` and `required-tests` runs `go test`, so the merged job **must** provision Go and its cache), (b) shell/harness (`e2e-harness` + the conditional ShellCheck pair), (c) manifest/docs (`conformance-manifest`, `new-package-tests`, `commit-type`). Keep each check a **named step** so a red job still says which check failed. | 5–6 slots/PR; Lint 21min → ~5min | Slight ↓ in granularity, mitigated by named steps |
| 1.2 | Fold `nfs-pynfs.yml` into `conformance.yml` as a manifest-driven `pynfs` suite cell (complete the half-wired `outputs.pynfs`). One matrix, one Build Binaries, one Nix install. Add `pynfs` to `summary`'s `needs` so the existing aggregator gates it. | **3 jobs/push** (its matrix/grading/build) — the 4 pynfs cells remain; 1 workflow, 1 build, 1 Nix install | Neutral — same 4 cells, same grader, same KNOWN_FAILURES tables |
| 1.3 | Remove the duplicate grader execution. Drop `run_test.sh` + `known-failures_test.sh` from `lint.yml`, keep `check-docs.sh` there; `conformance.yml`'s `graders` keeps the runner tests. | seconds of *duration*, **not a slot** — the `graders` job remains | None — each script still runs exactly once |
| 1.4 | Windows Build: **do not reduce the test scope.** Superseded by measurement — see below. Two corrections stand regardless: the job runs `go build ./cmd/dfs/` + `./cmd/dfsctl/` only and has **no `go vet` step at all**, so the "keep `go build ./...` + `go vet ./...`" mitigation in 5.1 names steps that would have to be **added**, not kept; and `go build` does not compile `_test.go`, so it is not the compile-coverage backstop 5.1 assumes. | **0** — withdrawn | N/A |

**1.4 is withdrawn, with evidence.** The §2.3 caveat said "measure the test step before acting".
Measured, per-package, comparing the Windows job (`-short -race -p2`) to the Linux job
(`-race`, no `-short`):

| package | Linux (full) | Windows (`-short`) | change |
| --- | ---: | ---: | ---: |
| `pkg/block/chunker` | 583s | **15s** | -97% |
| `pkg/block/engine` | 673s | 220s | -67% |
| `pkg/controlplane/runtime` | 206s | 119s | -42% |
| `pkg/metadata/store/badger` | 341s | 297s | -13% (conformance suite, no guard) |
| `pkg/metadata/store/sqlite` | 274s | 285s | ~flat |
| `internal/adapter/nfs/v3/handlers` | 170s | 252s | **+48%** (Windows/`-p2` overhead) |
| `pkg/block/journal` | 11s | **110s** | **10x** |
| `pkg/controlplane/runtime/shares` | 7s | **106s** | **15x** |

`-short` is already doing the cheap part of the pruning (chunker -97%). The two packages it does
*not* help — `journal` and `runtime/shares` — got **10x and 15x slower on Windows**, which is real
platform I/O signal, not padding. Both are **cross-platform packages with no `_windows.go` in the
name**, so the windows-tagged-file list 1.4 proposed would have dropped exactly them. Step-level:
`Run unit tests` is 1406s of the 1528s job (92%); the builds are 37s. The remaining saving does not
offset masking a genuine Windows I/O regression. **Leave Windows Build as it is.**
| 1.4b | **Restrict the CodeQL `pull_request` matrix to `go`.** `codeql.yml:33` runs `language: [actions, go, python]` unfiltered on every PR. The repo has 5 tracked Python files, none of them shipped product code (4 under `test/`, plus `scripts/gen-bench-charts.py`), and the workflow already runs weekly (`cron: 27 3 * * 1`) and on push-to-develop, so full-language coverage still lands on the merge path. | **2 slots**, one line | None — coverage retained on push + schedule |
| 1.5 | Add `timeout-minutes: 10` to every Lint job. | prevents a 6h slot hold | None — pure safety |
| 1.6 | Delete the dead `dfs-1.26.x` artifact upload and the single-entry `go-version` matrices. | seconds, clearer check names | None |

**Expected: ~44 → ~26 jobs per push** (slots), *plus* large duration reductions that do not change
the count. The two figures must be tracked separately — several items above save wall-clock without
freeing a slot, and conflating them is how the projection gets overstated:

| | before | after |
| --- | --- | --- |
| Job slots per push | ~44 | ~26 |
| Queue term on a 20-slot pool | ~18 min | near-zero |

Earlier drafts of this plan said "42 → 22" and credited 8 slots to the `nfs-pynfs` merge and 1 slot to
1.4. Both were wrong: the merge removes 3 scaffolding jobs (the 4 cells stay), and 1.4 shortens a job
rather than deleting one unless the job is explicitly split. The corrected numbers are above.

### Phase 1b — scope integration tests (confidence-neutral, do not hand-write the list)

`integration-tests.yml:112` runs `-tags=integration ./...` under `-p 1`, which builds and serializes
every package in the module including the 172 with no integration-tagged file.

**A naive `go list -tags=integration` filter does not work** — I tested it: `-tags=integration`
*includes* tagged files rather than selecting packages that have them, so
`{{len .TestGoFiles}}` matches 143 of 179 packages and narrows almost nothing. The tag-sensitive
set is the **difference** between the two tag states:

```bash
comm -13 \
  <(go list -f '{{.ImportPath}} {{join .TestGoFiles " "}} {{join .XTestGoFiles " "}}' ./... | sort) \
  <(go list -tags=integration -f '{{.ImportPath}} {{join .TestGoFiles " "}} {{join .XTestGoFiles " "}}' ./... | sort) \
  | awk '{print $1}' | sort -u
```

Verified output: **7 packages** (`internal/controlplane/api/handlers`, `pkg/block/remote/s3`,
`pkg/controlplane/runtime`, `pkg/controlplane/store`, `pkg/metadata/store/badger`,
`pkg/metadata/store/metabench`, `pkg/metadata/store/postgres`) — down from 143. Run those, plus the
untagged `test/integration/...` packages, and `-p 1`.

The list must be **derived at runtime, never hand-written** — a hardcoded list silently stops running
a new integration file in a package nobody remembered to add. That is the one real implementation
trap here; the diff-based derivation avoids it by construction.

### Phase 2 — adopt the gate (attacks the confidence gap)

1. **Add required status checks** to the `Protect develop` ruleset. Split by what is safe today:

   **Tier 1 — require now, zero plumbing.** These workflows trigger on every PR with no path
   filter, so the check always reports:
   - `golangci-lint`
   - `Format & Vet`
   - `gitleaks`
   - `Commit Type Matches Diff`

   **Tier 2 — require only after the aggregator fix below:** `Unit Tests`, `Integration Tests`,
   and the conformance suite.

   **Ordering hazard: Tier 1 as written names two checks that Phase 1.1 deletes.** 1.1 folds
   `format-vet` into group (a) and `commit-type` into group (c) as *named steps*, so `Format & Vet`
   and `Commit Type Matches Diff` stop being reported check names the moment Phase 1 lands. Requiring
   them first leaves every PR at "Expected — waiting" on a check that no longer exists, and the only
   way out is the admin bypass this section warns against three paragraphs down. Either require the
   post-consolidation job names, or — better, and the reason the aggregator pattern below matters —
   require **stable aggregator names** that outlive any regrouping. This is the same "consolidate
   before wiring" rule stated at the head of §4, applied to §4's own tier list; the tiers were
   written before 1.1 existed and were never re-read against it.

   **`Required Tests` belongs in the required set and is currently in no tier at all.** §3 calls
   `required-tests.json` high value because 83e532d78 shipped through its absence. As written,
   deleting a pinned authorization or encryption test fails only an *unrequired* check — the gate
   would not gate the one thing it was built for. **But it cannot go in Tier 1 as "zero plumbing":
   `required-tests` is in Phase 1.1 group (a), so its check name disappears for the same reason
   `Format & Vet` does.** It belongs in whichever naming scheme is chosen below, and this is the
   third check in a four-name tier that the consolidation deletes — which is the real argument for
   picking stable aggregator names rather than patching the tier list item by item.

   **Decide the naming scheme before either phase lands, because the tiers cannot be salvaged
   piecemeal.** After Phase 1, the only Tier 1 names that still exist are `golangci-lint` and
   `gitleaks` (verified: `lint.yml` and `secret-scan.yml` carry no path filter, so both always
   report). Pick one: require post-consolidation job names, or require stable aggregator names.
   This plan recommends **aggregator names**, because they also solve the path-filter problem below
   and they make Phase 2 orderable before Phase 1 — see the note at the head of §4.

   **`Conformance summary` is NOT currently a safe gate.** Its `needs:` is only
   `[wpts, smbtorture, pjdfstest, nfs-kerberos]` — it omits `matrix`, `graders`, and `build`. Worse,
   `wpts` and `smbtorture` `need:` only `matrix`, **not `build`**, so a failed `Build Binaries`
   leaves them to run against a missing artifact while `summary` reports green. A failed resolver or
   grader likewise never reaches the summary. Do not require it until it `needs:` every terminal job
   and fails on any non-success. This is a false-green path in the *existing* workflow, independent
   of this plan — worth fixing regardless of whether the gate is ever enabled.

   **Do not require coverage.** `codecov` is `continue-on-error: true` and should stay that way — a
   required coverage number invites gaming.

   **Blocker to solve first: path filters make a required check deadlock a docs-only PR.**
   `unit-tests.yml` and `integration-tests.yml` are `paths:`-filtered to Go files, and
   `conformance.yml` is `paths-ignore:`-filtered for `**.md` / `docs/**`. A **workflow** that does
   not trigger produces **no check at all**, and GitHub leaves a required check "Expected — waiting"
   forever. (Contrast: a *job* skipped by `if:` reports `skipped`, which does **not** block.)

   So marking `Unit Tests` required while its `paths:` filter stands means a documentation-only PR
   can never merge. The maintainer merges with an admin bypass, bypassing becomes routine, and the
   gate stops being a gate.

   This is not hypothetical: 83e532d78 was a `docs:`-titled commit pushed straight to `develop` —
   the exact change class that hits this. The gate would have to be bypassed on precisely the PRs
   that `commit-type-matches-diff` exists to police.

   Fix, and the first draft of this fix was **not constructible** — stating both so nobody
   re-derives the broken one. An always-triggering aggregator that `needs:` the path-filtered jobs
   cannot work while the filter sits on the workflow's `on:` block: on a docs-only PR the **whole
   workflow** does not trigger, so no job runs, aggregator included, and `needs:` cannot reach across
   workflows. The filter has to move before an aggregator means anything.

   So: **move the path filter from the workflow to the jobs.** Drop `paths:` / `paths-ignore:` from
   `on:` in `unit-tests.yml`, `integration-tests.yml` and `conformance.yml`, add a cheap
   `changes` job using `dorny/paths-filter`, and gate the expensive jobs on its output with `if:`.
   A job skipped by `if:` reports `skipped`, which **does not block** — that is the whole mechanism.
   Then add the always-running aggregator with `if: always()` that fails when any dependency
   failed, and require **that** name.

   **This pattern already exists in this repo — extend it, do not invent it.** `lint.yml:204`
   (`shellcheck-changes` → `shellcheck`) and `conformance.yml:425` (kerberos scoping) both already
   use `dorny/paths-filter@v4` at job level, and `lint.yml` already carries the `fetch-depth: 0`
   comment documenting the merge-base pitfall. Keeping the required check *name* stable while the
   jobs behind it change is what lets Phase 1 and Phase 2 be done in either order safely.
2. **Enable a merge queue on `develop`. Do *not* enable one on `main`.** `Protect main` has no
   `pull_request` rule and nothing reaches `main` by PR: per CLAUDE.md the release flow is
   `git branch -f main <commit> && git push origin main:main`. A required merge queue on `main`
   blocks that push outright, so every release would either fail or depend permanently on the admin
   bypass this section is trying to eliminate. Gate `develop`, where the PRs actually are, and leave
   `main`'s fast-forward release path alone.

   Adding `merge_group:` to the `on:` blocks is necessary but **not sufficient** — two existing
   payload assumptions break under it:

   - **`conformance.yml` selects the wrong tier.** Its `EVENT` expression maps only `pull_request`
     and `schedule`, falling through to `'push'` for anything else. `suites.json` gives `push` the
     **full** `all` matrix, so a merge group would run postsubmit-sized conformance — more jobs than
     the PR path it replaces, defeating the point. Map `merge_group` explicitly to the PR tier.
   - **`commit-type-matches-diff` can false-green.** Its non-PR branch reads `github.event.before`,
     which a `merge_group` payload does not carry (`merge_group` uses `base_sha`/`head_sha`). The
     value is empty, the script hits its "no base commit for this event; nothing to check" success
     path, and the required check passes without inspecting anything. A required check that silently
     does nothing is worse than no check — extend the `BASE` expression for `merge_group`.

   Audit every workflow that gains `merge_group:` for this class of bug: an event-name ternary with
   no `merge_group` arm, or a payload field that only exists on `pull_request`/`push`.
3. **Raise `required_approving_review_count` 0 → 1.**

   *(An earlier draft carried a fourth item, "apply the same settings to `Protect main`". It is
   deleted: it contradicts item 2 above. Nothing reaches `main` by PR, so there is no PR gate to
   apply there, and ruleset required checks would block the `git push origin main:main`
   fast-forward exactly as a merge queue would. `main`'s protection is that it only ever
   fast-forwards to a commit that already passed the gate on `develop`.)*

Why this is the confidence story: required checks make green *necessary*; the merge queue makes the
tested artifact the *merge result* rather than each PR against a stale base; `required-tests.json`
already pins the specific authorization/encryption gates whose deletion is otherwise silent. None of
it needs new test code.

### Phase 3 — cheap per-job wins

| # | Change | Saves |
| --- | --- | --- |
| 3.1 | Drop `-v` from `unit-tests.yml:50`. Thousands of per-test log lines on the most contended job. Keep it on failure via `-json` or a rerun if needed. | ~10–20% of unit job |
| 3.2 | ~~Keep `-race` on Linux only.~~ **Struck.** It contradicted 1.4 and 5.1, which kept `-race` on the reduced Windows set as the mitigation; with 1.4 withdrawn the premise ("already the case after 1.4") is gone too. Windows keeps `-short -race` exactly as today. | — |
| 3.3 | **Add `-short` to `unit-tests.yml` on the `pull_request` path only; keep the full suite on push-to-develop.** The Linux unit job passes no `-short`; `windows-build.yml:59` does. The guards already exist (`chunker_test.go:78`, `boundary_stability_test.go:69`) and simply never fire on the job that needs them, which is why `pkg/block/chunker` is 583s on Linux and 15s on Windows. **The event scoping is not optional — see the trap below.** | **~460-500s** of the unit job's test step on PRs (estimated: no A/B run with `-short` added) |
| 3.4 | Add guards to three large ungated tests: `TestChunker_MinDrivesEffectiveAverage` (72s), `TestChunker_ConstantMemory` (21s), `TestWarmReadIntegrity_AfterDrainUploads` (139s). Single `if testing.Short() { t.Skip(...) }` each, matching the pattern in their own files' siblings. | a further ~230s, taking the step to ~74% off (estimated) |
| 3.5 | Add `gotestsum` to `flake.nix` and use `--format pkgname`. Measured absent from `.github/`, `flake.nix` and the Makefile. It gives per-package progress without per-test spam, which **dissolves the 3.1 tension** between dropping `-v` and keeping failures diagnosable — take this instead of 3.1, not as well as. | makes 3.1 safe rather than a trade |

**Trap: 3.3, 3.4 and Phase 1b interact, and the interaction is invisible from any one of them.**
The fact that makes this subtle is never stated elsewhere in this document:
`integration-tests.yml:112` runs `go test -tags=integration -v -timeout=20m -p 1 ./...` with **no
`-short`**, and because `-tags=integration` *adds* files rather than selecting packages (Phase 1b's
own finding), that command **re-runs the entire unit suite, un-`-short`'d, a second time**. That is
today's real safety net under `-short`: it is where the full-scale chunker property tests, the
engine soak and GC-state tests, and `TestWarmReadIntegrity_AfterDrainUploads` would still execute.

**Phase 1b narrows that job to 7 packages and removes the net.** So 1b is not the
"confidence-neutral, pure waste removal" item it is billed as *once 3.3 or 3.4 lands* — the
compile waste it removes is real, but the same change also deletes an un-`-short`'d execution of
every other package. The two items are individually correct and jointly lossy. Land them together
and say so, or land 1b and keep one un-`-short`'d full run somewhere.

**Trap, restated concretely:** If `-short` is applied unconditionally,
the full-scale chunker property tests run **nowhere on a PR**: `windows-build.yml:59` already passes
`-short`, and the only other full execution is `integration-tests.yml`'s `go test -tags=integration
./...`, which Phase 1b scopes down to 7 packages. A plan headlined "without deleting a single test"
would delete one, silently, as an emergent property of two items that are individually correct.
Hence the `pull_request`-only scoping in 3.3, and hence the rule: **whenever an item narrows what a
job runs, name the job that still runs the full set.** For 3.3 that is push-to-develop, within
minutes of merge rather than nightly.

**The `-short` tier has a known ceiling, and it is not "more guards".** Repo-wide there are 19
`testing.Short()` call sites in 12 files (matching §2.3). Of those: **5 are dead** — the
`pkg/snapshot/backup_bench_test.go` guards sit inside `func Benchmark*` bodies and **no CI job passes
`-bench`**, so they cost zero and save zero; 4 more (`snapshot_concurrency_test.go`) guard tests that
already run in 2-6s. Three memory-held "slow spots" are **refuted by measurement**:
`backup_bench_test.go` (0s), `snapshot_concurrency_test.go` (2-6s/test),
`parent_mtime_contention_test.go` (3.2s).

**What `-short` must never be pointed at:** `pkg/metadata/store/{badger,sqlite}` (341s/274s) are the
`storetest` conformance suite that CLAUDE.md makes the contract for any new metadata backend, and
`internal/adapter/{nfs/v3,smb,nfs/v4}/handlers` (170+137+86 = 393s) are hundreds of sub-1.5s
wire-and-permission cases. Both are correctness **breadth**, not stress — there is no scale knob to
turn down, and gating them removes coverage rather than slowness.

Note: `-v` output is the only place the job's per-test progress is visible. Before dropping it,
confirm a failing run is still diagnosable from the failure output alone; otherwise keep `-v` and
accept the cost. This is a small win against a real debugging cost.

### Phase 4 — flake policy that does not normalise red

`ci-health.yml` already re-runs failed conformance jobs once on develop. Keep it, but:

- **Keep the retry scoped to postsubmit/periodic develop runs only**, and only `run_attempt == 1`.
  Do **not** extend auto-retry to PR runs — a required check that auto-retries on red teaches
  everyone that red is noise, which is the opposite of what Phase 2 is for.
- Keep the `cancelled` exclusion (concurrency-superseded runs are benign).
- Emit rerun events into a step summary or issue so a chronically-flaky suite is visible.
- A rerun never counts as a first-attempt pass when judging suite health.
- Add a **quarantine list** for known flakes (issue #2634 `TestReconcileSysregTogglesSidecar` is a
  live example): named in a file, run non-blocking, with a deadline. Reuse the shape of
  `required-tests.json` rather than inventing a new mechanism.

The failure mode to avoid is "rerun until green", which is how green stops meaning anything.

## 5. What could mask a defect

1. **1.4 (Windows test scope) — WITHDRAWN, retained only so the reasoning is not rediscovered.**
   The proposal was to reduce the Windows test step to windows-tagged files. It is withdrawn on
   measurement (see the table under 1.4): `-short` already prunes chunker by 97%, while
   `pkg/block/journal` and `pkg/controlplane/runtime/shares` run **10x and 15x slower on Windows**
   than on Linux. Both are cross-platform packages with no `_windows.go` in the name, so a
   tagged-file list would have dropped exactly the two packages carrying the real platform signal.
   The mitigations an earlier draft proposed here were also unsound: the job has **no `go vet` step
   to keep**, and `go build` does not compile `_test.go`, so it is not the compile-coverage backstop
   they assumed. **Windows Build stays as it is. Do not re-propose this without new measurement.**

   **Path-trigger caveat:** an earlier draft proposed triggering on `filepath` or `os.PathSeparator`.
   Those are *source identifiers*, not paths — GitHub path filters match changed file paths, so those
   patterns would never match. If path-triggering is wanted, list concrete Windows-specific files
   (`**/*_windows_test.go`, `cmd/dfs/commands/daemon*.go`, `pkg/controlplane/runtime/snapshot_open*.go`)
   or add a diff-content detector.
2. **Lint consolidation.** Only safe if every existing check is retained in the merged jobs. A
   dropped check is a silent coverage loss — verify by diffing the check list before/after.
3. **Merge queue.** Batching means a batch failure can implicate several PRs. Configure max batch
   size 1–3 initially.
4. **The merge queue costs slots that Phase 1 just freed, and this is not in the ~44 → ~26 table.**
   A queue runs the full check suite a **second time** per queued merge, on a synthetic ref, on top
   of the PR's own run. With 10 open PRs and a post-Phase-1 suite of ~26 jobs, that is up to 26
   extra jobs per merge that the projection does not count. **Worse, they will not cancel.** All 12
   workflows use `group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}`.
   On a `merge_group` event `pull_request.number` is null, so the group falls back to `github.ref` =
   `refs/heads/gh-readonly-queue/develop/pr-N-<sha>`, which is **unique per queue entry** — so a
   superseded entry runs to completion holding slots rather than being cancelled. The failure mode
   to avoid: Phase 1 frees ~18 slots, Phase 2 spends them on uncancellable queue runs, wall-clock
   returns to where it started, and §6's falsifier has already been evaluated and passed. **Fix the
   concurrency group for `merge_group` before enabling the queue, and re-measure slots after.**
5. **Merge queue vs. `cancel-in-progress: true`.** Every workflow here sets
   `cancel-in-progress: true` with a group keyed on `github.ref`. A merge queue uses a synthetic
   `gh-readonly-queue/...` ref, so the group key changes and cancellation behaves differently than
   on a PR ref. Verify after enabling that a superseded queue entry cancels cleanly rather than
   leaving a required check pending forever.
5. **Aggregator jobs add a job each.** They are cheap (seconds, no checkout needed beyond the
   minimal) but they do consume a slot. Net job count still falls sharply; do not let this argue
   against the gate.

## 6. What would falsify this plan

- If, after Phase 1, PR wall-clock does **not** drop sharply, then queue starvation was not the
  binding constraint and the long conformance cells are. That would point at raising the PR tier
  rather than shrinking jobs.
- The projected savings are estimates. The queue-wait numbers are exact; the projections assume the
  runner pool is binding, which the evidence strongly supports but which was not A/B tested.
  **Measure Phase 1 before committing to Phases 3–4.**
- The exact free-tier concurrency ceiling was not readable via the API. Public repo implies minutes
  are free, which points at concurrency, but spending-limit throttling was not ruled out.
- **Queue depth is bursty.** The 21-min Lint and 997s-grader measurements were taken when 24 runs
  were queued. On a quiet repo the queue term shrinks and the critical path reverts to conformance's
  genuine ~29 min floor. The structural fix (fewer jobs) holds either way, but **do not promise
  "21 min → 5 min"** as a steady-state number.
- **A note on the three `Unit Tests` numbers in this document, which are not contradictions but
  are easy to read as one.** 390s (§2.9) is the *execution* span of one representative successful
  run (`started_at` → `completed_at`). ~950s is the *test step* on the run sampled for per-package
  timing in 3.3. 1152s is the *whole-job wall clock including queue* on the congested run in §2.1.
  Three spans, three runs. Any future edit citing a `Unit Tests` duration must say which span it
  means, or this document reads as self-contradicting to anyone checking it.
- **`Unit Tests`' execution time is real, not queue** — it is a single job, so it cannot be
  inflated by intra-workflow stagger. But an earlier draft called it "a floor that cannot be cut
  without dropping `-race`", and that is now **refuted twice over**: `-short` alone takes ~460-500s
  off it (3.3), and neither `-race` nor `-coverprofile` defeats Go's test cache (§2.8), so the floor
  is a consequence of the broken cache rather than a property of the suite. Do not treat it as
  queue-recoverable; do not treat it as irreducible either.
- **The `-short` question for Windows is now answered, and the answer went against 1.4** (§2.3
  caveat, resolved in 1.4): `-short` already prunes chunker by 97%, while `journal` and
  `runtime/shares` got 10x and 15x *slower* on Windows. 1.4 is withdrawn.
- **Phase 0's CI delivery is the least-evidenced claim in this plan.** The 342s → 32s figure is a
  local same-machine no-change re-run, and the one CI-side natural experiment available (§2.8)
  found **no signal**. If Phase 0 lands, verifies as restoring via 0.3, and Unit Tests does not
  move, then the build-cache theory is wrong — and that is also the trigger in §3 for reopening the
  build-system question. This is the cleanest falsifier in the document; run 0.3 before believing
  anything else in Phase 0.
- **The fan-out measurement cuts against the headline for *this* team.** §2.9 measures a median
  commit at 6.8% of suite time but the active block-dataflow commits at 69.9-92.1%. A cache win
  reported as a median will overstate what the people actually pushing today will feel.

## 7. Verify

```bash
# Phase 0.1: cache budget must fall well under the 10 GB cap, and NAR entries must stop dominating.
gh api repos/marmos91/dittofs/actions/cache/usage \
  --jq '"\(.active_caches_count) entries, \(.active_caches_size_in_bytes/1e9|floor) GB"'
gh api --paginate "repos/marmos91/dittofs/actions/caches?per_page=100" \
  --jq '.actions_caches[].key' | grep -c '\.nar' || true

# Phase 0.2: an explicit go-build cache must exist and must NOT be a 10 MB stub.
gh api --paginate "repos/marmos91/dittofs/actions/caches?per_page=100" \
  --jq '.actions_caches[]|select(.key|startswith("gocache-"))|"\(.size_in_bytes) \(.key)"'

# Phase 0.3: the probe step must print "(cached)" on its second go test.
# Phase 3.3: -short must actually be on the unit job.
grep -n 'go test' .github/workflows/unit-tests.yml

# Phase 1: total job slots per push across ALL PR-triggered workflows (not just Lint).
# Replace <sha> with the head commit of the PR under test.
# NOTE: the inner `gh api` must not inherit stdin, or it eats the remaining ids and
# the loop silently processes one run. Also filter out skipped jobs: .total_count
# counts them, and a skipped job is not a slot.
gh api "repos/marmos91/dittofs/actions/runs?head_sha=<sha>&per_page=100" \
  --jq '.workflow_runs[]|select(.event=="pull_request")|.id' > /tmp/runids
while read -r id; do
  gh api "repos/marmos91/dittofs/actions/runs/$id/jobs" </dev/null \
    --jq '[.jobs[]|select(.conclusion!="skipped")]|length'
done < /tmp/runids | paste -sd+ | bc

# Phase 1: Lint wall-clock should fall from ~21min to ~5min
gh run list --workflow=Lint --limit 5 --json createdAt,updatedAt \
  --jq '.[]|(((.updatedAt|fromdate)-(.createdAt|fromdate))|floor)'

# Phase 2: required checks must exist and be non-empty
gh api repos/marmos91/dittofs/rulesets --jq '.[].id' > /tmp/rulesetids
while read -r id; do
  gh api "repos/marmos91/dittofs/rulesets/$id" </dev/null \
    --jq '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context'
done < /tmp/rulesetids

# Phase 2: no duplicate grader execution. `grep -c` exits 1 on the INTENDED
# post-fix result (0 matches in lint.yml), so it must not be the last command in
# a `set -e` script — hence the explicit `|| true` and the printed expectation.
grep -c 'known-failures_test.sh' .github/workflows/conformance.yml .github/workflows/lint.yml || true
echo "expect: conformance.yml >= 1, lint.yml == 0" 

# Phase 2: merge_group present on every gated workflow
grep -L 'merge_group' .github/workflows/{lint,unit-tests,integration-tests,conformance,secret-scan}.yml
```

## 7b. Operational gaps this plan does not yet close

An adversarial pass found these missing entirely. They are listed rather than solved because each
needs a decision, not analysis — but **none of Phase 1 or Phase 2 should land before 1 and 2 have
an answer.**

1. **No rollback plan.** Nothing states how to undo Phase 1 or Phase 2 if throughput gets *worse*.
   Given §5.4 (the merge queue may consume the slots Phase 1 frees, uncancellably), this is the
   most load-bearing omission in the document. Write the revert before the change.
2. **No baseline capture, so the success metric is not measurable as specified.** §6 warns that
   queue depth is bursty, and then §7 offers a single point-in-time command. A before/after taken
   on different queue-depth days is uninterpretable. **Take N ≥ 5 baseline samples across different
   times of day before Phase 1 lands**, and compare distributions, not single runs. This is the
   same discipline §2.9's corrected table had to learn the hard way.
3. **In-flight PRs during consolidation.** Phase 1.1/1.2 delete check names while ~10 PRs are open.
   Unstated: whether those PRs need a rebase, whether stale check names linger on them, and what
   happens if the consolidation and the required-check configuration land the same day.
4. **Whether required checks can be added incrementally.** The tier split implies yes, but it is
   never confirmed that a ruleset accepts a partial `required_status_checks` list without blocking,
   nor what happens to the 10 in-flight PRs the moment the first name is added. `Protect develop`
   currently has `required_status_checks: ABSENT`, so this is a one-way door with no stated exit.
5. **Phase 0.3 has no owner and no trigger.** It is listed as a change item with confidence "None —
   pure observation". Name who runs it and on which run, or it will not be run.
6. **`windows-build.yml` has the same path-filter deadlock property as `unit-tests.yml`** — it is
   `paths:`-filtered to Go files identically — but it appears in no tier and in none of the
   path-filter discussion. Either it is never required, or it needs the same job-level treatment.

## 8. Open decisions

1. **Windows suite destination**: reduce to windows-tagged files (1.4), push-to-develop only, or
   path-triggered on Windows-touching PRs? **Resolved: none of these — 1.4 is withdrawn** (see the
   measured table under 1.4). Windows Build stays exactly as it is. Keep a full
   Windows run on push-to-develop. Measure the current test-step duration first.
2. **Merge-queue batch size**: start at 1 (correctness) or 3 (throughput)? Recommendation: 1.
3. **Required review count 0 → 1**: does the maintainer want a human gate, or is the check suite
   sufficient? This plan assumes yes.
4. **Whether to spend on paid runners** instead of restructuring. Not required — the plan reaches
   ~5–8 min on free runners in the quiet case — but it is the alternative if throughput must scale
   past ~20 concurrent PRs.

## 9. Provenance and confidence

Two independent reviews were run against this plan; both are reflected above.

**Council pass (oracle, forked context; reviewer, fresh context).** No `council-*` advisor profiles
are installed on this machine, so this ran in the documented **degraded mode**: two advisors rather
than a full roster, single pass, read-only. Labeled honestly rather than presented as a full council.

**Findings the council added to the original draft:**

- **`lint.yml` has no `timeout-minutes` on any job** (verified: `grep -c` → 0). A wedge holds a
  runner slot for 6 hours. This became 1.5.
- **The `dfs-1.26.x` artifact is uploaded and never downloaded** — every `download-artifact` in the
  repo reads `dfs-binaries`. This became 1.6.
- **The path-filter deadlock in Phase 2** (a required check on a path-filtered workflow can never be
  satisfied on a docs-only PR). This is the single most important correction to the draft; it is
  now the Tier-1/Tier-2 split.
- **`-p 1 ./...` in integration tests serializes the whole module**, and the fix must derive its
  package list at runtime or a new integration file silently stops running. This became Phase 1b.
- **The `-short` guards sit in the *expensive* tests**, so the Windows saving may be smaller than
  the wall-clock average suggests. This caveat now sits in §2.3 and §6.
- **`Unit Tests`' 1152s is a single job and cannot be queue-inflated** — it is a genuine floor. Do
  not present it as recoverable.

**Corrections the council made to the parent's own evidence:**

> **SUPERSEDED — read §2.8 and §6 first.** The bullets below were true findings when written and are
> now **contradicted by later measurement**. They are kept because deleting them would hide that
> this document changed its mind, but they are **not live instructions**: the `setup-go` bullet is
> the reasoning that removed a GOCACHE item which Phase 0.2 has since restored and ordered first,
> and the "genuine floor" bullet is refuted twice over in §6 with a 1152s figure matching no run
> measured today (1002-1055s). Do not cite either as current.

- `actions/setup-go cache: true` caches **both** `GOMODCACHE` and `GOCACHE`, so there is no cold
  build-cache problem. The draft's original "add GOCACHE caching" item was wrong and was removed
  rather than kept for its face value.
- The `testing.Short()` counts were initially inflated by `.claude/worktrees/`; the numbers in §2.3
  are worktree-excluded.
- The `nfs-pynfs` merge saves ~3 jobs, not 8 — the higher figure double-counted cells already
  `needs`-gated rather than independently scheduled.

**Findings added by the Copilot review on PR #2637:**

- **`Conformance summary` is not a safe gate today.** It `needs:` only the four suite jobs, omitting
  `matrix`, `graders`, and `build`; and `wpts`/`smbtorture` `need:` only `matrix`, not `build`. A
  failed build or grader therefore leaves `summary` green. This is a **pre-existing false-green path**
  worth fixing whether or not the gate is ever enabled.
- **CodeQL contributes 3 jobs, not 1** (`[actions, go, python]`), so the baseline was understated.
- **`merge_group` breaks two payload assumptions:** `conformance.yml`'s `EVENT` ternary falls through
  to `'push'` (the *full* matrix — more jobs than the PR path), and `commit-type-matches-diff` reads
  `github.event.before`, which a merge group does not carry, so it would pass without checking
  anything.
- **The job-count projection conflated slots with duration.** 1.3 and 1.4 save wall-clock without
  freeing a slot; the corrected table separates the two.
- **Lint 10 → 2 was arithmetically wrong** (the grouping leaves 5 definitions, 4 active), and
  `spec-citations`/`required-tests` need Go provisioned in the merged job.
- **The Windows path-trigger mitigation was invalid** — `filepath` and `os.PathSeparator` are source
  identifiers, not paths.
- **The Phase 1 verify command only counted Lint jobs** while claiming to verify the global total.

**Confidence:** high on the diagnosis (job timestamps are exact, the ruleset state was read from the
API, and three independent reviewers reached the same dominant cause). Medium on the projected
savings, which are estimates — §6 states what would falsify them. The structural recommendation
(fewer jobs, then a real gate) does not depend on the exact magnitudes.
