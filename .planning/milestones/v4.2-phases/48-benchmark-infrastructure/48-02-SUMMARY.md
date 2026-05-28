---
phase: 33-benchmark-infrastructure
plan: 02
subsystem: infra
tags: [docker-compose, nfs, smb, ganesha, kernel-nfs, rclone, juicefs, samba, benchmarks]

# Dependency graph
requires:
  - phase: 33-01
    provides: "bench/ scaffolding, common.sh, Dockerfile.dittofs, .env.example, Makefile"
provides:
  - "Docker Compose with 9 profiles for DittoFS (3 NFS + 1 SMB) and 5 competitors"
  - "3 minimal DittoFS config YAMLs (badger-s3, postgres-s3, badger-fs)"
  - "Parameterized bootstrap-dittofs.sh creating stores/shares/adapters per profile via dfsctl"
  - "NFS-Ganesha, RClone, kernel NFS, Samba config files"
  - "Comprehensive bench/README.md with quick start, pipeline, port map, troubleshooting"
affects: [34, 35, 36, 37, 38]

# Tech tracking
tech-stack:
  added: [localstack, nfs-ganesha, kernel-nfs, rclone, juicefs, samba]
  patterns: [docker-compose-profiles, parameterized-bootstrap, resource-capped-services]

key-files:
  created:
    - bench/docker-compose.yml
    - bench/configs/dittofs/badger-s3.yaml
    - bench/configs/dittofs/postgres-s3.yaml
    - bench/configs/dittofs/badger-fs.yaml
    - bench/scripts/bootstrap-dittofs.sh
    - bench/configs/ganesha/ganesha.conf
    - bench/configs/rclone/rclone.conf
    - bench/configs/kernel-nfs/exports
    - bench/configs/samba/smb.conf
    - bench/README.md
  modified: []

key-decisions:
  - "DittoFS config YAMLs intentionally identical (stores/shares differ only via bootstrap script)"
  - "JuiceFS uses S3 gateway mode (not NFS export) due to FUSE + NFS layering complexity"
  - "RClone healthcheck falls back to TCP check if rclone rc noop is unavailable"
  - "Samba healthcheck falls back to nc if smbclient fails"
  - "DittoFS SMB profile reuses badger-s3.yaml config (adapter type determined by bootstrap)"

patterns-established:
  - "Profile-per-system: one docker compose profile per benchmarked system for isolation"
  - "Bootstrap-per-profile: single script handles all DittoFS backend combinations via case statement"
  - "Consistent /export path across all systems for uniform mount commands"

requirements-completed: [BENCH-01]

# Metrics
duration: 5min
completed: 2026-02-26
---

# Phase 33 Plan 02: Docker Compose, Configs, and README Summary

**Docker Compose with 9 profiles for 11 services (4 DittoFS + 5 competitors + 2 infra), competitor configs, parameterized bootstrap script, and comprehensive workflow README**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-26T15:43:12Z
- **Completed:** 2026-02-26T15:48:38Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- Docker Compose defining 11 services with 9 profiles, healthchecks, and resource limits on every service
- Unique port assignments for all systems (12049/22049/32049/42049/52049 NFS, 12445/22445 SMB)
- Parameterized bootstrap script handling 4 DittoFS profiles (badger-s3, postgres-s3, badger-fs, smb)
- Competitor configs ready to use: NFS-Ganesha, RClone, kernel NFS, Samba
- 274-line README with quick start, pipeline diagram, system table, port map, platform notes, troubleshooting

## Task Commits

Each task was committed atomically:

1. **Task 1: Docker Compose, DittoFS configs, and bootstrap script** - `d7837289` (feat)
2. **Task 2: Competitor configs and README** - `836bdfad` (feat)

## Files Created/Modified
- `bench/docker-compose.yml` - 341-line Docker Compose with all profiles, healthchecks, resource limits
- `bench/configs/dittofs/badger-s3.yaml` - Minimal DittoFS config (stores via bootstrap)
- `bench/configs/dittofs/postgres-s3.yaml` - Minimal DittoFS config (stores via bootstrap)
- `bench/configs/dittofs/badger-fs.yaml` - Minimal DittoFS config (stores via bootstrap)
- `bench/scripts/bootstrap-dittofs.sh` - Creates stores/shares/adapters per profile via dfsctl REST API
- `bench/configs/ganesha/ganesha.conf` - NFS-Ganesha FSAL_VFS export with NFSv3+v4
- `bench/configs/rclone/rclone.conf` - S3 remote pointing to LocalStack
- `bench/configs/kernel-nfs/exports` - Kernel NFS exports reference file
- `bench/configs/samba/smb.conf` - Samba [export] share with bench user
- `bench/README.md` - Complete workflow guide with 11 sections

## Decisions Made
- DittoFS config YAMLs are intentionally identical across all profiles; the only difference is what the bootstrap script creates (which stores, adapters) based on the profile argument
- JuiceFS uses `juicefs gateway` (S3 gateway mode) since exposing JuiceFS FUSE mount via NFS adds unfair overhead from layering two filesystems
- RClone and Samba healthchecks use dual-command fallback patterns for robustness
- DittoFS SMB profile reuses the badger-s3.yaml config; the bootstrap script creates an SMB adapter instead of NFS
- All S3/Postgres credentials come from environment variables, not hardcoded in configs

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All Docker Compose profiles validate successfully (`docker compose --profile <name> config`)
- Bootstrap script is ShellCheck-clean (only SC1091 info about source path)
- Ready for Phase 34 (fio workload definitions) and Phase 36 (orchestrator)
- `make up PROFILE=<name>` works for any profile once images are built

## Self-Check: PASSED

All 10 created files verified present. Both task commits (d7837289, 836bdfad) verified in git history.

---
*Phase: 33-benchmark-infrastructure*
*Completed: 2026-02-26*
