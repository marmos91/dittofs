package models

import "time"

// Restore step markers. They record how far an in-progress RestoreSnapshot
// reached before a crash, for operator diagnostics. Rollback on startup is
// identical regardless of which step a crash interrupted (roll the share
// back to the safety snapshot), so the step value is informational only.
const (
	// RestoreStepStarted is written after the safety snapshot is verified
	// and durably persisted, immediately BEFORE the first destructive op
	// (block-store local-state reset). A crash with a marker at this step
	// means no destructive op may have run yet, or any of them may have
	// run partially.
	RestoreStepStarted string = "started"

	// RestoreStepLocalReset is recorded after ResetLocalState completes.
	RestoreStepLocalReset string = "local_reset"

	// RestoreStepMetaReset is recorded after the metadata store Reset
	// completes (its contents are now wiped; the dump replay has not yet
	// run or is partial).
	RestoreStepMetaReset string = "meta_reset"

	// RestoreStepRestored is recorded after the metadata dump replay
	// completes (post-verify may still be pending).
	RestoreStepRestored string = "restored"
)

// RestoreMarker is the durable record of an in-progress share restore.
// Exactly one marker exists per share while a restore is mid-flight: it is
// written after the safety snapshot is verified and BEFORE the first
// destructive step, and deleted only after the restore fully completes and
// post-verifies. A marker found at server startup means the restore was
// interrupted (crash, kill, power loss) and the share must be rolled back
// to SafetySnapshotID.
//
// The marker lives in the control-plane DB (the same durable store that
// holds snapshot rows) so it survives a crash and is consulted on the next
// boot by recoverInterruptedRestores.
type RestoreMarker struct {
	// ShareName is the primary key: at most one in-flight restore per share.
	ShareName string `gorm:"primaryKey;size:255" json:"share_name"`

	// TargetSnapshotID is the snapshot the interrupted restore was
	// restoring FROM. Informational for diagnostics — rollback restores
	// the safety snapshot, not this one.
	TargetSnapshotID string `gorm:"not null;size:36" json:"target_snapshot_id"`

	// SafetySnapshotID is the pre-restore safety snapshot to roll back to.
	// Always non-empty: the marker is only written after the safety snap is
	// verified.
	SafetySnapshotID string `gorm:"not null;size:36" json:"safety_snapshot_id"`

	// Step records the furthest restore step reached. Informational.
	Step string `gorm:"not null;size:20" json:"step"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (RestoreMarker) TableName() string {
	return "restore_markers"
}
