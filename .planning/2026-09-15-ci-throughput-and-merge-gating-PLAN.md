# CI throughput and merge gating — plan

## 1. The job

Cut DittoFS PR CI wall-clock from ~20–30 minutes to ~5–8, **without deleting a single test**, and
make a green CI mean "safe to merge" — which today it does not.

Two problems, one root each:

| Problem | Root cause | Fix shape |
| --- | --- | --- |
| PR CI takes forever | ~42 jobs per push into a ~20-slot free-runner pool; ~85% of wall-clock is queue wait | Delete jobs, not tests |
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

Nothing stops a merge with 42 red checks, and nothing requires a green one to be read. Every
question about "does green mean safe" is currently moot.

### 2.3 Windows Build is largely a duplicate, but `-short` prunes the *expensive* tests

`windows-build.yml:59` runs `go test -short -race -p 2 -v ./...`.

- `pkg`/`internal`/`cmd` contain **1099** test files.
- `-short` guards **13** call sites in that tree, all `if testing.Short()` (19 `testing.Short()`
  references total, 6 of them negations or bench guards).
- The rest of the 197 repo-wide references live in `test/e2e` (50) and `test/integration`, which the
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

### 2.7 Correction to a plausible-looking claim

`actions/setup-go` with `cache: true` caches **both** `GOMODCACHE` and `GOCACHE` ("built-in caching
and restoration for Go modules and build outputs"). All 17 jobs already use it. Build-cache sharing
is therefore **already done** and is not an item in this plan.

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
- **Test breadth.** 4475 test files, POSIX/WPTS/smbtorture conformance, E2E. That is the asset.
  Nothing here deletes coverage.

## 4. The change set

Ordered. **Consolidate before wiring required checks** — merging jobs changes check names, so
configuring the gate first means configuring it against names about to be deleted.

### Phase 1 — delete jobs (attacks the dominant cause)

| # | Change | Saves | Confidence |
| --- | --- | --- | --- |
| 1.1 | Lint: 10 jobs → **5 job definitions, 4 active on a typical PR**. Group (a) Go-toolchain checks sharing checkout + `setup-go` (`format-vet`, `spec-citations`, `required-tests` — note `spec-citations` runs `go run` and `required-tests` runs `go test`, so the merged job **must** provision Go and its cache), (b) shell/harness (`e2e-harness` + the conditional ShellCheck pair), (c) manifest/docs (`conformance-manifest`, `new-package-tests`, `commit-type`). Keep each check a **named step** so a red job still says which check failed. | 5–6 slots/PR; Lint 21min → ~5min | Slight ↓ in granularity, mitigated by named steps |
| 1.2 | Fold `nfs-pynfs.yml` into `conformance.yml` as a manifest-driven `pynfs` suite cell (complete the half-wired `outputs.pynfs`). One matrix, one Build Binaries, one Nix install. Add `pynfs` to `summary`'s `needs` so the existing aggregator gates it. | **3 jobs/push** (its matrix/grading/build) — the 4 pynfs cells remain; 1 workflow, 1 build, 1 Nix install | Neutral — same 4 cells, same grader, same KNOWN_FAILURES tables |
| 1.3 | Remove the duplicate grader execution. Drop `run_test.sh` + `known-failures_test.sh` from `lint.yml`, keep `check-docs.sh` there; `conformance.yml`'s `graders` keeps the runner tests. | seconds of *duration*, **not a slot** — the `graders` job remains | None — each script still runs exactly once |
| 1.4 | Windows Build: keep `go build ./...` + `go vet ./...` per-PR, reduce the test step to the windows-tagged files (5) plus `-race` on those. **Measure first** (§2.3 caveat). | 1477s → ~5–8min of *duration*; **no slot saved** unless the job is split | ↓ — see 5.1 |
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

   Fix: add one always-triggering **aggregator job** per gated workflow that `needs:` the
   path-filtered jobs with `if: always()` and fails when any dependency failed, then mark **that**
   job required. It always reports, so the required check is always satisfied — by real passes or by
   a genuine failure. This also keeps the required check *name* stable while the jobs behind it
   change, which is what lets Phase 1 and Phase 2 be done in either order safely.
2. **Enable a merge queue** on `develop` and `main`. Adding `merge_group:` to the `on:` blocks is
   necessary but **not sufficient** — two existing payload assumptions break under it:

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
4. **Apply the same settings to `Protect main`.** The gate is only described for `Protect develop`,
   but merges to `main` flow through it and the rulesets are separate. Gating one branch leaves the
   other bypassable.

Why this is the confidence story: required checks make green *necessary*; the merge queue makes the
tested artifact the *merge result* rather than each PR against a stale base; `required-tests.json`
already pins the specific authorization/encryption gates whose deletion is otherwise silent. None of
it needs new test code.

### Phase 3 — cheap per-job wins

| # | Change | Saves |
| --- | --- | --- |
| 3.1 | Drop `-v` from `unit-tests.yml:50`. Thousands of per-test log lines on the most contended job. Keep it on failure via `-json` or a rerun if needed. | ~10–20% of unit job |
| 3.2 | Keep `-race` on Linux only. It is already the case after 1.4. | — |

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

1. **1.4 (Windows test scope).** Reducing to windows-tagged files drops coverage of the Windows
   branch of *cross-platform* tests (e.g. `pkg/controlplane/store/config_test.go`,
   `pkg/block/journal/segment.go`). This is a genuine confidence trade, not a free win. Mitigations:
   keep `-race` on the reduced set; keep `go build ./...` + `go vet ./...` (compile coverage is the
   bulk of what a platform job uniquely provides); run the full Windows suite on push-to-develop
   and nightly so a break cannot reach `main`.

   **Path-trigger caveat:** an earlier draft proposed triggering on `filepath` or `os.PathSeparator`.
   Those are *source identifiers*, not paths — GitHub path filters match changed file paths, so those
   patterns would never match. If path-triggering is wanted, list concrete Windows-specific files
   (`**/*_windows_test.go`, `cmd/dfs/commands/daemon*.go`, `pkg/controlplane/runtime/snapshot_open*.go`)
   or add a diff-content detector.
2. **Lint consolidation.** Only safe if every existing check is retained in the merged jobs. A
   dropped check is a silent coverage loss — verify by diffing the check list before/after.
3. **Merge queue.** Batching means a batch failure can implicate several PRs. Configure max batch
   size 1–3 initially.
4. **Merge queue vs. `cancel-in-progress: true`.** Every workflow here sets
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
- **`Unit Tests`' 1152s is probably mostly real execution, not queue.** It is a single job, so it
  cannot be inflated by intra-workflow stagger. That ~19 min is a floor that cannot be cut without
  dropping `-race`. Do not treat it as queue-recoverable.
- **The `-short` saving for Windows is unverified** (§2.3 caveat): the 13 guards sit in the most
  expensive tests, so `-short` may already be doing the pruning. Measure the test step's duration
  before acting on 1.4.

## 7. Verify

```bash
# Phase 1: total job slots per push across ALL PR-triggered workflows (not just Lint).
# Replace <sha> with the head commit of the PR under test.
gh api "repos/marmos91/dittofs/actions/runs?head_sha=<sha>&per_page=100" \
  --jq '.workflow_runs[]|select(.event=="pull_request")|.id' | while read id; do
  gh api repos/marmos91/dittofs/actions/runs/$id/jobs --jq '.total_count'
done | paste -sd+ | bc

# Phase 1: Lint wall-clock should fall from ~21min to ~5min
gh run list --workflow=Lint --limit 5 --json createdAt,updatedAt \
  --jq '.[]|(((.updatedAt|fromdate)-(.createdAt|fromdate))|floor)'

# Phase 2: required checks must exist and be non-empty
gh api repos/marmos91/dittofs/rulesets --jq '.[].id' | while read id; do
  gh api repos/marmos91/dittofs/rulesets/$id \
    --jq '.rules[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context'
done

# Phase 2: no duplicate grader execution
grep -c 'known-failures_test.sh' .github/workflows/conformance.yml .github/workflows/lint.yml

# Phase 2: merge_group present on every gated workflow
grep -L 'merge_group' .github/workflows/{lint,unit-tests,integration-tests,conformance,secret-scan}.yml
```

## 8. Open decisions

1. **Windows suite destination**: reduce to windows-tagged files (1.4), push-to-develop only, or
   path-triggered on Windows-touching PRs? Recommendation: reduce the set per 1.4, and keep a full
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
