// Package executor orchestrates one backup attempt end-to-end.
//
// An Executor is called by pkg/controlplane/runtime/storebackups.Service
// from both cron-driven schedule ticks and Phase 6's on-demand POST
// /backups handler (D-23). It is store-agnostic: takes a Backupable
// source, a Destination, a BackupRepo row, and a narrow JobStore
// interface covering only the three persistence calls it needs.
//
// Sequence (D-21):
//  1. ulid.Make() → recordID
//  2. CreateBackupJob(status=running, started_at=now)
//  3. Build manifest.Manifest{BackupID=recordID, StoreID, Encryption, ...}
//  4. io.Pipe: source.Backup(ctx, w) || dst.PutBackup(ctx, &m, r)
//  5. On success:
//     CreateBackupRecord(id=recordID, sha256=m.SHA256, size=m.SizeBytes, status=succeeded)
//     UpdateBackupJob(status=succeeded, finished_at=now, backup_record_id=&recordID)
//  6. On failure: UpdateBackupJob(status=failed|interrupted, error=err.Error())
//     — no BackupRecord is created (D-16)
package executor
