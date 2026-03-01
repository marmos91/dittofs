---
phase: 33-benchmark-infrastructure
plan: 01
subsystem: infra
tags: [docker, shell, fio, benchmarks, makefile]

# Dependency graph
requires: []
provides:
  - "bench/ directory scaffolding with docker/, configs/, scripts/, workloads/, analysis/"
  - "Shared shell library (common.sh): logging, timers, OS detection, Docker Compose helpers"
  - "Prerequisites checker validating docker, fio, jq, bc, make, curl, compose v2"
  - "Cleanup script for NFS/SMB unmount, container teardown, volume removal"
  - "Multi-stage Dockerfile producing Alpine image with dfs+dfsctl"
  - ".env.example with S3/Postgres/DittoFS/resource/benchmark defaults"
  - "Makefile with help, check, build, up, down, logs, ps, clean, bootstrap targets"
affects: [33-02, 34, 37]

# Tech tracking
tech-stack:
  added: [shellcheck, fio]
  patterns: [multi-stage-docker-build, sourced-shell-library, self-documenting-makefile]

key-files:
  created:
    - bench/.gitignore
    - bench/.env.example
    - bench/scripts/lib/common.sh
    - bench/docker/Dockerfile.dittofs
    - bench/scripts/check-prerequisites.sh
    - bench/scripts/clean-all.sh
    - bench/Makefile
    - bench/workloads/.gitkeep
    - bench/analysis/.gitkeep
  modified: []

key-decisions:
  - "ShellCheck shell=bash directive in common.sh (sourced library, no shebang)"
  - "SC2034 suppression for GREEN color variable exported to sourcing scripts"
  - "Force-added bench/.gitignore past root .gitignore exclusion of all .gitignore files"

patterns-established:
  - "Sourced library pattern: common.sh with shellcheck directives, no shebang, no set -euo pipefail"
  - "Script sourcing pattern: SCRIPT_DIR detection + source lib/common.sh with shellcheck source directive"
  - "Makefile self-documenting: grep/awk help target matching ## comments"

requirements-completed: [BENCH-01]

# Metrics
duration: 4min
completed: 2026-02-26
---

# Phase 33 Plan 01: Benchmark Infrastructure Scaffolding Summary

**bench/ directory with shared shell library, prerequisites checker, cleanup script, DittoFS Dockerfile, environment template, and self-documenting Makefile**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-26T15:35:25Z
- **Completed:** 2026-02-26T15:39:40Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Complete bench/ directory tree with all subdirectories for configs, docker, scripts, workloads, analysis, and results
- ShellCheck-clean shared library with logging, timers, OS detection, Docker Compose validation, and health polling
- Multi-stage Dockerfile producing Alpine image with dfs+dfsctl, healthcheck, non-root user, exposing NFS/API/pprof ports
- Prerequisites checker validating 8 required tools with install hints, plus 3 optional tools
- Cleanup script handling NFS/SMB unmount, container teardown, volume removal, and optional Docker prune

## Task Commits

Each task was committed atomically:

1. **Task 1: Create directory structure, .gitignore, .env.example, and common.sh** - `aa5368f7` (feat)
2. **Task 2: Create Dockerfile.dittofs, check-prerequisites.sh, clean-all.sh, and Makefile** - `59292d1c` (feat)

## Files Created/Modified
- `bench/.gitignore` - Excludes results/, .env, *.pyc, __pycache__/
- `bench/.env.example` - 7-section environment template with LocalStack/Postgres defaults
- `bench/scripts/lib/common.sh` - Shared library: log_info/warn/error, die, timer_start/stop, detect_os, require_docker_compose_v2, wait_healthy
- `bench/docker/Dockerfile.dittofs` - Multi-stage build: golang:1.25-alpine -> alpine:3.21, dfs+dfsctl, ports 12049/8080/6060
- `bench/scripts/check-prerequisites.sh` - Validates docker, fio, jq, bc, make, curl, compose v2, daemon status
- `bench/scripts/clean-all.sh` - Unmounts /mnt/bench* and /tmp/bench*, stops compose, removes volumes, optional prune
- `bench/Makefile` - Self-documenting targets: help, check, build, up, down, logs, ps, clean, bootstrap
- `bench/workloads/.gitkeep` - Placeholder for Phase 34 workload definitions
- `bench/analysis/.gitkeep` - Placeholder for Phase 37 analysis pipeline

## Decisions Made
- Added `# shellcheck shell=bash` directive to common.sh since it is a sourced library without a shebang
- Suppressed SC2034 for GREEN color variable (used by sourcing scripts, not directly in common.sh)
- Used `git add -f` for bench/.gitignore because root .gitignore excludes all .gitignore files globally

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed unused FOUND_COUNT variable in check-prerequisites.sh**
- **Found during:** Task 2 (check-prerequisites.sh)
- **Issue:** ShellCheck SC2034 flagged FOUND_COUNT as unused
- **Fix:** Removed the unused variable assignment
- **Files modified:** bench/scripts/check-prerequisites.sh
- **Verification:** ShellCheck passes at warning severity
- **Committed in:** 59292d1c (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Minor cleanup, no scope change.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- bench/ directory scaffolding is complete, ready for Plan 02 (Docker Compose + configs)
- All scripts source common.sh successfully
- Makefile targets ready for docker-compose.yml (Plan 02 deliverable)

## Self-Check: PASSED

All 9 created files verified present. Both task commits (aa5368f7, 59292d1c) verified in git history.

---
*Phase: 33-benchmark-infrastructure*
*Completed: 2026-02-26*
