// Package scheduler provides store-agnostic scheduler primitives for
// periodic backup runs: cron-based firing with CRON_TZ timezone support
// (via robfig/cron/v3), stable per-repo phase offset (FNV-1a jitter,
// D-03), per-repo overlap guard (D-07), and strict schedule validation
// (D-06).
//
// The package takes an abstract Target interface (ID, Schedule) instead
// of *models.BackupRepo so a future block-store-backup milestone can
// reuse the same primitives without refactor (D-24, D-25).
package scheduler
