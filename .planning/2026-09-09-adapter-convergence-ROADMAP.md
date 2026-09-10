# Adapter convergence — roadmap as of 2026-09-09 (Wave 2 CLOSED)

Source of truth: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` (waves, corrections
ledger) and `.planning/2026-09-08-adapter-convergence-TODO.md` (execution tracking). This file is
the point-in-time status + proposed order. If the two disagree, the plan wins.

Verified against GitHub on 2026-09-09 ~23:40: Wave 2 fully landed — PRs #2494/#2495/#2496/#2497/#2498
all squash-merged, issues #2471/#2487/#2482/#2464/#2490 auto-closed, `origin/develop` at `7e5cd2e40`,
worktrees and branches deleted, `graphify update .` re-run from the merged checkout (31,103 nodes).
Note: develop carries 2 local docs commits (23de25e56 + eda765c47) not yet pushed to origin/develop.

---

## Where the program stands

| Wave | Scope | State |
| --- | --- | --- |
| 0 — Coordination | peer takeover, worktree cleanup | ✅ done (only #2340's 32-row re-derivation left) |
| 1 — Ownership class | #2393–#2396, #2413, #2414 | ✅ **LANDED 2026-09-08** — 7 PRs, soundness-audited; follow-ups #2449, #2451, #2436 closed; doc rule merged (#2468) |
| 2 — `sm.mu` + pynfs | #2398 + singles | ✅ **LANDED 2026-09-09** — residual #2340 rows + #2329 confirmation tracked below |
| 3 — Shared-layer reuse | errmap, identity, lifecycle | ❌ not started — **NEXT** (premises verified, see Step 2) |
| 4 — SMB triage | ~155 untriaged findings | ❌ not started (plan moved it *before* the SMB fix waves) |
| 5 — God objects | `manager.go` 3,985, `Create()` 1,016… | ❌ blocked behind 1–4 |
| 6 — Dispatch-core finish line | acceptance test | lands inside 1/3/5 |
| 7 — Perf lens | bufpool, measurement plan | ❌ not started |

### Wave 2 — final state (all landed 2026-09-09)

| Issue | PR | Squash commit |
| --- | --- | --- |
| #2471 CREATE_SESSION channel sizes | #2494 | `c5dd09bc2` |
| #2482 v4.0/v4.1 client-index fold | #2496 | `a4f479f10` |
| #2487 reclaim-persist retry + off-sm.mu write | #2495 | `8c4895714` |
| #2464 pynfs verified-lease guard + sync watcher refresh | #2497 | `80331c4ea` |
| #2490 LOCK special-stateid BAD_STATEID | #2498 | `7e5cd2e40` |

Earlier Wave 2 singles: #2398 via #2459+#2466, #2362 via #2458, #2369 via #2460, #2382 via #2456,
# 2399 via #2457, #2341 via #2484+#2481, #2340 partial via #2491/#2489/#2486/#2485.

**Wave 2 residual (carries into Wave 3+):** #2340's remaining pynfs v4.1 rows (walk out of
KNOWN_FAILURES only on demonstrated CI passes), #2329's pynfs delegation confirmation (server side
landed; residual is pynfs harness-side), and the 32-row re-derivation from Wave 0.

---

## Proposed order (next sessions)

### Step 1 — ~~Close Wave 2~~ ✅ DONE 2026-09-09

All five PRs merged and assigned marmos91; #2490/#2482/#2487/#2471/#2464 auto-closed. #2329's
pynfs delegation confirmation remains open as harness-side verification (no DittoFS change
warranted — verdict in `.planning/2026-09-09-diag-2329-2464.md`).

### Step 2 — Wave 3: shared-layer reuse + lifecycle (goals 2 + 4) — IMPLEMENTED, review passes running

All six branches pushed 2026-09-10 after the fan-out (5 workers, plan-validation corrections baked
in; full state in `.planning/2026-09-09-wave3-plan.md`). 12 read-only reviewers fanned out
(correctness + simplification + adversarial per branch); next: PR opens (guard first), serialized
merges, Copilot babysitting.

| Branch | Content | Head |
| --- | --- | --- |
| fix/errmap-coverage-dead-columns | COMBINED GUARD: enum walk + ErrConflict row + exoticCodes pin + delete MapLockToNFS3/4 (L0+L2 merged) | 98769af27 |
| fix/v4-content-errmap-wiring | 5 sites (read/read_plus/write/commit/deallocate) → MapContentToNFS4, ErrStoreClosed→STALE end to end | 27c86db9a |
| fix/smb-resolved-identity | getUserIdentity → uidGIDFromSessionUser pure rename (Option B: ResolvedIdentity-consumption deferred to follow-up) | 0bd237197 |
| fix/adapter-lifecycle-accept | Stop-path listenerReady close (double-close-guarded) + accept backoff 10ms→1s; bind-failure close killed (awaitListener two-channel select) | 60caff603 |
| fix/async-credit-grants | 2 async grant sites → GrantCredits + SequenceWindow.Grant + nil-SessionManager floor guard | 3bf0a5fd4 |
| fix/export-acl-citations | CheckExportAccess → ResolveSharePermission/Tree Connect ACL renames (CLAUDE.md + create.go) | e68dbb49b |

### Step 3 — Wave 4: SMB triage (before the SMB fix waves)

- File an SMB umbrella (#2407 mirror) + area tranches for the ~155 untriaged findings
- Quote the audit's *diagnosis*, re-derive the *fix* (`Verified: CONFIRMED` covers only the diagnosis)
- Triage "duplicate/boilerplate" findings by diffing — the Kerberos AP-REP copies have already drifted
- Credit/sequencing: the plan's `smb/state/` + `handlers/create/` + `handlers/info/` split is Wave 5;
  triage decides what joins it

### Step 4 — Wave 5: god objects, one package per move-only PR

`manager.go` 3,985 → `setFileInfoFromStore` 1,538 → `Create()` 1,016 → `open.go` 1,103 →
`completeCreateAfterBreak` 853 → `Handler` 56 fields. Apparatus is load-bearing (119 test files /
42,810 LOC in `smb/handlers`): move-only, `git diff -M --color-moved` zero body edits, one package
per PR merged same day, `graphify update .` after each, CI guard requiring a `_test.go` per new
package. Every extracted function gets its own test (no 150-LOC ceiling).

### Step 5 — Wave 7: perf lens (goal 3)

No measurement plan exists. Build it before touching anything: connection ramp both protocols,
v4.1 >64 concurrent ops per session, large-I/O allocation rate toward 16MB. Then `bufpool` tier
sizing (large tier `1<<20` == advertised max I/O; every max-size READ/WRITE falls off the pool).
Issue 2423 (adaptive controller samples the wrong window) belongs here with #2398's axis.

### Continuously (cross-cutting, slot into any lull)

- `auxsvc` lock fix (`Group.Start` holds `g.mu` across `s.Start()`); rename → `sidecar`
- Conformance CI gate: non-empty Reason+Issue per known-failure row (`kf_load`); knfsd/Samba
  comparison per SMB suite; #2322 suite unification
- Instrumentation: per-op RED inside the NFSv4 COMPOUND loop; pprof on `pkg/metrics/server.go`
- Hygiene sweeps: 962 banner separators, signature-restating doc comments, phantom-symbol citations
- #2437 (v4.0 has no trusted client identity) — the known ceiling on Wave 1's guards; needs a
  design decision, not a patch. Wave 1's stateid semantics have settled, so it can be scheduled.
- #2441 (`ErrCrossShare` code + errmap rows so SMB matches NFS's XDEV) — small, pairs with Step 2
- #2397 (GDD4_OK but no CB_NOTIFY) and #2442 (SEEK ISDIR for non-regular files) — protocol gaps
- #2483 (seqid=0 v4.1 bypass applied to v4.0) — the last Wave-2 stateid item; touches all seqid
  ops in `v4/state`, one writer after Step 2 settles

Not this quarter: #2423 beyond the measurement, #2353 (RAG substrate), #2324, #2320, #2322 (beyond
the gate), #2345, #2258, #2215, #2263, #117, and the pre-audit backlog (issues 1417, 1418,
1454, 1489, 1290, 1199, 1603).

---

## Standing hazards (unchanged, carry into every prompt)

- Audits pinned at `40884ad4f`, predating #2356 — re-verify premises on develop
- Postgres tests are `//go:build integration`; a green `go test ./...` proves nothing about postgres
- #2345: DSN is a boolean gate; check `pg_stat_activity` before any dropdb — sessions may be live
- Count `gh pr checks`, not failures; a stacked PR runs zero checks
- Merge via `PUT pulls/{n}/merge -f sha=<re-read head>`; sign every commit; rebase, never merge
- Protocol/e2e suites are exclusive — brokered, one agent at a time
- Never re-count the known-failures ledger files: read each file's own tally (178 = SMB 86 + NFS 92)
