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

Cause: public repo, free standard runners, shared concurrency pool. One PR push creates ~42 jobs
(Lint 10, Conformance 18, NFS Conformance 8, Unit 1, Integration 1, Windows 1, CodeQL 1, Secret
Scan 1, CI Health 1). Nine open PRs plus every `develop` push draw from the same pool. The pool
cannot drain that, so latency is set by scheduling, not by test execution.

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

### 2.3 Windows Build is ~99% a duplicate

`windows-build.yml:59` runs `go test -short -race -p 2 -v ./...`.

- `pkg`/`internal`/`cmd` contain **1099** test files.
- `-short` guards exactly **13** call sites in that tree (`if testing.Short()`).
- The rest of the 197 repo-wide `testing.Short()` references live in `test/e2e` (50) and
  `test/integration`, which the untagged `./...` run does not compile.

So Windows re-runs ~1086 test files that Linux already ran, race-enabled, on the most contended
and most expensive runner. Avg wall 1477s. The two `go build` steps are the part that is genuinely
Windows-specific.

### 2.4 Duplicate grader execution

`test/conformance/run_test.sh` and `test/common/known-failures_test.sh` run in **both**
`conformance.yml:123-126` and `lint.yml:75-78`.

### 2.5 Duplicate build + scaffolding

`conformance.yml` "Build Binaries" and `nfs-pynfs.yml` "Build Binaries" both nix-build `dfs` +
`dfsctl` and both upload an artifact named `dfs-binaries`. `conformance.yml:83` computes a `pynfs`
matrix output it never uses — dead. `nfs-pynfs.yml` re-resolves the same `suites.json` with its own
matrix/grading/baseline jobs.

Note the cells are **not** duplicated: pynfs runs only in `nfs-pynfs.yml`. What is duplicated is the
scaffolding around them.

### 2.6 Correction to a plausible-looking claim

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
| 1.1 | Lint: 10 jobs → 2. Group the script-only checks (spec-citations, conformance-manifest, new-package-tests, commit-type, required-tests, e2e-harness) into one job sharing a checkout; keep `golangci-lint` and `format-vet` separate (they need the Go toolchain). `shellcheck-changes`+`shellcheck` stay as-is (already gated). | 8 slots/PR; Lint 21min → ~5min | Neutral — same checks run |
| 1.2 | Fold `nfs-pynfs.yml` into `conformance.yml` as a `pynfs` suite job. One matrix, one Build Binaries, one Nix install. Delete the dead `pynfs` output or use it. | 8 slots/PR, 1 workflow, 1 build | Slight ↑ — one manifest path |
| 1.3 | Delete the duplicate grader run from `conformance.yml` (keep it in Lint, which runs on docs-only changes and `conformance.yml` does not). | 1 slot, seconds | None |
| 1.4 | Windows Build: keep the two `go build` steps per-PR; move `go test -short -race ./...` to push-to-develop + nightly. | ~25min off most PR walls, 1 slot | ↓ on Windows-specific race detection — see 5.1 |

**Expected: ~42 → ~22 jobs per push.** On a 20-slot pool that is the difference between a 20-minute
queue and near-none.

### Phase 2 — adopt the gate (attacks the confidence gap)

1. **Add required status checks** to the `Protect develop` ruleset, against the *post-Phase-1* names:
   `Conformance summary`, `golangci-lint`, `Format & Vet`, `Required Tests`, `Unit Tests`,
   `Integration Tests`, `gitleaks`, `Commit Type Matches Diff`, and the merged Lint job.
   Terminal/gate jobs only — not all 42.
2. **Enable a merge queue** on `develop` and `main`. Add `merge_group:` to the `on:` blocks of every
   workflow carrying a required check, or the queue waits for checks that never start.
3. **Raise `required_approving_review_count` 0 → 1.**

Why this is the confidence story: required checks make green *necessary*; the merge queue makes the
tested artifact the *merge result* rather than each PR against a stale base; `required-tests.json`
already pins the specific authorization/encryption gates whose deletion is otherwise silent. None of
it needs new test code.

### Phase 3 — cheap per-job wins

| # | Change | Saves |
| --- | --- | --- |
| 3.1 | Drop `-v` from `unit-tests.yml:50`. Thousands of per-test log lines on the most contended job. Keep it on failure via `-json` or a rerun if needed. | ~10–20% of unit job |
| 3.2 | Keep `-race` on Linux only. It is already the case after 1.4. | — |

### Phase 4 — flake policy that does not normalise red

`ci-health.yml` already re-runs failed conformance jobs once on develop. Keep it, but:

- Emit rerun events into a step summary or issue so a chronically-flaky suite is visible.
- A rerun never counts as a first-attempt pass when judging suite health.
- Add a **quarantine list** for known flakes (issue #2634 `TestReconcileSysregTogglesSidecar` is a
  live example): named in a file, run non-blocking, with a deadline. Reuse the shape of
  `required-tests.json` rather than inventing a new mechanism.

The failure mode to avoid is "rerun until green", which is how green stops meaning anything.

## 5. What could mask a defect

1. **1.4 (Windows test run off the PR path).** Windows-specific race or path-separator bugs would
   no longer block a PR. Mitigations: keep the build per-PR; run the Windows suite on push-to-develop
   so a break is caught before it reaches `main`; optionally trigger the full Windows run on PRs that
   touch `*_windows_test.go`, `filepath`, or `os.PathSeparator`.
2. **Lint consolidation.** Only safe if every existing check is retained in the merged jobs. A
   dropped check is a silent coverage loss — verify by diffing the check list before/after.
3. **Merge queue.** Batching means a batch failure can implicate several PRs. Configure max batch
   size 1–3 initially.

## 6. What would falsify this plan

- If, after Phase 1, PR wall-clock does **not** drop sharply, then queue starvation was not the
  binding constraint and the long conformance cells are. That would point at raising the PR tier
  rather than shrinking jobs.
- The projected savings are estimates. The queue-wait numbers are exact; the projections assume the
  runner pool is binding, which the evidence strongly supports but which was not A/B tested.
  **Measure Phase 1 before committing to Phases 3–4.**
- The exact free-tier concurrency ceiling was not readable via the API. Public repo implies minutes
  are free, which points at concurrency, but spending-limit throttling was not ruled out.

## 7. Verify

```bash
# Phase 1: job count per push should fall from ~42 to ~22
gh run list --workflow=Lint --limit 3 --json databaseId \
  --jq '.[]|.databaseId' | while read id; do
  gh api repos/marmos91/dittofs/actions/runs/$id/jobs --jq '.total_count'
done

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

1. **Windows suite destination**: push-to-develop only, or path-triggered on Windows-touching PRs?
   Recommendation: path-triggered, so Windows changes are still gated.
2. **Merge-queue batch size**: start at 1 (correctness) or 3 (throughput)? Recommendation: 1.
3. **Required review count 0 → 1**: does the maintainer want a human gate, or is the check suite
   sufficient? This plan assumes yes.
4. **Whether to spend on paid runners** instead of restructuring. Not required — the plan reaches
   ~5–8 min on free runners — but it is the alternative if throughput must scale past ~20 PRs.
