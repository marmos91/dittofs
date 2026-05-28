# DittoFS v1.0 Code Review & Cleanup

## Context

DittoFS reached Cubbit traction but was heavily AI-coded. User distrusts the verbosity and wants a single concentrated v1.0-prep milestone — weeks of focus — that:

1. Strips AI bloat (planning leak, fluff comments, AI commit trailers).
2. Cross-checks every protocol handler vs canonical Samba / Linux kernel impl — not just "tests pass".
3. Audits each architectural area for correctness, simplicity, perf, security, docs.
4. Rewrites docs so README = "how to use", `docs/` = "how it works".
5. Archives `.planning/` so develop ships lean.

Posture: break anything pre-v1.0 (handles, config, CLI surface in scope per [[feedback_no_prod_users_delete_eagerly]]). Reach v1.0 production-ready.

## Baseline numbers

| Surface | LOC / size |
|---|---|
| Production Go (cmd + pkg + internal) | 194.6K |
| Tests (unit + e2e + integration) | 197.2K |
| Shipping docs | 16.1K |
| `.planning/` | **212.4K (13 MB)** — bloat exceeds codebase |
| K8s operator (`k8s/dittofs-operator/`) | 9.6K |
| Bench suite | ~7.5K active |

Bloat signatures found: 1,441 phase/decision refs in source comments, 95 commits with Claude/AI trailers, 138 fluff phrases, 2,360 `_ =` error discards (needs sampling), no dead code or single-impl interfaces — **code itself is healthier than feared; the real bloat is planning leak + comment verbosity + planning artifacts**.

## Approach

Hybrid: agents find → you+I triage HIGH/MED/LOW → agents fix HIGH, backlog MED/LOW. Cross-check vs canonical impls (Samba `samba-team/samba`, Linux `torvalds/linux/fs/nfs`) for every protocol handler. Wave-staged.

**PR shape per area** — two-PR cycle:
- **PR-A (audit, read-only)**: produces `.planning/v1.0-audit/{area}/REVIEW.md` with HIGH/MED/LOW-tagged findings + reuse opportunities + spec-deviation list. Diff is markdown only.
- **PR-B (fix)**: applies HIGH fixes from REVIEW.md. Atomic per finding where practical. MED/LOW deferred as issues unless trivially co-fixable.

Naming: branch `v1.0/{area}-audit` and `v1.0/{area}-fix`. Assignee `marmos91`. Per [[feedback_assign_prs_to_marmos91]] + commit guidelines.

## Wave 0 — Mechanical cleanup (single PR)

Pre-canvas before deep work. **Branch `v1.0/wave0-mechanical-cleanup`, 1 PR**.

1. **Strip 1,441 planning refs from code comments** — regex sweep across `*.go` for `Phase \d+`, `D-\d{2}`, `BSCAS-\d+`, `GC-\d+`, `STATE-\d+`, `LSL-\d+`, `TD-\d+`, `INV-\d+`, `SC-\d+`, `WR-\d+`, `T-\d{2}-\d{2}-\d{2}`, `decision-?\d+`, `per plan`, `per spec`. Delete comment if reference was the whole comment; trim ref if comment has real content. Per [[feedback_no_phase_comments_in_code]].
2. **Strip Claude/AI commit trailers** — `git log --grep="Claude\|Co-Authored-By: Claude\|AI"` returns 95 commits. Since we're not rewriting public history (would force-push develop), instead: enforce going forward (commit hook) and accept legacy hashes. No history rewrite.
3. **Strip fluff phrases** — `basically`, `simply`, `essentially`, `in order to`, `please note`, `this function`, `this method` in `// ` comments only. Manual review for false positives.
4. **Aggressive `.planning/` archive** — extract to orphan branch `planning-archive`. Keep on develop only: `PROJECT.md`, `ROADMAP.md`, `MILESTONES.md`, `REQUIREMENTS.md`, `STATE.md`, `RETROSPECTIVE.md`, `codebase/` intel, ACTIVE phases (≥17). Delete pre-v0.15 phases + dated roadmaps. Target 212K → ~30K lines.
5. **Delete `docs/FUTURE_IMPROVEMENTS.md`** (974 lines, planning artifact). Move `BENCHMARK-PLAN.md` (340 lines) → `.planning/perf/`.
6. **Capture perf baseline** *(NEW)* — Wave 0 closes by running the macro-bench on Scaleway bench infra against the cleaned tree, capturing CPU + heap + goroutine + mutex profiles into `.planning/v1.0-audit/_baseline/`. Per-area audits diff their post-fix profile against these. Also run micro-bench gates (D-40/41/42) to seed Wave 2 perf-CI work.
7. **Verify**: `go build ./...`, `go test ./... -count=1`, `go vet ./...`, `gofmt -l .` clean. `staticcheck ./...` clean. `gsd-graphify` and `gsd-map-codebase` refresh `.planning/{graphs,codebase}/` for downstream audits.

Estimate: 2–3 days (+1 day for baseline bench run). Single PR. Unblocks everything downstream.

## Wave 1 — Deep area audits (12 area-pairs, parallel-capable)

Each area gets PR-A (REVIEW.md) → triage → PR-B (fixes + simplifier + reviewer + verify). Areas ordered by **perf leverage × correctness risk** — hot paths first (biggest pprof wins), cold paths later:

| # | Area | Path(s) | LOC | Cross-check ref | Perf leverage |
|---|---|---|---|---|---|
| 1 | Block stores + CAS + engine | `pkg/blockstore/{engine,local,remote,chunker,compression,encryption}` | 19.3K | FastCDC paper, S3 API spec, BLAKE3 spec | **HIGH** (data path) |
| 2 | Syncer | `pkg/blockstore/{remote,engine}/syncer` | (subset of #1) | RFC 7233 ranges, S3 multipart | **HIGH** (background I/O) |
| 3 | SMB handlers (SMB2 wire family: dialects 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1; **path-rename candidate** — see §SMB layout below) | `internal/adapter/smb/`, `pkg/adapter/smb/` | 36.9K | MS-SMB2, Samba `source3/smbd/smb2_*`, MS-FSA | **HIGH** (protocol hot path) |
| 4 | NFS handlers | `internal/adapter/nfs/`, `pkg/adapter/nfs/` | 52.9K | RFC 1813 / 7530 / 8881, Linux `fs/nfsd/`, `fs/nfs/` | **HIGH** (protocol hot path) |
| 5 | Lock manager + ACL | `pkg/metadata/lock`, `pkg/metadata/acl` | ~3.5K | MS-FSA §2.1.5, Samba locking, MS-DTYP SD | MED (contention-prone) |
| 6 | Metadata stores | `pkg/metadata/store/{badger,memory,postgres}` | 15.4K | POSIX.1-2017 VFS, Linux `fs/` — **NOT storetest** | MED |
| 7 | Runtime | `pkg/controlplane/runtime/` | 11.1K | (internal — graph from gsd-graphify) | MED |
| 8 | GC | `pkg/blockstore/engine/gc*` | (subset of #1) | RFC-style ref-counted CAS | LOW (background) |
| 9 | Backup/Snapshot | per [[project_share_snapshots_design]] | n/a (new) | (replaces deprecated v0.13 backup) | LOW |
| 10 | Config | `pkg/config/` | 1.2K | — | LOW |
| 11 | `dfs` + `dfsctl` CLI | `cmd/dfs/`, `cmd/dfsctl/`, `internal/cli/` | 14K | UX coherence, Cobra patterns | LOW |
| 12 | K8s operator | `k8s/dittofs-operator/` | 9.6K | controller-runtime, kubebuilder patterns | LOW |

**Each PR-A (audit) checks** (9 dimensions):

1. **Bloat**: unused funcs/types, single-impl interfaces, overlong files, dead branches.
2. **Correctness vs canonical**: handler-by-handler diff against Samba/Linux. Flag spec deviations that tests don't catch (overfit risk). Use pcap-diff playbook from CLAUDE.md for protocol byte-level checks.
3. **Module structure, Go best practices & aggressive collapse** *(NEW)* — guiding principle: **less is more** ([[feedback_less_is_more]]). Default action is *delete* / *merge*; expansion is the exception.
   - **Collapse structs** — if two structs share >70% of fields or always travel together, merge them. Kill DTOs/wire-types that mirror domain types 1:1. Kill `Options`/`Config` structs that have one consumer.
   - **Collapse interfaces** — if an interface has one impl AND one caller, inline. If two interfaces have overlapping method sets used together, merge. No "future-proofing" interfaces.
   - **Collapse functions** — kill wrapper funcs that just call another. Kill `NewFooDefault` when `NewFoo` with defaults works. Kill helper funcs called once — inline at the callsite.
   - **Collapse types** — kill type aliases that add no semantic value. Kill enum wrappers around plain strings/ints when constants suffice.
   - **Kill shims** — adapter layers / compat wrappers left from old refactors. Per [[feedback_no_prod_users_delete_eagerly]] we have no prod users; delete in place.
   - Package layout — does each package have a single coherent purpose? Are utility grab-bags (`util`, `helpers`, `common`) hiding poor cohesion?
   - **No interface leaks** — interfaces defined and exported where consumed (not where implemented); no exported types that only internal callers use; no `internal/` types leaking through public `pkg/` API.
   - Dependency direction — **first decide intent** for the `pkg/` ↔ `internal/` split. The Go compiler only blocks `internal/` imports from *outside the parent module*; intra-module `pkg/ → internal/` is allowed. The "pkg/ is public, internal/ is private" rule is `golang-standards/project-layout` folklore, not Go language guidance. For DittoFS (primarily an app, not a library), three options to choose between in the runtime/structural audit:
     - **(a) Treat `pkg/` as stable public API** — enforce no `pkg/` → `internal/` imports going forward; current violations become audit findings.
     - **(b) Treat the split as organizational only** — drop the rule, focus solely on cycles + layering + cohesion.
     - **(c) Flatten** — merge `pkg/` and `internal/` into a single tree organized by domain. Many large Go apps do this.
     
     **Hard rules** regardless of which we pick: no import cycles (`go list -deps`), no cross-layer skips (handlers don't reach into stores past Runtime per CLAUDE.md invariant), no upward deps (storage doesn't import adapters).
   - Public surface minimization — every exported symbol justified; unexport what only one package consumes.
   - File layout — files split by concern, not by arbitrary size; no 2K-line god-files (e.g. SMB `set_info.go` 1.7K, NFS `StateManager` 2.9K) unless concern is genuinely monolithic — but also no 50-file packages where 5 would do.
   - **Naming pass** — rename for clarity at every level:
     - *Variables*: kill cryptic shortenings (`req`/`res` OK, `mgr`/`hdlr`/`svc`/`cfg` OK at locality, but `m`/`s`/`x` only as receivers or 1-line scope; full words where domain is non-obvious).
     - *Structs / types*: behavior-describing, not pattern-suffix-driven (`LeaseTable` over `LeaseManager` if it's really a table; `SecurityDescriptor` not `SDImpl`; drop `Impl`/`Helper`/`Wrapper` suffixes unless genuinely a wrapper).
     - *Functions*: verb-first, name describes effect/return; no `DoFoo` / `HandleFoo` / `ProcessFoo`; no `GetX` for trivial accessors (Go convention).
     - *Files*: filename matches dominant type or behavior; no `utils.go`/`helpers.go`/`misc.go` — split by concern.
     - *Modules/packages*: short, lowercase, no underscores, no plurals (`blockstore` not `block_stores`); package name not stuttered in symbol names.
     - Go-idiomatic: no `IFoo` interfaces, no stuttering `pkg.PkgThing`, accept short receiver names, error sentinels `Err*`.
     - Renames touch the public API — bundle into the area's PR-B; document in CHANGELOG.
   - Module boundaries — `runtime/` sub-services (adapters/stores/shares/mounts/lifecycle/identity) are properly separated; no cross-talk that bypasses Runtime.
4. **Performance & profiling** *(NEW)* — per area, capture pprof CPU + heap profiles under a representative workload, then triage hotspots:
   - **CPU profile** (`go test -bench -cpuprofile cpu.pprof` for micro paths; `go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30` on a running `dfs` under load for macro). Top-10 functions by `cum` and `flat` get a line-by-line look. Targets: hot allocs, redundant copies, lock contention (use `block.pprof` + `mutex.pprof`).
   - **Heap profile** (`go tool pprof http://localhost:6060/debug/pprof/heap`). Look for unbounded buffers, per-request allocs that should pool (per [[feedback_streaming_io_for_data_paths]]), object retention through caches.
   - **Goroutine profile** — leaks, stuck goroutines.
   - Per-area workload definitions:
     - SMB / NFS handlers — smbtorture / pjdfstest + macro-bench (random write, metadata burst, large sequential).
     - Block store / engine — FastCDC chunker bench, S3 sync loop under load, GC sweep on large CAS.
     - Metadata stores — concurrent OPEN/CREATE/RENAME storm on badger + postgres.
     - Lock manager — multi-client lease churn, BR-lock storm.
     - Runtime — share lifecycle churn, mount/unmount loop.
   - Each area's REVIEW.md gets a **Bottlenecks** section listing the top 5 hotspots (file:func with cum%) + proposed fix or "no-action: contention is intrinsic".
   - PR-B for an area MAY include perf fixes IF they're isolated; deeper rewrites filed as separate `v1.0-perf-{area}` issues.
   - Acceptance: macro-bench on bench infra shows no regression > 5% vs v0.16 baseline AND ideally one area-level improvement.
5. **Concurrency**: `go test -race`, lock orderings, shared-state audit.
6. **Error handling**: each `_ =` error discard categorized (legit cleanup vs hidden bug); error wrapping uses `%w`; sentinel error discipline.
7. **Resource lifecycle**: leaks, missing `Close`, context plumbing, goroutine ownership.
8. **Security**: input validation, path traversal, share-boundary checks, signing/encryption correctness.
9. **Logging**: signal vs noise; level discipline (`Debug` for expected errors, `Error` for unexpected per CLAUDE.md invariant 6).
10. **Docs / godoc**: behavior-only comments. **No planning refs, no conformance-suite refs** (no "fixes smbtorture X" / "needed by WPTS Y" / "pjdfstest expects Z" — those rot and bias readers toward the test, not the spec). When a citation is load-bearing, reference the **authoritative source**: RFC number + section, MS-* doc section (MS-SMB2 §3.3.5.x, MS-FSA §2.1.5.x), POSIX.1-2017 section, Linux kernel file path, Samba source path. Format: `// per MS-SMB2 §3.3.5.9` not `// fixes smbtorture lease.breaking3`.
11. **Tests**: structural fit (unit tests live with code, table-driven idiom, no over-mocking of internals), gaps in unit coverage, edge cases missing from e2e, brittle assertions.

**Skill / agent orchestration per area**:

*PR-A — audit (parallel where possible):*
- `feature-dev:code-explorer` agent — maps area's architecture, dependencies, layers, abstractions. → REVIEW.md "current state".
- `feature-dev:code-architect` agent — proposes ideal module/folder structure, flags interface leaks + layering violations + over-abstraction + collapse opportunities. → REVIEW.md "structural findings".
- `feature-dev:code-reviewer` agent — bugs, security, quality, convention adherence with confidence-based filtering. → REVIEW.md "bug findings".
- `gsd-graphify` skill — once, at Wave 0 end. Builds project dep graph in `.planning/graphs/`. Each area audit queries it instead of re-mapping.
- `gsd-map-codebase` skill — once, refreshes `.planning/codebase/` intel docs. Each area audit pulls from these.

*PR-B — fix & verify (sequential):*
1. Apply HIGH findings from REVIEW.md (manual or via subagent).
2. `code-simplifier:code-simplifier` agent — second-pass collapse sweep over the just-changed code. Catches what the human-driven fix missed.
3. `feature-dev:code-reviewer` agent — re-review the changes (independent eyes on the fix).
4. `superpowers:systematic-debugging` skill — invoke if any test went red during PR-B.
5. `superpowers:verification-before-completion` skill — gates the "done" claim; requires real test run + acceptance criteria check.
6. `verify` / `run` skill — manual smoke if a UX/runtime path was touched.
7. `code-review` skill (low or medium effort, post-push) — sanity diff review before merge.
8. `caveman:caveman-commit` skill — terse commit messages.
9. `gsd-extract-learnings` skill — after area closes, harvest decisions/patterns/surprises into memory so later areas benefit.

*Cross-cutting:*
- `gsd-secure-phase` skill — final acceptance for security dimension (post-Wave 1).
- `code-review ultra` skill (cloud, user-triggered) — once at end of each Wave 1 area-pair before merging the FIX PR.
- `superpowers:dispatching-parallel-agents` skill — orchestrates concurrent area audits (Wave 1 streams that don't share files).

Output: `.planning/v1.0-audit/{area}/REVIEW.md` with sections — Current State, Structural Findings, Bug Findings, Tests Findings, HIGH/MED/LOW triage table, reuse opportunities, suggested deletes.

**In-tree conformance suites are AUDIT SUBJECTS, not authorities.** `pkg/metadata/storetest/` and `pkg/blockstore/blockstoretest/` were authored as part of the same AI-assisted effort and may carry the same biases as the implementations they test (a backend can pass a suite that codifies a bug). Each suite gets its own audit pass:

- Cross-check the assertions against external specs (POSIX, MS-FSA, S3 API), not against current DittoFS behavior.
- Look for "round-trip" tests that just echo implementation choice instead of asserting spec behavior.
- Look for missing edge cases the implementation doesn't handle (which is why the test was never written).
- Verify the suite exercises ALL backends equally — no badger-shaped assertions that happen to pass on postgres by accident.
- Pair the conformance suite audit with the corresponding store/backend audit so spec drift is found once and fixed in both.

Where ground truth is unclear, prefer **external test suites** (pjdfstest for POSIX, smbtorture + WPTS for SMB, NFS Tests Suite or Connectathon for NFS) over our own conformance code.

**Each PR-B (fix)** applies HIGH; MED/LOW filed as `v1.0-followup` issues.

**Parallelization**: areas with no overlap can run audit-streams concurrently. Suggest pairs that share files (lock+ACL+metadata; engine+syncer+GC) run sequentially.

### SMB layout — rename `v2/` and flatten

Sub-decision under area #3 (SMB handlers). The current path `internal/adapter/smb/v2/handlers/` is misleading: `v2/` looks like it implies the server only speaks SMB 2.x, when in fact it supports the full SMB2 wire family — dialects 2.0.2, 2.1, 3.0, 3.0.2, 3.1.1 — plus SMB3-only features (AES-CCM/GCM encryption, CMAC/GMAC signing, SMB3 KDF, multi-channel, durable handles V1, leases, ACLs). The `v2` token there is Microsoft's name for the wire format ("SMB2"), not a dialect ceiling.

External readers (and our future selves) read `v2/handlers/` as "this is the SMB2.x-only handler bag, SMB3 must live elsewhere." There IS no elsewhere — every command handler under that dir is dispatched for every SMB 2.x AND 3.x request. The naming is technically defensible per MS-SMB2 spec convention but actively misleads code review.

**Proposal** — bundle a structural rename into the area #3 PR-B:
- **Option A (minimal, recommended)**: collapse `internal/adapter/smb/v2/handlers/` → `internal/adapter/smb/handlers/`. The `v2` segment carries zero information at the path level — dispatch already routes by dialect at runtime, and there is no `v3/handlers/` sibling. Single `git mv`, `goimports`, and one round of import-path updates. ~88 files moved, no logic change.
- **Option B**: keep `v2/` but rename to `wire/` or `commands/` — preserves a layer of grouping if we expect future non-wire SMB code (e.g. an admin REST surface). Same migration cost.
- **Option C**: do nothing — keep `v2/` but document the meaning in a top-level `internal/adapter/smb/README.md`. Cheapest, least durable.

Decision points for the area-pair PR-A:
- Are there any *non-SMB2-wire* code paths planned under `internal/adapter/smb/` that would justify a sibling to `v2/`?  (If no — A wins.)
- Does any external consumer import `internal/adapter/smb/v2/...`? (Probably no — `internal/` blocks external imports anyway.)
- Does `pkg/adapter/smb/` need the same treatment? (It exports types; check whether `v2` appears in the public API.)

Migration constraints:
- `internal/` packages have no external consumers, so the rename is repo-internal — no SemVer concerns.
- Bundle the rename into PR-B for area #3 so the audit's other findings land together with the move. Don't ship a "rename-only" PR — it would conflict with every active SMB branch (durable-handle V2 #432, BR-lock async #430, ADS-cluster #471, …).
- Update `CLAUDE.md` references to `internal/adapter/smb/v2/handlers/` after the rename.

Filed as **issue #674** (will create alongside the area-pair kickoff).

## Wave 2 — Cross-cutting streams

Run alongside Wave 1.

1. **E2E flakiness stabilization** — replace 135 `time.Sleep`/`time.After` with `WaitFor*` framework helpers (10 already exist; extend). Audit `test/e2e/framework/` (17K `mount.go` is heavy — trim). Branch `v1.0/e2e-stabilize`.
2. **Test coverage gap-fill** — postgres store (6 tests / 20 src), `pkg/metadata/errors` (0), `pkg/controlplane` (0). Use `superpowers:test-driven-development` skill where adding new tests for un-tested code. Branch `v1.0/test-coverage`.
3. **Bench suite — from-scratch redesign** — Wave 0 baseline run proved the current harness is fundamentally mis-designed: `dfsctl bench run` confuses CI-gate and user-facing roles, has no per-op timeout (a server hang wedges the client into D-state and loses results), and shares implicit state between runs. Three-layer rebuild on branch `v1.0/bench-refactor`:
   - **bench primitives** (`pkg/bench/workloads/`) — pure I/O workload generators (seq-write, rand-write, metadata, …). Every syscall wrapped in `context.WithTimeout`. No filesystem assumptions.
   - **bench runner** (`pkg/bench/runner/`) — composes workloads, manages bench-env lifecycle (mount/fixture/teardown). Emits structured events. Targets any directory. Hard `--budget` wall-clock kill. Health checks (stat + readdir + small round-trip) between phases — failure aborts with structured error, skips remaining phases, still emits partial JSON. Explicit `--env-dir` scratch dir; refuses to reuse an existing scratch dir from a previous run.
   - **bench orchestrator** (`bench/orchestrator/`, Go binary replacing `scripts/run-bench.sh` + `bench/scripts/run-all.sh`) — provisions infra, drives runner over SSH, collects results. Reads `privateNetworkID` from Pulumi outputs and uses private-network IP for mounts (public IP for SSH only). `dfs` binary injected via scp; the `go build` path in `dittofs-badger-s3.sh` is deleted (B4 hardcoded-`main`-branch footgun).
   - **Versioned result schema** — `{schema_version, run_id, system, git_sha, outcome ∈ {completed,partial,aborted}, abort_reason, workloads: {name: {outcome, throughput_mbps, latency_p50/95/99, ops: {total,succeeded,failed}, errors: [{op, offset, error_kind, count}]}}}`. CI gates assert `outcome == "completed" && len(errors) == 0`. Audit diff tools compare structured fields.
   - **`dfsctl bench` becomes a thin CLI over the runner** — same code path drives CI, user-facing benchmarks, and audit baselines.
   - **Migration** — one PR per stage: skeleton + one workload e2e; port `dfsctl bench` to new runner; port `scripts/run-bench.sh` → Go orchestrator; delete bash. New harness runs alongside old until parity proven.
   - **Docs consolidation** — 3 docs → 1 (`docs/BENCHMARKS.md` canonical, merge `test/e2e/BENCHMARKS.md`). Add `bench/README.md`. Wire micro-bench gates (D-40/41/42) into CI under the new schema.

   The Wave 0 baseline (`.planning/v1.0-audit/_baseline/`) documents 8 specific failure modes (B1–B8) the new harness must handle. Original entry was 1 line: "3 docs → 1 + wire D-40/41/42 into CI" — way too narrow given what the baseline run uncovered.
4. **Discarded-error sampling** — random sample of 50 `_ =` sites, categorize, fix HIGH-risk discards. Branch `v1.0/err-discard-audit`.
5. **Perf-optimization stream** *(NEW)* — cross-area hot-path wins surfaced by pprof that don't fit inside a single area PR-B: object pooling retrofit, streaming I/O conversions (per [[feedback_streaming_io_for_data_paths]]), allocation reduction in protocol decoders, lock-contention fixes. Driven by the aggregated Bottlenecks sections from Wave 1 REVIEW.md files. Branch `v1.0/perf-fixes`, may produce multiple sub-PRs.
6. **Security verification** *(NEW)* — once Wave 1 finishes, invoke `gsd-secure-phase` and `security-review` skills across the touched code. Branch `v1.0/security-verify`.

## Wave 3 — Docs & CLAUDE.md

Final stream. Branch `v1.0/docs-rewrite`.

1. **README.md** — keep "how to use" framing. Cut install methods to Nix + Homebrew + curl. Move long Docker/K8s sections to `docs/`. Target 781 → ~400 lines.
2. **docs/** — keep ARCHITECTURE, SMB, NFS, CONFIGURATION, IMPLEMENTING_STORES, SECURITY, FAQ, CONTRIBUTING, TROUBLESHOOTING, WINDOWS_TESTING, ACLS, RELEASING, BENCHMARKS, ENCRYPTION, CLI. Rewrite each pass: behavior-first, no planning refs, no AI fluff, link from README.
3. **CLAUDE.md** — keep invariants + commands + commit rules. Move SMB/NFS interop debug playbook → `docs/DEBUGGING.md` for public use. Trim to ~80 lines.
4. **Auto-gen `docs/CLI.md`** from Cobra if practical.

## Final — v1.0 tag

1. Bump version, write CHANGELOG.md.
2. Re-run full suite: `go test -race ./...`, e2e against all configs, smbtorture + WPTS BVT + pjdfstest, macro-bench on Scaleway, K8s operator deploy on Kapsule.
3. Confirm KNOWN_FAILURES.md is up to date.
4. Tag from main per [[feedback_commit_on_develop_not_main]].

## Deliverables structure

```
.planning/v1.0-audit/
  README.md                  # index of all area audits + triage status
  wave0-mechanical-cleanup/REVIEW.md
  smb/REVIEW.md
  nfs/REVIEW.md
  metadata/REVIEW.md
  locks-acl/REVIEW.md
  blockstore/REVIEW.md
  runtime/REVIEW.md
  syncer/REVIEW.md
  gc/REVIEW.md
  backup-snapshot/REVIEW.md
  config/REVIEW.md
  cli/REVIEW.md
  operator/REVIEW.md
  e2e-stabilize/REVIEW.md
  test-coverage/REVIEW.md
  bench-refactor/REVIEW.md
  err-discard-audit/REVIEW.md
  docs-rewrite/REVIEW.md
```

`planning-archive` orphan branch holds historical `.planning/` content (pushed but not merged).

## Verification end-to-end

- `go build ./... && go vet ./...` clean.
- `go test -race ./...` clean (no `KNOWN_FAILURES` regressions).
- `staticcheck ./...` clean (add to CI if missing).
- `go list -deps ./...` shows no import cycles. (`pkg/` ↔ `internal/` rule depends on (a)/(b)/(c) decision above — if (a), enforce no `pkg/` → `internal/` edges.)
- Public-API surface audit: every exported `pkg/` symbol has at least one external caller. Tooling — `staticcheck` `U1000` (in-package unused) + manual `grep`-based external-callsite check across the repo. (Note: `unused` does **not** flag exported symbols by zero external callers; `unparam` is not enabled in `.golangci.yml` — wire it in if we want unused-parameter coverage as a separate CI gate.)
- `cd test/e2e && sudo ./run-e2e.sh` green for all configs including `--s3`.
- `test/smb-conformance` no new failures.
- `test/posix` pjdfstest baseline holds.
- K8s operator: `kubectl apply` on `dittofs-demo` cluster, `DittoServer` reconciles to Ready.
- Macro-bench on Scaleway: no perf regression > 5% vs v0.16 baseline.
- No `Phase \d+|D-\d{2}|BSCAS-...` matches in `*.go` (`grep -rE` returns 0).
- No `Co-Authored-By: Claude` in commits authored on `v1.0/*` branches.
- README + docs/ rendered preview clean.

## Order recap

```
Wave 0 (1 PR + baseline profiles)
    ↓
Wave 1 area-pairs in hot-path-first order (perf leverage):
    blockstore → syncer → smb → nfs → lock/acl → metadata → runtime → gc → snapshot → config → cli → operator
    (each area: PR-A audit → triage → PR-B fix + simplifier + reviewer + verify; with per-area pprof delta)
    ↓
Wave 2 streams in parallel with Wave 1 (e2e-stabilize, test-coverage, bench-refactor, err-discard, perf-fixes, security-verify)
    ↓
Wave 3 (docs + CLAUDE.md rewrite)
    ↓
v1.0 tag
```

Estimated 5–9 weeks. Each Wave 1 area-pair ~3–5 days (was 2–4; +1 for pprof + post-fix simplifier/reviewer pass).
