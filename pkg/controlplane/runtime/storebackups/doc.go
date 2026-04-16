// Package storebackups provides scheduled backup execution for registered
// store-backup repos. In v0.13.0 the target is metadata stores
// (D-25); block-store backup is additive future work.
//
// This is the 9th runtime sub-service under pkg/controlplane/runtime/. It
// mirrors the adapters.Service precedent: explicit hot-reload API
// (RegisterRepo / UnregisterRepo / UpdateRepo — D-22), SAFETY-02
// interrupted-job recovery at boot (D-19), and a unified RunBackup
// entrypoint shared between the cron tick and Phase 6's on-demand API
// (D-23).
//
// Composition:
//   - pkg/backup/scheduler.Scheduler — cron firing + jitter (Plan 02)
//   - pkg/backup/scheduler.OverlapGuard — per-repo mutex shared between
//     cron and on-demand paths (D-07)
//   - pkg/backup/executor.Executor — single-attempt pipeline (Plan 03)
//   - RunRetention — inline retention pass under the same mutex (Plan 04,
//     SCHED-06)
//   - StoreResolver — service-layer FK replacement for polymorphic
//     (target_kind, target_id) — D-26
//
// Files:
//   - service.go — Service struct and lifecycle (Plan 05)
//   - target.go  — BackupRepoTarget + DefaultResolver (Plan 05)
//   - retention.go — RunRetention and D-17 job pruner (Plan 04)
//   - errors.go  — Re-exports of Phase-4 sentinels from models
package storebackups
