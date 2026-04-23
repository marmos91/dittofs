---
phase: 08-pre-refactor-cleanup-a0
plan: 12
subsystem: build
tags: [build, go-mod, dependencies, otel-audit, v0.13.0-backup-removal, TD-03, PR-B]

# Dependency graph
requires:
  - phase: 08-pre-refactor-cleanup-a0
    provides: "Plans 08-06..08-11 removed pkg/backup, storebackups, REST/CLI/apiclient backup surfaces, e2e tests, and docs (PR-B commits 1-8 per D-30)."
provides:
  - "go.mod tidied — 2 direct deps pruned (backup-only consumers), 2 direct-to-indirect demotions."
  - "go.sum regenerated — 4 checksum lines removed (2 modules × h1+mod sums)."
  - "OTel-audit decision documented: OTel kept as indirect because AWS SDK transitively requires it; backup was not the sole consumer."
  - "PR-B ready to open — final commit of the 8-commit PR-B stack complete."
affects: [v0.15.0-pr-b]

# Tech tracking
tech-stack:
  added: []
  removed:
    - "github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.20.0 (direct → removed — exclusively backup-consumed)"
    - "github.com/robfig/cron/v3 v3.0.1 (direct → removed — backup scheduler only)"
  demoted:
    - "github.com/aws/smithy-go v1.23.2 (direct → indirect)"
    - "go.opentelemetry.io/otel/trace v1.37.0 (direct → indirect)"
  patterns:
    - "`go mod tidy` post-deletion dependency hygiene — confirm direct-use grep across `.go` files, then run tidy, then diff-audit go.mod."

key-files:
  created:
    - ".planning/phases/08-pre-refactor-cleanup-a0/08-12-SUMMARY.md"
  modified:
    - "go.mod"
    - "go.sum"
  deleted: []

key-decisions:
  - "**OTel retained (indirect).** Audit (`grep -rn 'go.opentelemetry.io/otel' . --include='*.go'`) returned 0 non-vendor matches, confirming no DittoFS source uses OTel directly. However, `go mod tidy` did NOT remove `go.opentelemetry.io/otel*` from go.mod — AWS SDK v2 transitively pulls them in. Conclusion: backup was not the sole consumer; OTel stays as indirect deps. This matches D-28 guidance exactly ('if backup is the only consumer, drop the dep; otherwise keep')."
  - "**Two direct deps dropped by tidy** — both exclusively backup-consumed: `feature/s3/manager` (backup destination uploads) and `robfig/cron/v3` (backup scheduler). Grep confirmed zero non-vendor source references after the backup removal."
  - "**Two direct-to-indirect demotions** — `smithy-go` and `otel/trace` were previously listed as direct (no `// indirect`) but after backup removal are only used transitively. Tidy correctly re-classified them."
  - "**Idempotence verified** — second `go mod tidy` run produced zero additional diff (confirmed via md5 + diff against post-first-run baseline)."

patterns-established:
  - "Post-deletion dep hygiene workflow: grep → tidy → diff → build → vet → test → commit."

requirements-completed: [TD-03]

# Metrics
duration: 3min
completed: 2026-04-23
---

# Phase 08 Plan 12: go mod tidy + OTel audit Summary

**`go mod tidy` after v0.13.0 backup removal pruned two orphaned direct deps (`aws-sdk-go-v2/feature/s3/manager`, `robfig/cron/v3`) and demoted two more to indirect; OTel retained (AWS SDK transitively requires it — backup was not sole consumer).**

## Performance

- **Duration:** ~3 min
- **Started:** 2026-04-23T18:23:01Z
- **Completed:** 2026-04-23T18:26:00Z
- **Tasks:** 1 (+ audit + verification)
- **Files modified:** 2 (go.mod, go.sum)
- **Commit:** `c2ca7bde` (signed)

## Tasks Completed

### Task 1: go mod tidy + OTel audit (D-27, D-28, D-30 step 7)

**Commit:** `c2ca7bde` — `build: go mod tidy after v0.13.0 backup removal (TD-03)`

**Step 1 — OTel audit:**
```bash
grep -rn "go.opentelemetry.io/otel" . --include='*.go' | grep -v "^./vendor/"
```
→ **0 matches** in source files. No `vendor/` directory exists. This confirms the source-level audit side: no DittoFS code directly imports OTel.

**Step 2 — `go mod tidy -v`:**
Output:
```
unused github.com/aws/aws-sdk-go-v2/feature/s3/manager
unused github.com/robfig/cron/v3
```

Two direct deps flagged unused and removed:
- `github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.20.0` — backup destination S3 uploader
- `github.com/robfig/cron/v3 v3.0.1` — backup scheduler cron parser

grep confirmation (both returned 0 non-vendor matches):
```bash
grep -rn "aws-sdk-go-v2/feature/s3/manager" . --include='*.go' | grep -v "^./vendor/"  → 0 matches
grep -rn "robfig/cron" . --include='*.go' | grep -v "^./vendor/"                        → 0 matches
```

**go.mod diff (direct-vs-indirect re-classifications):**
```diff
- github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.20.0
  (deleted)
- github.com/robfig/cron/v3 v3.0.1
  (deleted)
- github.com/aws/smithy-go v1.23.2
+ github.com/aws/smithy-go v1.23.2 // indirect
- go.opentelemetry.io/otel/trace v1.37.0
+ go.opentelemetry.io/otel/trace v1.37.0 // indirect
```

**OTel decision:** `go mod tidy` did NOT remove `go.opentelemetry.io/otel`, `.../otel/metric`, or `.../otel/trace`. These are still pulled in transitively by AWS SDK v2 (e.g., `otelhttp` instrumentation in the HTTP transport stack). Therefore — per D-28 — they REMAIN in `go.mod` as indirect requirements. Backup was NOT the sole OTel consumer.

**go.sum diff:** 4 lines removed (2 modules × `h1:` + `/go.mod` checksums each). All removals, zero additions.

**Step 3 — Verification:**

| Check | Command | Result |
|-------|---------|--------|
| Tidy idempotence | second `go mod tidy` produces no diff | PASS (md5 unchanged) |
| Build | `go build ./...` | PASS (exit 0) |
| Vet | `go vet ./...` | PASS (exit 0) |
| Tests | `go test -count=1 -short -race ./...` | PASS (79 `ok`, 0 `FAIL`, exit 0) |
| Commit signed | `git log -1 --show-signature` | `Good "git" signature for m.marmos@gmail.com` |
| No forbidden strings | `grep -iE 'claude code\|co-authored-by'` | PASS (no matches) |
| No file deletions | `git diff --diff-filter=D HEAD~1 HEAD` | PASS (empty — only modifications) |

**Step 4 — Commit:**
```
build: go mod tidy after v0.13.0 backup removal (TD-03)

Prune direct deps orphaned by PR-B backup removal:
- github.com/aws/aws-sdk-go-v2/feature/s3/manager (backup destination only)
- github.com/robfig/cron/v3 (backup scheduler only)

Demote to indirect (still transitively needed):
- github.com/aws/smithy-go
- go.opentelemetry.io/otel/trace

OTel deps (otel, otel/metric, otel/trace) kept as indirect — AWS SDK
transitively depends on them; backup was not the sole OTel consumer.
```

## Deviations from Plan

None — plan executed exactly as written.

The only notable runtime observation was that `go mod tidy` flagged `aws-sdk-go-v2/feature/s3/manager` as unused. The plan text focused on the OTel audit, but D-27 already called out "prune orphaned direct deps" generally — so the removal of `s3/manager` and `robfig/cron/v3` fits D-27 verbatim. The plan-predicted outcome for OTel (keep-or-drop depending on audit) resolved to **keep** because AWS SDK transitively requires it.

## Auth Gates

None.

## Threat Flags

None. Dependency removal reduces attack surface (pruning unused modules). Remaining direct deps (`aws-sdk-go-v2`, `aws/smithy-go` as indirect) are unchanged from pre-phase state in terms of their own usage — just re-classified. No new network endpoints, auth paths, file access patterns, or trust-boundary schema changes.

## TDD Gate Compliance

Not applicable — plan type is `execute` (not `tdd`), and the work is dependency housekeeping rather than behavior change.

## Known Stubs

None. This plan modifies build metadata only — no rendering paths, UI, or feature stubs introduced or left behind.

## Deferred Issues

None.

## Success Criteria

- [x] `go mod tidy` run; go.mod + go.sum changes committed (`c2ca7bde`)
- [x] Orphaned direct deps audited; removed where confirmed orphan (2 removed, 2 demoted to indirect)
- [x] `go build ./...` passes (exit 0)
- [x] `go vet ./...` passes (exit 0)
- [x] `go test -count=1 -short -race ./...` passes (79 `ok`, 0 `FAIL`)
- [x] SUMMARY.md created
- [x] Commit signed with `git commit -S --no-verify`
- [x] No STATE.md / ROADMAP.md modifications

## Self-Check

Verifying claims:

- `go.mod` modified at HEAD (`git show --stat HEAD | grep go.mod`): FOUND
- `go.sum` modified at HEAD (`git show --stat HEAD | grep go.sum`): FOUND
- Commit `c2ca7bde` exists (`git log --oneline | grep c2ca7bde`): FOUND
- Commit signature verified (`git log -1 --show-signature`): FOUND (Good signature, m.marmos@gmail.com)
- No Claude Code / Co-Authored-By strings in commit message: VERIFIED
- `go.opentelemetry.io/otel` remains in go.mod as indirect: VERIFIED (not removed by tidy because AWS SDK transitively requires it)
- `aws-sdk-go-v2/feature/s3/manager` absent from go.mod: VERIFIED (tidy removed it)
- `robfig/cron/v3` absent from go.mod: VERIFIED (tidy removed it)

## Self-Check: PASSED

## Next Phase Handoff

**PR-B is complete.** Commits 1-8 of the PR-B stack (D-30 steps 1-7, with step-6 e2e removal at start, then reverse-import-order backup deletion, ending with this tidy commit) are all landed.

PR-C (TD-01 merge + TD-03 remnants + TD-04 parser collapse) begins at plan 08-13 and operates on a clean dependency graph with no backup-specific transitive pulls.
