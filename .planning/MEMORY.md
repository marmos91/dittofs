---
allowed-tools: Bash(graphify *), Bash(rg *), Read, Grep, Glob
description: DittoFS program memory — index of live planning docs (update after each wave)
---

# DittoFS program memory

Last updated: 2026-09-22. This file is an **index**, not a record: per-wave closure notes,
lane prompts, triage tables and shipped step plans live in git history, not here.
`git log --diff-filter=D -- .planning/` finds a deleted one.

## Live planning docs

| Doc | Covers | State |
| --- | --- | --- |
| `2026-09-09-adapter-convergence-ROADMAP.md` | adapter convergence, waves 0-7 | **the live index** — refreshed 2026-09-21 |
| `2026-09-08-adapter-convergence-MASTER-PLAN.md` | same program, design depth | its 2026-09-14 correction is operative |
| `2026-09-21-wave7-perf-lens-PLAN.md` | Wave 7 perf lens | plan, not started |
| `2026-09-21-smb-boundary-cut-measure.sh` | cut-cost measurement for the SMB boundary split | re-runnable tool |
| `2026-09-01-block-dataflow-MASTER-PLAN.md` | block module refactor, steps 0-5 | **closed** 2026-09-21; kept because the successor plan extends its named §3d gaps |
| `2026-09-14-nfs-med-triage.md` | NFS MED tranches #2400-#2407 | #2401, #2402, #2407 still open |
| `2026-09-15-ci-throughput-and-merge-gating-PLAN.md` | PR wall-clock + merge gating | design doc, work ongoing |
| `2026-07-21-reliability-scale-features-ROADMAP.md` | top-level program order | standing |
| `CONVENTIONS-WAVE-TEST-NAMES.md` | wave/lane names never enter source | standing rule |
| `audits/`, `perf/` | audit findings and measured evidence | referenced by open triage; expensive to re-derive |

The successor to the block master plan is `2026-09-22-block-simplification-PLAN.md`, carried by
open PR #2828 on `docs/block-simplification-plan` — it lands in `.planning/` when that PR merges.

## Program state

Adapter convergence waves 0-5 are **complete** (see the roadmap for per-wave PR tables).
Wave 7 is the only live wave. Block refactor steps 0-5 are verified on develop.
The one wave-8 item that outlived its plan is **#2437** (NFSv4.0 has no trusted client
identity) — it is named in the roadmap's continuous queue.

## Standing hazards (carry into every prompt)

- Audits predate later fixes — re-verify each premise on develop before acting on a finding
- Postgres tests are `//go:build integration` — green `go test ./...` proves nothing about postgres
- Check `pg_stat_activity` before any dropdb (#2345)
- Merge via gh CLI: `PUT pulls/{n}/merge -f sha=<re-read head>`; sign every commit; rebase, never merge
- Assign every PR to marmos91; THREE reviews before open (correctness, simplification, adversarial);
  babysit Copilot + CI after open
- `graphify update .` after the last merge of the day, from the main checkout
