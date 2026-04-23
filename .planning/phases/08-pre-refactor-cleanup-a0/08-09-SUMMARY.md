---
phase: 08-pre-refactor-cleanup-a0
plan: 09
subsystem: controlplane/runtime
tags: [backup-removal, td-03, d-30]
status: complete
requires: [08-08b]
provides: []
affects: [pkg/controlplane/runtime/storebackups]
tech-stack:
  added: []
  removed: [storebackups package, OTel backup spans, backup_hold, orphan_sweep, retention, restore, service, target]
  patterns: []
key-files:
  created: []
  modified: []
  deleted:
    - pkg/controlplane/runtime/storebackups/backup_hold.go
    - pkg/controlplane/runtime/storebackups/backup_hold_test.go
    - pkg/controlplane/runtime/storebackups/doc.go
    - pkg/controlplane/runtime/storebackups/errors.go
    - pkg/controlplane/runtime/storebackups/metrics.go
    - pkg/controlplane/runtime/storebackups/metrics_test.go
    - pkg/controlplane/runtime/storebackups/orphan_sweep.go
    - pkg/controlplane/runtime/storebackups/restore.go
    - pkg/controlplane/runtime/storebackups/restore_test.go
    - pkg/controlplane/runtime/storebackups/retention.go
    - pkg/controlplane/runtime/storebackups/retention_test.go
    - pkg/controlplane/runtime/storebackups/service.go
    - pkg/controlplane/runtime/storebackups/service_test.go
    - pkg/controlplane/runtime/storebackups/target.go
    - pkg/controlplane/runtime/storebackups/target_test.go
decisions:
  - "Collapsed into single atomic commit with 08-08a/08-08b/08-10 due to import cycle."
metrics:
  completed: 2026-04-23
---

# Phase 08 Plan 09: storebackups Package Tree Removal Summary

Deleted the entire `pkg/controlplane/runtime/storebackups/` directory tree. This eliminates the Runtime-layer backup orchestration (service, retention, restore, orphan sweep, backup hold, metrics, target). Part of the v0.13.0 backup deletion (D-30 step 6).

## One-liner

`pkg/controlplane/runtime/storebackups/` package tree deleted as part of atomic PR-B collapse.

## Commit

- **SHA:** `7308eb92f4c63446d9de28acb5e669a188066d87`
- **Subject:** `refactor: remove v0.13.0 backup system (D-30 steps 4-7 combined, TD-03)`
- **Signed:** yes

This plan was collapsed into the atomic commit alongside 08-08a, 08-08b, and 08-10 — see 08-08a-SUMMARY.md for the cycle explanation.

## Deletions attributed to this plan

### Files deleted (15)

Full `pkg/controlplane/runtime/storebackups/` directory:

- `backup_hold.go` + `backup_hold_test.go`
- `doc.go`
- `errors.go`
- `metrics.go` + `metrics_test.go` (OTel backup span wiring)
- `orphan_sweep.go`
- `restore.go` + `restore_test.go`
- `retention.go` + `retention_test.go`
- `service.go` + `service_test.go`
- `target.go` + `target_test.go`

## Pre-check

`grep -rn "runtime/storebackups\"\\|storebackups\\." . --include='*.go'` (excluding the directory itself) -> 0 matches before commit.

## Verification

- `go build ./...` exits 0
- `go vet ./...` exits 0
- `go test -count=1 -short -race ./pkg/controlplane/... ./pkg/blockstore/...` exits 0
- `test ! -d pkg/controlplane/runtime/storebackups` passes
- `grep -rn "runtime/storebackups" . --include='*.go'` -> 0 matches
- `grep -rn "storebackups" pkg/ --include='*.go'` -> 0 matches

## Deviations from Plan

None beyond the plan-collapse rationale described above.

## Self-Check: PASSED

- Commit `7308eb92` exists
- Directory absent: confirmed
- Build/tests green
