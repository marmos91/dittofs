// Package storebackups provides scheduled backup execution for registered
// store-backup repos. In v0.13.0 the target is metadata stores
// (D-25); block-store backup is additive future work.
//
// Plan 04 contribution: retention pass (retention.go) implementing
// D-08..D-17 (union policy, pinned skip, safety rail, destination-first
// delete, continue-on-error, non-degrading parent job status, 30-day
// BackupJob pruner).
//
// Plan 05 will add service.go composing scheduler + executor + retention
// into the 9th pkg/controlplane/runtime sub-service.
package storebackups
