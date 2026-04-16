package storebackups

import "github.com/marmos91/dittofs/pkg/controlplane/models"

// Re-exports of Phase-4 sentinels for caller convenience. Callers may
// import either the models or the storebackups package; both identities
// match because these are variable aliases, not new errors.New values.
// This preserves errors.Is matching across the package boundary.
var (
	ErrScheduleInvalid      = models.ErrScheduleInvalid
	ErrRepoNotFound         = models.ErrRepoNotFound
	ErrBackupAlreadyRunning = models.ErrBackupAlreadyRunning
	ErrInvalidTargetKind    = models.ErrInvalidTargetKind
)
