# Adapter convergence — roadmap as of 2026-09-09

Source of truth: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` (waves, corrections
ledger) and `.planning/2026-09-08-adapter-convergence-TODO.md` (execution tracking). This file is
the point-in-time status + proposed order. If the two disagree, the plan wins.

Verified against GitHub on 2026-09-09: 40 open issues, 0 open PRs, `origin/develop` at
`71d56bc37` (post-#2493). Issue states below re-checked live, not carried from the TODO file.

---

## Where the program stands

| Wave | Scope | State |
| --- | --- | --- |
| 0 — Coordination | peer takeover, worktree cleanup | ✅ done (only #2340's 32-row re-derivation left) |
| 1 — Ownership class | #2393–#2396, #2413, #2414 | ✅ **LANDED 2026-09-08** — 7 PRs, soundness-audited; follow-ups #2449, #2451, #2436 closed; doc rule merged (#2468) |
| 2 — `sm.mu` + pynfs | #2398 + singles | ~90% landed — see below |
| 3 — Shared-layer reuse | errmap, identity, lifecycle | ❌ not started |
| 4 — SMB triage | ~155 untriaged findings | ❌ not started (plan moved it *before* the SMB fix waves) |
| 5 — God objects | `manager.go` 3,985, `Create()` 1,016… | ❌ blocked behind 1–4 |
| 6 — Dispatch-core finish line | acceptance test | lands inside 1/3/5 |
| 7 — Perf lens | bufpool, measurement plan | ❌ not started |

### Wave 2 — what actually landed (the TODO file is stale here)

| Issue | State | Landed via |
| --- | --- | --- |
| #2398 `sm.mu` serialization | CLOSED 2026-09-08 | #2459 + #2466 |
| #2362 FATTR4 numbers | CLOSED | #2458 |
| #2369 SECINFO flavor list | CLOSED | #2460 |
| #2382 change-attr freeze | CLOSED | #2456 (documented, per decision) |
| #2399 CLAIM_DELEGATE_CUR | CLOSED | #2457 |
| #2341 14 v4.0 rows | CLOSED 2026-09-08 | #2484, #2481 |
| #2340 32 v4.1 rows | **OPEN** — partially | #2491 (EXCHANGE_ID), #2489 (DESTROY_CLIENTID), #2486 (RECLAIM_COMPLETE), #2485 (boot epoch) |
| #2329 v4.1 delegations | **OPEN** — but the root cause landed | #2476 (client-record unify) + #2479 (grant on verified callback path) |
| #2389, #2359 singles | CLOSED 2026-09-08/09 | — |

Fresh issues opened against the landed code (post-merge finding churn — expected): 2490, 2487, 2483, 2482, 2471, 2467, 2464.

**Wave 2 residual:** #2340's remaining rows, #2329's pynfs confirmation, #2482/#2483/#2487/#2490/#2471/#2467
(stateid semantics + CREATE_SESSION sizes), plus the 32-row re-derivation from Wave 0.

---

## Proposed order (next sessions)

### Step 1 — Close Wave 2: NFSv4.0/4.1 stateid correctness (goal 1)

One cluster, one lens: special-stateid answers, seqid bypass, CREATE_SESSION reply sizes,
client-index fold.

- #2490 LOCK on `exist_lock_owner4` answers STALE where BAD is required
- #2483 seqid=0 v4.1 bypass applied to v4.0 (LOCKU with bad lockseqid succeeds)
- #2482 fold v4.0/v4.1 client indexes into one map with version filters (companion to #2476)
- #2487 failed reclaim-complete persist never retried (grace re-waits)
- #2471 CREATE_SESSION negotiated reply sizes unenforced, NFS4ERR_RESOURCE where invalid
- #2340 remaining v4.1 rows; #2329 confirm pynfs now gets a delegation (#2479 landed)
- #2464 COUR6 postgres-s3 flake (courtesy-client reaping vs lease_time) — diagnose, don't mask

Order within: #2482 first (index fold unblocks #2490/#2483 fixes); #2464 last (flake, needs
postgres-s3 run). All touch `state/manager.go` / `v4/state` — **one writer**, sequential PRs, no
stacking (Wave-1 trap: a stacked PR runs zero checks).

### Step 2 — Wave 3: shared-layer reuse + lifecycle (goals 2 + 4)

Small, mostly independent PRs; good for a parallel fan-out (disjoint files):

1. Wire all five bypassing content paths to the shared error mapper; restore `ErrStoreClosed → STALE`
2. `TestErrorMapCoverage` enum rewrite (5-line PR, unguards everything else in this wave) — land FIRST
3. Delete dead `MapLockToNFS3/4`; fix `ErrLockLimitExceeded` Jukebox-vs-IO; add `ErrConflict` row
4. Delete `getUserIdentity` (SMB uses the `ResolvedIdentity` it already has); scope to resolved identities
5. Listener stop-before-bind leak + EMFILE/ENFILE accept busy-loop (`pkg/adapter/base.go:246-263`)
6. `CheckExportAccess` stale-citation fix in `CLAUDE.md:105` + `create.go:1874`
7. Delete `grantAdaptive` dead branch

Guards from the plan: a dead function does not make its file dead (check `init()`-filled tables
before deleting); audits predate #2356 — re-verify each premise on develop.

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
  design decision, not a patch. Schedule it after Step 1 settles stateid semantics.
- #2441 (`ErrCrossShare` code + errmap rows so SMB matches NFS's XDEV) — small, pairs with Step 2
- #2397 (GDD4_OK but no CB_NOTIFY) and #2442 (SEEK ISDIR for non-regular files) — protocol gaps,
  slot alongside Step 1

Not this quarter: #2423 beyond the measurement, #2353 (RAG substrate), #2324, #2320, #2322 (beyond
the gate), #2345, #2258, #2215, #2263, #117, and the pre-audit backwalog (issues 1417, 1418,
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
